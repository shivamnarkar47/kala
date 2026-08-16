// Ported from tests/test_gateway.py (511 lines) — pure unit tests, no real
// network except the local httptest servers for transport behavior.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

func newGateway() *Gateway {
	return &Gateway{BaseURL: "https://example.test/v1", APIKey: "sk-test", Model: "deepseek-v4-flash"}
}

func collect(t *testing.T, ch <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// -- fake transport -----------------------------------------------------------

type fakeOpener struct {
	responses []func() (*http.Response, error)
	calls     int
}

func (f *fakeOpener) Do(req *http.Request, timeout time.Duration) (*http.Response, error) {
	if f.calls >= len(f.responses) {
		return nil, fmt.Errorf("fakeOpener: no more responses")
	}
	f.calls++
	return f.responses[f.calls-1]()
}

func resp(status int, header http.Header, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func respErr(err error) func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, err }
}

func http429(retryAfter string) func() (*http.Response, error) {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return resp(429, h, "rate limited")
}

var successSSE = "data: {\"choices\": [{\"delta\": {\"content\": \"hi\"}, \"finish_reason\": null}]}\n" +
	"\n" +
	"data: [DONE]\n" +
	"\n"

// failingReader serves one SSE line, then fails (the Python _FailingStream).
type failingReader struct {
	first  []byte
	err    error
	served bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		return copy(p, r.first), nil
	}
	return 0, r.err
}

// -- parse / headers / merge / body -------------------------------------------

func TestParseSSELine(t *testing.T) {
	cases := []struct {
		line string
		want *string
	}{
		{`data: {"a":1}`, strptr(`{"a":1}`)},
		{"data: [DONE]", strptr("[DONE]")},
		{"event: message", nil},
		{"", nil},
		{"data:", strptr("")},
	}
	for _, c := range cases {
		got := parseSSELine([]byte(c.line))
		if c.want == nil {
			if got != nil {
				t.Errorf("line %q: want nil, got %q", c.line, *got)
			}
			continue
		}
		if got == nil || *got != *c.want {
			t.Errorf("line %q: want %q, got %v", c.line, *c.want, got)
		}
	}
	// bytes input
	if got := parseSSELine([]byte("data: x")); got == nil || *got != "x" {
		t.Errorf("bytes input: want x, got %v", got)
	}
}

func TestBuildHeaders(t *testing.T) {
	h := buildHeaders("sk-test")
	if h.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("Authorization: %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type: %q", h.Get("Content-Type"))
	}
	if h.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept: %q", h.Get("Accept"))
	}
	if h.Get("User-Agent") != "python-requests/2.31.0" {
		t.Errorf("User-Agent: %q", h.Get("User-Agent"))
	}
}

func TestMergeToolCalls(t *testing.T) {
	dec := func(s string) []any {
		var v []any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	t.Run("merge same index", func(t *testing.T) {
		acc := map[int]*toolAccumulator{}
		mergeToolCalls(acc, dec(`[{"index": 0, "id": "call_1", "function": {"name": "get_weather", "arguments": "{\"ci"}}]`))
		mergeToolCalls(acc, dec(`[{"index": 0, "function": {"arguments": "ty\": \"SF\"}"}}]`))
		if acc[0].id != "call_1" || acc[0].name != "get_weather" || acc[0].arguments != `{"city": "SF"}` {
			t.Fatalf("got %+v", acc[0])
		}
	})
	t.Run("arguments delta concatenates", func(t *testing.T) {
		acc := map[int]*toolAccumulator{}
		mergeToolCalls(acc, dec(`[{"index": 0, "function": {"name": "f", "arguments_delta": "a"}}]`))
		mergeToolCalls(acc, dec(`[{"index": 0, "function": {"arguments_delta": "b"}}]`))
		if acc[0].arguments != "ab" {
			t.Fatalf("got %q", acc[0].arguments)
		}
	})
	t.Run("different indices accumulate separately", func(t *testing.T) {
		acc := map[int]*toolAccumulator{}
		mergeToolCalls(acc, dec(`[{"index": 0, "function": {"name": "a"}}]`))
		mergeToolCalls(acc, dec(`[{"index": 1, "function": {"name": "b"}}]`))
		if len(acc) != 2 || acc[0].name != "a" || acc[1].name != "b" {
			t.Fatalf("got %+v", acc)
		}
	})
	t.Run("missing fields do not clobber", func(t *testing.T) {
		acc := map[int]*toolAccumulator{}
		mergeToolCalls(acc, dec(`[{"index": 0, "id": "call_x", "function": {"name": "n", "arguments": "{}"}}]`))
		mergeToolCalls(acc, dec(`[{"index": 0}]`))
		if acc[0].id != "call_x" || acc[0].name != "n" || acc[0].arguments != "{}" {
			t.Fatalf("got %+v", acc[0])
		}
	})
	t.Run("default index zero", func(t *testing.T) {
		acc := map[int]*toolAccumulator{}
		mergeToolCalls(acc, dec(`[{"function": {"name": "f"}}]`))
		if len(acc) != 1 || acc[0] == nil {
			t.Fatalf("got %+v", acc)
		}
	})
}

func TestBuildBody(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "f", "parameters": map[string]any{}}}}
	body := BuildBody("deepseek-v4-flash", []any{map[string]any{"role": "user", "content": "hi"}}, tools, 0)
	if body.Model != "deepseek-v4-flash" || body.MaxTokens != config.MaxOutputTokens || !body.Stream {
		t.Fatalf("body basics wrong: %+v", body)
	}
	if len(body.Tools) != 1 {
		t.Fatalf("tools missing: %+v", body)
	}
	b, err := jsonpy.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"tool_choice", "temperature", "stream_options", "store"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("banned field %q present in body: %s", banned, b)
		}
	}
	noTools := BuildBody("deepseek-v4-flash", []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0)
	if noTools.Tools != nil {
		t.Fatalf("tools must be omitted when nil: %+v", noTools)
	}
	emptyTools := BuildBody("deepseek-v4-flash", []any{map[string]any{"role": "user", "content": "hi"}}, []any{}, 0)
	if emptyTools.Tools != nil {
		t.Fatalf("tools must be omitted when empty: %+v", emptyTools)
	}
}

// TestBuildBodyGoldenTurn2 is the exact 400-on-turn-2 trap, caught at unit
// level: the turn-2 request body (assistant turn with a tool call must
// replay its streamed reasoning_content verbatim) must be byte-identical to
// the Python build's body. The golden fixture was generated by
// harness/gateway._build_body + json.dumps(ensure_ascii=False).
func TestBuildBodyGoldenTurn2(t *testing.T) {
	reasoning := "first read, then summarize"
	msgs := []messages.Message{
		messages.UserMessage{Text: "read the file"},
		messages.AssistantMessage{
			Content:          "",
			ReasoningContent: &reasoning,
			ToolCalls:        []messages.ToolCall{{ID: "call_1", Name: "read", Arguments: `{"path": "a.txt"}`}},
		},
		messages.ToolResultMessage{ToolCallID: "call_1", Content: "file contents here"},
	}
	body := BuildBody("deepseek-v4-flash", messages.ToWireMessages(msgs), nil, 0)
	got, err := jsonpy.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/turn2.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("turn-2 body not byte-identical to Python fixture:\n got  %s\n want %s", got, want)
	}
	if !bytes.Contains(got, []byte("reasoning_content")) {
		t.Fatal("golden body must contain reasoning_content (the replay trap)")
	}

	// The negative case: without streamed reasoning the field is absent.
	noReasoning := []messages.Message{
		messages.UserMessage{Text: "read the file"},
		messages.AssistantMessage{Content: "ok", ToolCalls: []messages.ToolCall{{ID: "call_1", Name: "read", Arguments: `{"path": "a.txt"}`}}},
		messages.ToolResultMessage{ToolCallID: "call_1", Content: "file contents here"},
	}
	got2, _ := jsonpy.Marshal(BuildBody("deepseek-v4-flash", messages.ToWireMessages(noReasoning), nil, 0))
	if bytes.Contains(got2, []byte("reasoning_content")) {
		t.Fatalf("reasoning_content must be absent without streamed reasoning: %s", got2)
	}
}

// -- stream events and retries ------------------------------------------------

func TestStreamEvents(t *testing.T) {
	payload := "data: {\"choices\": [{\"delta\": {\"reasoning_content\": \"think\", \"content\": \"hi\"}, \"finish_reason\": null}]}\n" +
		"\n" +
		"data: {\"choices\": [{\"delta\": {\"tool_calls\": [{\"index\": 0, \"id\": \"call_abc\", \"function\": {\"name\": \"lookup\", \"arguments\": \"{}\"}}]}, \"finish_reason\": \"tool_calls\"}]}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n"
	opener := &fakeOpener{responses: []func() (*http.Response, error){resp(200, nil, payload)}}
	urlOpen = opener
	defer setTransport()
	events := collect(t, newGateway().Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
	if opener.calls != 1 {
		t.Fatalf("want 1 open, got %d", opener.calls)
	}
	want := []StreamEvent{
		{Kind: EventReasoning, Text: "think"},
		{Kind: EventContent, Text: "hi"},
		{Kind: EventToolCall, ToolCall: messages.ToolCall{ID: "call_abc", Name: "lookup", Arguments: "{}"}},
		{Kind: EventDone},
	}
	if len(events) != len(want) {
		t.Fatalf("want %d events, got %d: %+v", len(want), len(events), events)
	}
	for i := range want {
		if events[i].Kind != want[i].Kind || events[i].Text != want[i].Text || events[i].ToolCall != want[i].ToolCall {
			t.Fatalf("event %d: want %+v, got %+v", i, want[i], events[i])
		}
	}
}

func TestStreamRetryOn429(t *testing.T) {
	t.Run("retries then succeeds", func(t *testing.T) {
		opener := &fakeOpener{responses: []func() (*http.Response, error){
			http429("2"), http429("2"), resp(200, nil, successSSE),
		}}
		urlOpen = opener
		defer setTransport()
		var sleeps []time.Duration
		sleepFn = func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return true }
		defer func() { sleepFn = func(ctx context.Context, d time.Duration) bool { return true } }()
		events := collect(t, newGateway().Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
		if len(events) != 2 || events[0].Kind != EventContent || events[1].Kind != EventDone {
			t.Fatalf("want [content hi, done], got %+v", events)
		}
		if opener.calls != 3 {
			t.Fatalf("want 3 opens, got %d", opener.calls)
		}
		if len(sleeps) != 2 {
			t.Fatalf("want 2 sleeps, got %v", sleeps)
		}
		for _, s := range sleeps {
			if s < 2*time.Second { // Retry-After: 2 beats 1s/2s backoff
				t.Errorf("sleep %v below Retry-After 2s", s)
			}
		}
	})
	t.Run("exhausts retries", func(t *testing.T) {
		opener := &fakeOpener{responses: []func() (*http.Response, error){http429("1"), http429("1"), http429("1")}}
		urlOpen = opener
		defer setTransport()
		var sleeps []time.Duration
		sleepFn = func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return true }
		defer func() { sleepFn = func(ctx context.Context, d time.Duration) bool { return true } }()
		events := collect(t, newGateway().Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
		if len(events) != 1 || events[0].Kind != EventError || !strings.Contains(events[0].Text, "429") {
			t.Fatalf("want error mentioning 429, got %+v", events)
		}
		if opener.calls != 3 {
			t.Fatalf("want 3 opens, got %d", opener.calls)
		}
		if len(sleeps) != 2 {
			t.Fatalf("want 2 sleeps, got %v", sleeps)
		}
		for _, s := range sleeps {
			if s < time.Second {
				t.Errorf("sleep %v below 1s", s)
			}
		}
	})
	t.Run("429 after content raises", func(t *testing.T) {
		firstLine := []byte("data: {\"choices\": [{\"delta\": {\"content\": \"hi\"}, \"finish_reason\": null}]}\n")
		body := &failingReader{first: firstLine, err: &HTTPStatusError{Code: 429, Header: http.Header{}, Body: []byte("rate limited")}}
		opener := &fakeOpener{responses: []func() (*http.Response, error){
			func() (*http.Response, error) { return &http.Response{StatusCode: 200, Body: io.NopCloser(body)}, nil },
		}}
		urlOpen = opener
		defer setTransport()
		var sleeps []time.Duration
		sleepFn = func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return true }
		defer func() { sleepFn = func(ctx context.Context, d time.Duration) bool { return true } }()
		events := collect(t, newGateway().Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
		if len(events) != 2 || events[0].Kind != EventContent || events[1].Kind != EventError {
			t.Fatalf("want [content, error], got %+v", events)
		}
		if !strings.Contains(events[1].Text, "interrupted after content") {
			t.Fatalf("want interrupted-after-content error, got %q", events[1].Text)
		}
		if opener.calls != 1 { // retry guard: no second attempt after content
			t.Fatalf("want 1 open, got %d", opener.calls)
		}
		if len(sleeps) != 0 {
			t.Fatalf("want no sleeps, got %v", sleeps)
		}
	})
	t.Run("retry after http date", func(t *testing.T) {
		retryAt := time.Now().Add(5 * time.Second).Format(http.TimeFormat)
		opener := &fakeOpener{responses: []func() (*http.Response, error){
			http429(retryAt), resp(200, nil, successSSE),
		}}
		urlOpen = opener
		defer setTransport()
		var sleeps []time.Duration
		sleepFn = func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return true }
		defer func() { sleepFn = func(ctx context.Context, d time.Duration) bool { return true } }()
		events := collect(t, newGateway().Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
		if len(events) != 2 || events[0].Kind != EventContent {
			t.Fatalf("want [content, done], got %+v", events)
		}
		if opener.calls != 2 {
			t.Fatalf("want 2 opens, got %d", opener.calls)
		}
		if len(sleeps) != 1 || sleeps[0] < 2*time.Second { // honors date-form Retry-After
			t.Fatalf("want sleep >= 2s honoring date, got %v", sleeps)
		}
	})
}

func TestOther4xxRaisesImmediately(t *testing.T) {
	opener := &fakeOpener{responses: []func() (*http.Response, error){resp(400, nil, "bad key")}}
	urlOpen = opener
	defer setTransport()
	var sleeps []time.Duration
	sleepFn = func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return true }
	defer func() { sleepFn = func(ctx context.Context, d time.Duration) bool { return true } }()
	events := collect(t, newGateway().Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
	if len(events) != 1 || events[0].Kind != EventError || !strings.Contains(events[0].Text, "gateway HTTP 400") {
		t.Fatalf("want gateway HTTP 400 error, got %+v", events)
	}
	if opener.calls != 1 {
		t.Fatalf("want 1 open, got %d", opener.calls)
	}
	if len(sleeps) != 0 {
		t.Fatalf("want no sleeps, got %v", sleeps)
	}
}

// -- transport ----------------------------------------------------------------

func startCountingServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(successSSE)))
		w.WriteHeader(200)
		io.WriteString(w, successSSE)
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

func streamTwice(t *testing.T, g *Gateway) {
	t.Helper()
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	for i := 0; i < 2; i++ {
		events := collect(t, g.Stream(context.Background(), msgs, nil, 0))
		if len(events) != 2 || events[0].Kind != EventContent || events[1].Kind != EventDone {
			t.Fatalf("stream %d: want [content, done], got %+v", i, events)
		}
	}
}

func TestKeepaliveTwoStreamsOneConnection(t *testing.T) {
	// Two sequential streams must ride ONE pooled connection (the Python
	// per-thread socket juggling disappears; the transport pools natively).
	srv, conns := startCountingServer(t)
	g := &Gateway{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "deepseek-v4-flash"}
	streamTwice(t, g)
	if got := atomic.LoadInt32(conns); got != 1 {
		t.Fatalf("want 1 connection for 2 streams, got %d", got)
	}
}

func TestNoKeepaliveEnvUsesSeparateConnections(t *testing.T) {
	// KAAL_NO_KEEPALIVE=1: no connection reuse — each stream opens its own.
	t.Setenv("KAAL_NO_KEEPALIVE", "1")
	setTransport()
	defer setTransport()
	srv, conns := startCountingServer(t)
	g := &Gateway{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "deepseek-v4-flash"}
	streamTwice(t, g)
	if got := atomic.LoadInt32(conns); got != 2 {
		t.Fatalf("want 2 connections without keep-alive, got %d", got)
	}
}

func TestProxyEnvDisablesKeepalive(t *testing.T) {
	t.Setenv("http_proxy", "http://proxy:3128")
	setTransport()
	defer setTransport()
	if keepaliveEnabled() {
		t.Fatal("keepalive must be disabled with a proxy env var")
	}
}

func TestStreamRetryThroughRealServer(t *testing.T) {
	// A 429 through the real transport must flow through the same retry
	// logic: 3 attempts hit the server, then the error surfaces. (Go's
	// transport may reuse the pooled connection across attempts — pooling
	// is the point of the Go build — so only the request count is asserted.)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		attempts++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		io.WriteString(w, "rate limited")
	}))
	defer srv.Close()
	g := &Gateway{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "deepseek-v4-flash"}
	var sleeps []time.Duration
	sleepFn = func(_ context.Context, d time.Duration) bool { sleeps = append(sleeps, d); return true }
	defer func() { sleepFn = func(ctx context.Context, d time.Duration) bool { return true } }()
	events := collect(t, g.Stream(context.Background(), []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
	if attempts != 3 {
		t.Fatalf("want 3 attempts, got %d", attempts)
	}
	if len(sleeps) != 2 {
		t.Fatalf("want 2 sleeps, got %v", sleeps)
	}
	if len(events) != 1 || events[0].Kind != EventError || !strings.Contains(events[0].Text, "429") {
		t.Fatalf("want 429 error, got %+v", events)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	// A cancelled context must abort immediately — no retry sleeps.
	opener := &fakeOpener{responses: []func() (*http.Response, error){respErr(errors.New("boom"))}}
	urlOpen = opener
	defer setTransport()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := collect(t, newGateway().Stream(ctx, []any{map[string]any{"role": "user", "content": "hi"}}, nil, 0))
	if len(events) != 0 {
		t.Fatalf("cancelled stream must yield nothing, got %+v", events)
	}
	if opener.calls != 1 {
		t.Fatalf("want 1 open, got %d", opener.calls)
	}
}

// -- helpers ------------------------------------------------------------------

func strptr(s string) *string { return &s }
