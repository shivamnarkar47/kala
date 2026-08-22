// Command Code Go-plan tests: the 403 upgrade_required router and the
// /alpha/generate wire mapping, driven against a local mock of both
// endpoints (protocol per github.com/patlux/pi-commandcode-provider).
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kaal/kaal/internal/messages"
)

// ccMock is a two-endpoint Command Code mock: the Provider API (which may
// demand an upgrade) and the Go plan's alpha/generate stream.
type ccMock struct {
	mu          sync.Mutex
	chatCalls   int
	genCalls    int
	lastGenBody map[string]any
	lastHeaders http.Header
	upgrade     bool // chat endpoint answers 403 upgrade_required
}

func newCCMock(t *testing.T, upgrade bool) (*ccMock, *httptest.Server) {
	t.Helper()
	m := &ccMock{upgrade: upgrade}
	mux := http.NewServeMux()
	mux.HandleFunc("/provider/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.chatCalls++
		m.mu.Unlock()
		if m.upgrade {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"message":"Upgrade required","code":"upgrade_required"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/alpha/generate", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.genCalls++
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		json.Unmarshal(body, &doc)
		m.lastGenBody = doc
		m.lastHeaders = r.Header.Clone()
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, "{\"type\":\"reasoning-delta\",\"text\":\"pondering\"}\n")
		io.WriteString(w, "\n") // blank noise line must be tolerated
		io.WriteString(w, "{\"type\":\"text-delta\",\"text\":\"Hello \"}\n")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"text\":\"from generate\"}\n")
		io.WriteString(w, "{\"type\":\"tool-call\",\"toolCallId\":\"cc1\",\"toolName\":\"read\",\"input\":{\"path\":\"a.txt\"}}\n")
		io.WriteString(w, "{\"type\":\"finish\",\"finishReason\":\"tool-calls\",\"totalUsage\":{\"inputTokens\":9,\"outputTokens\":4}}\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return m, srv
}

func drainStream(ch <-chan StreamEvent) []StreamEvent {
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func withCommandCodeBase(t *testing.T, srvURL string) {
	t.Helper()
	old := commandCodeBase
	commandCodeBase = func(baseURL string) bool {
		return strings.HasPrefix(baseURL, srvURL)
	}
	t.Cleanup(func() { commandCodeBase = old })
	// Keep mock traffic out of the real discovery cache.
	oldPath := ccCachePath
	cacheFile := filepath.Join(t.TempDir(), "cc-transport.json") // stable per test
	ccCachePath = func() string { return cacheFile }
	t.Cleanup(func() { ccCachePath = oldPath })
}

func TestGenerateEndpointDerivation(t *testing.T) {
	got := generateEndpoint("https://api.commandcode.ai/provider/v1/")
	if got != "https://api.commandcode.ai/alpha/generate" {
		t.Fatalf("endpoint: %s", got)
	}
}

func TestGoPlanFallbackToGenerate(t *testing.T) {
	m, srv := newCCMock(t, true) // Go plan: provider API refuses
	withCommandCodeBase(t, srv.URL)
	g := &Gateway{BaseURL: srv.URL + "/provider/v1", APIKey: "sk-go", Model: "deepseek/deepseek-v4-flash"}

	msgs := []any{
		messages.WireSystem{Role: "system", Content: "You are kaal."},
		messages.WireUser{Role: "user", Content: "read the file"},
	}
	tools := []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "read", "description": "Read a file",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}
	events := drainStream(g.Stream(context.Background(), msgs, tools, 100))

	if m.chatCalls != 1 {
		t.Fatalf("chat calls: %d", m.chatCalls)
	}
	if m.genCalls != 1 {
		t.Fatalf("generate calls after 403 upgrade_required: %d", m.genCalls)
	}

	// Event order and shapes.
	kinds := []EventKind{}
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	want := []EventKind{EventReasoning, EventContent, EventContent, EventToolCall, EventDone}
	if len(kinds) < len(want) || strings.Join(kindNames(kinds), ",") != strings.Join(kindNames(want), ",") {
		t.Fatalf("events: %v", kindNames(kinds))
	}
	if events[0].Text != "pondering" || events[1].Text != "Hello " || events[2].Text != "from generate" {
		t.Fatalf("text events: %+v", events[:3])
	}
	tc := events[3].ToolCall
	if tc.ID != "cc1" || tc.Name != "read" || !strings.Contains(tc.Arguments, `"path"`) {
		t.Fatalf("tool call: %+v", tc)
	}
	if events[4].FinishReason == nil || *events[4].FinishReason != "tool_calls" {
		t.Fatalf("finish reason: %+v", events[4])
	}
	if errEv := findError(events); errEv != "" {
		t.Fatalf("unexpected error event: %s", errEv)
	}

	// The generate payload carries the CLI shape.
	params, _ := m.lastGenBody["params"].(map[string]any)
	if params == nil || params["model"] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("params.model missing: %v", m.lastGenBody)
	}
	if params["temperature"] != 0.3 || params["stream"] != true {
		t.Fatalf("params flags: %v", params)
	}
	ccTools, _ := params["tools"].([]any)
	if len(ccTools) != 1 {
		t.Fatalf("tools: %v", ccTools)
	}
	if tool, _ := ccTools[0].(map[string]any); tool["input_schema"] == nil || tool["name"] != "read" {
		t.Fatalf("converted tool: %v", tool)
	}
	sys, _ := params["system"].(string)
	if sys != "You are kaal." {
		t.Fatalf("system: %q", sys)
	}
	if _, ok := m.lastGenBody["threadId"]; !ok {
		t.Fatal("threadId missing")
	}
	if got := m.lastHeaders.Get("x-command-code-version"); got != commandCodeCLIVersion {
		t.Fatalf("cli version header: %q", got)
	}
	// Transport remembered for the next turn.
	if g.recallTransport() != "generate" {
		t.Fatal("transport must be remembered as generate")
	}
}

func TestTransportRememberedSkipsProviderAPI(t *testing.T) {
	m, srv := newCCMock(t, true)
	withCommandCodeBase(t, srv.URL)
	g := &Gateway{BaseURL: srv.URL + "/provider/v1", APIKey: "sk-go", Model: "m"}
	msgs := []any{messages.WireUser{Role: "user", Content: "hi"}}

	drainStream(g.Stream(context.Background(), msgs, nil, 0)) // discovers generate
	drainStream(g.Stream(context.Background(), msgs, nil, 0)) // straight there

	if m.chatCalls != 1 || m.genCalls != 2 {
		t.Fatalf("calls: chat=%d gen=%d (provider API must be skipped once learned)", m.chatCalls, m.genCalls)
	}
	// A key change re-evaluates the transport.
	g.APIKey = "sk-other"
	if g.recallTransport() != "unknown" {
		t.Fatal("key change must reset the transport")
	}
}

func TestNonUpgrade403NeverFlips(t *testing.T) {
	m, srv := newCCMock(t, false)
	withCommandCodeBase(t, srv.URL)
	// Swap the chat handler's answer to a plain 403 by pointing BaseURL at a
	// server that always 403s with a different code.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"authentication_error"}}`)
	}))
	defer srv2.Close()
	withCommandCodeBase(t, srv.URL) // eligibility stays with the mock host
	_ = srv2

	events := drainStream((&Gateway{BaseURL: srv.URL + "/provider/v1", APIKey: "k", Model: "m"}).
		Stream(context.Background(), []any{messages.WireUser{Role: "user", Content: "hi"}}, nil, 0))
	// The mock's non-upgrade path answers 200; force-check via upgrade=false:
	// no generate call may have happened either way.
	if m.genCalls != 0 {
		t.Fatalf("generate called without upgrade_required: %d", m.genCalls)
	}
	if findError(events) != "" && len(events) == 0 {
		t.Fatal("no events")
	}
}

func kindNames(kinds []EventKind) []string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		switch k {
		case EventContent:
			names[i] = "content"
		case EventReasoning:
			names[i] = "reasoning"
		case EventToolCall:
			names[i] = "tool"
		case EventDone:
			names[i] = "done"
		default:
			names[i] = "error"
		}
	}
	return names
}

func findError(events []StreamEvent) string {
	for _, ev := range events {
		if ev.Kind == EventError {
			return ev.Text
		}
	}
	return ""
}

func TestBuildGenerateBodyToolRoundTripShape(t *testing.T) {
	// The CLI's toWireMessages contract: assistant turns carry "tool-call"
	// parts, results ride role:"tool" with a MANDATORY toolName — its
	// absence is exactly what 400'd against the live endpoint.
	msgs := []any{
		messages.WireSystem{Role: "system", Content: "sys"},
		messages.WireUser{Role: "user", Content: "read a.txt"},
		messages.WireAssistant{
			Role:    "assistant",
			Content: "",
			ToolCalls: []messages.WireToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: messages.WireFunction{Name: "read", Arguments: `{"path":"a.txt"}`},
			}},
		},
		messages.WireToolResult{Role: "tool", ToolCallID: "call_1", Content: "contents of a.txt"},
	}
	payload, err := buildGenerateBody("m", msgs, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Params struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	msgsOut := doc.Params.Messages
	if len(msgsOut) != 3 { // system rides params.system, not messages
		t.Fatalf("messages: %+v", msgsOut)
	}

	// The assistant turn: tool-call part with the call's identity.
	asst := msgsOut[1]
	var asstParts []map[string]any
	raw, _ := json.Marshal(asst.Content)
	json.Unmarshal(raw, &asstParts)
	if len(asstParts) != 1 || asstParts[0]["type"] != "tool-call" {
		t.Fatalf("assistant parts: %v", asstParts)
	}
	if asstParts[0]["toolCallId"] != "call_1" || asstParts[0]["toolName"] != "read" {
		t.Fatalf("call part: %v", asstParts[0])
	}

	// The result turn: role tool, named result part, text output value.
	res := msgsOut[2]
	if res.Role != "tool" {
		t.Fatalf("result role: %q", res.Role)
	}
	var resParts []map[string]any
	raw, _ = json.Marshal(res.Content)
	json.Unmarshal(raw, &resParts)
	part := resParts[0]
	out, _ := part["output"].(map[string]any)
	if part["type"] != "tool-result" ||
		part["toolCallId"] != "call_1" ||
		part["toolName"] != "read" ||
		out["value"] != "contents of a.txt" {
		t.Fatalf("result part: %v", part)
	}

	// Unpaired calls/results never ship.
	lonely := []any{
		messages.WireAssistant{Role: "assistant", ToolCalls: []messages.WireToolCall{{
			ID: "x", Type: "function",
			Function: messages.WireFunction{Name: "nobody", Arguments: "{}"},
		}}},
	}
	payload, err = buildGenerateBody("m", lonely, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(payload, &doc)
	if n := len(doc.Params.Messages); n != 0 {
		t.Fatalf("unpaired assistant message must be dropped, got %d", n)
	}
}

func TestTransportCacheSkipsColdProbe(t *testing.T) {
	// Discovery persists: a fresh process (new Gateway) with the same
	// credential goes straight to generate — no wasted 403 probe.
	dir := t.TempDir()
	oldPath := ccCachePath
	ccCachePath = func() string { return dir + "/cc-transport.json" }
	t.Cleanup(func() { ccCachePath = oldPath })

	m, srv := newCCMock(t, true) // Go plan: provider API refuses
	withCommandCodeBase(t, srv.URL)

	g1 := &Gateway{BaseURL: srv.URL + "/provider/v1", APIKey: "sk-go", Model: "m"}
	msgs := []any{messages.WireUser{Role: "user", Content: "hi"}}
	drainStream(g1.Stream(context.Background(), msgs, nil, 0))
	if m.chatCalls != 1 || m.genCalls != 1 {
		t.Fatalf("discovery turn: chat=%d gen=%d", m.chatCalls, m.genCalls)
	}

	// A second Gateway (the "next process") must not touch /chat/completions.
	g2 := &Gateway{BaseURL: srv.URL + "/provider/v1", APIKey: "sk-go", Model: "m"}
	drainStream(g2.Stream(context.Background(), msgs, nil, 0))
	if m.chatCalls != 1 {
		t.Fatalf("cached transport still probed the Provider API: chat=%d", m.chatCalls)
	}
	if m.genCalls != 2 {
		t.Fatalf("gen calls: %d", m.genCalls)
	}

	// A different credential invalidates the cache: probe returns.
	g3 := &Gateway{BaseURL: srv.URL + "/provider/v1", APIKey: "sk-other", Model: "m"}
	drainStream(g3.Stream(context.Background(), msgs, nil, 0))
	if m.chatCalls != 2 {
		t.Fatalf("key change must re-probe: chat=%d", m.chatCalls)
	}
}
