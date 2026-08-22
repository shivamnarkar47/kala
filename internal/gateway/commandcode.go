// Package gateway — commandcode.go: the Go-plan transport for Command Code.
//
// Provider-plan accounts stream OpenAI chat completions from
// /provider/v1/chat/completions like any other gateway. Go-plan ($1)
// accounts have no Provider API access: that endpoint answers 403
// upgrade_required, and the working transport is the CLI's own
// POST /alpha/generate (protocol reverse-engineered from
// github.com/patlux/pi-commandcode-provider).
//
// The router mirrors pi's rule exactly: only a 403 whose body carries code
// "upgrade_required" flips this Gateway to the generate transport; every
// other error never triggers the fallback. The detected transport is
// remembered per process and re-evaluated when the API key changes.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

// commandCodeGenerateMaxTokens caps the generate output budget
// (pi's DEFAULT_GENERATE_MAX_TOKENS).
const commandCodeGenerateMaxTokens = 64_000

// commandCodeCLIVersion is the CLI version header the generate endpoint
// expects (capability snapshot of command-code@1.15.1).
const commandCodeCLIVersion = "1.15.1"

// commandCodeBase reports whether an endpoint speaks Command Code's provider
// shape — the only place a Go plan can answer 403 upgrade_required. Tests
// repoint it at their mock server.
var commandCodeBase = func(baseURL string) bool {
	return strings.Contains(baseURL, "//api.commandcode.ai/")
}

// generateEndpoint derives /alpha/generate from the provider base:
// https://api.commandcode.ai/provider/v1 → https://api.commandcode.ai/alpha/generate.
func generateEndpoint(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if i := strings.LastIndex(trimmed, "/provider/v1"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return trimmed + "/alpha/generate"
}

// isUpgradeRequired reports whether an error body is Command Code's
// "your plan has no Provider API access" answer.
func isUpgradeRequired(body []byte) bool {
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	return doc.Error.Code == "upgrade_required" || doc.Code == "upgrade_required"
}

// -- transport memory ------------------------------------------------------------

// ccCacheRecord is the persisted transport discovery: which endpoint the
// credential (fingerprinted, never the key itself) resolves to. Spares every
// new process the wasted 403 probe against the Provider API.
type ccCacheRecord struct {
	KeyFP     string `json:"key_fp"`
	Host      string `json:"host"`
	Transport string `json:"transport"`
	SavedAt   string `json:"saved_at"`
}

// ccCachePath is the discovery-cache location; tests repoint it.
var ccCachePath = func() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "kaal", "cc-transport.json")
}

func ccKeyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

func ccHost(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil {
		return u.Host
	}
	return baseURL
}

// loadCachedTransport returns the cached transport for this key+host, or "".
func loadCachedTransport(apiKey, baseURL string) string {
	path := ccCachePath()
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var rec ccCacheRecord
	if json.Unmarshal(raw, &rec) != nil {
		return ""
	}
	if rec.KeyFP != ccKeyFingerprint(apiKey) || rec.Host != ccHost(baseURL) {
		return ""
	}
	if rec.Transport != "generate" && rec.Transport != "provider" {
		return ""
	}
	return rec.Transport
}

// storeCachedTransport persists the discovery (best-effort; failures are
// silent — the probe just runs again next process).
func storeCachedTransport(apiKey, baseURL, transport string) {
	path := ccCachePath()
	if path == "" {
		return
	}
	rec := ccCacheRecord{
		KeyFP:     ccKeyFingerprint(apiKey),
		Host:      ccHost(baseURL),
		Transport: transport,
		SavedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, payload, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// recallTransport returns the remembered transport ("provider" |
// "generate"), consulting the persistent cache on an in-flight miss and
// re-evaluating when the credential changed since it resolved.
func (g *Gateway) recallTransport() string {
	if !commandCodeBase(g.BaseURL) {
		return "provider"
	}
	g.ccMu.Lock()
	defer g.ccMu.Unlock()
	if g.ccKey != g.APIKey {
		g.ccTransport = ""
		g.ccKey = g.APIKey
	}
	if g.ccTransport == "" || g.ccTransport == "unknown" {
		if cached := loadCachedTransport(g.APIKey, g.BaseURL); cached != "" {
			g.ccTransport = cached
		} else {
			g.ccTransport = "unknown"
		}
	}
	return g.ccTransport
}

// rememberTransport stores the transport the current credential resolved to,
// in memory and on disk.
func (g *Gateway) rememberTransport(t string) {
	g.ccMu.Lock()
	g.ccTransport = t
	g.ccKey = g.APIKey
	g.ccMu.Unlock()
	storeCachedTransport(g.APIKey, g.BaseURL, t)
}

// -- request shaping ---------------------------------------------------------------

// The /alpha/generate wire shape mirrors the official CLI's toWireMessages
// (read out of its dist bundle): assistant content is a part array of
// text | tool-call | reasoning; every executed call returns as its own
// role:"tool" message whose tool-result part MUST carry toolName (the CLI
// defaults it to "unknown"); thinking replays as reasoning parts.

// ccToolCallPart is one assistant tool-call part.
type ccToolCallPart struct {
	Type       string         `json:"type"` // "tool-call"
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Input      map[string]any `json:"input"`
}

// ccTextPart is one text part.
type ccTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ccReasoningPart replays the previous turn's thinking.
type ccReasoningPart struct {
	Type string `json:"type"` // "reasoning"
	Text string `json:"text"`
}

// ccToolResult is one tool-result side of a round trip; toolName is
// mandatory on the wire (the CLI defaults it to "unknown").
type ccToolResult struct {
	Type       string `json:"type"` // "tool-result"
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Output     struct {
		Type  string `json:"type"` // "text" | "error-text"
		Value string `json:"value"`
	} `json:"output"`
}

// buildGenerateBody converts kaal's wire messages into the CLI-shaped
// /alpha/generate payload. reasoning_content is deliberately not replayed:
// the generate backend keeps its own thread state (pi's converter drops it
// too), and the replay rule exists for the OpenAI-shaped gateways.
func buildGenerateBody(modelID string, msgs []any, tools []any, maxTokens int) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = config.MaxOutputTokens
	}
	if maxTokens > commandCodeGenerateMaxTokens {
		maxTokens = commandCodeGenerateMaxTokens
	}

	// Paired tool calls only: an assistant tool-call ships when a tool
	// result with the same id exists (pi's completeToolCallIds rule).
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	callNames := map[string]string{}
	for _, m := range msgs {
		switch msg := m.(type) {
		case *messages.WireAssistant:
			for _, tc := range msg.ToolCalls {
				callIDs[tc.ID] = true
				callNames[tc.ID] = tc.Function.Name
			}
		case *messages.WireToolResult:
			resultIDs[msg.ToolCallID] = true
		case messages.WireAssistant:
			for _, tc := range msg.ToolCalls {
				callIDs[tc.ID] = true
				callNames[tc.ID] = tc.Function.Name
			}
		case messages.WireToolResult:
			resultIDs[msg.ToolCallID] = true
		}
	}
	paired := func(id string) bool { return id != "" && callIDs[id] && resultIDs[id] }

	var systemParts []string
	ccMsgs := make([]map[string]any, 0, len(msgs))
	appendText := func(role, text string) {
		if text == "" {
			return
		}
		ccMsgs = append(ccMsgs, map[string]any{"role": role, "content": text})
	}
	appendAssistant := func(msg *messages.WireAssistant) {
		var parts []any
		if msg.Content != "" {
			parts = append(parts, ccTextPart{Type: "text", Text: msg.Content})
		}
		if msg.Reasoning != "" {
			parts = append(parts, ccReasoningPart{Type: "reasoning", Text: msg.Reasoning})
		}
		for _, tc := range msg.ToolCalls {
			if !paired(tc.ID) {
				continue
			}
			input := map[string]any{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			parts = append(parts, ccToolCallPart{
				Type: "tool-call", ToolCallID: tc.ID, ToolName: tc.Function.Name, Input: input,
			})
		}
		if len(parts) > 0 {
			ccMsgs = append(ccMsgs, map[string]any{"role": "assistant", "content": parts})
		}
	}
	appendToolResult := func(msg *messages.WireToolResult) {
		if !paired(msg.ToolCallID) {
			return
		}
		part := ccToolResult{Type: "tool-result", ToolCallID: msg.ToolCallID}
		part.ToolName = callNames[msg.ToolCallID]
		if part.ToolName == "" {
			part.ToolName = "unknown" // the CLI's own default
		}
		part.Output.Type = "text"
		part.Output.Value = msg.Content
		ccMsgs = append(ccMsgs, map[string]any{
			"role":    "tool",
			"content": []any{part},
		})
	}
	for _, m := range msgs {
		switch msg := m.(type) {
		case *messages.WireSystem:
			systemParts = append(systemParts, msg.Content)
		case messages.WireSystem:
			systemParts = append(systemParts, msg.Content)
		case *messages.WireUser:
			appendText("user", msg.Content)
		case messages.WireUser:
			appendText("user", msg.Content)
		case *messages.WireAssistant:
			appendAssistant(msg)
		case messages.WireAssistant:
			appendAssistant(&msg)
		case *messages.WireToolResult:
			appendToolResult(msg)
		case messages.WireToolResult:
			appendToolResult(&msg)
		}
	}

	// Tools arrive in OpenAI shape; the generate endpoint wants flat
	// Anthropic-flavored entries.
	ccTools := make([]any, 0, len(tools))
	for _, t := range tools {
		raw, err := json.Marshal(t)
		if err != nil {
			continue
		}
		var spec struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &spec) != nil || spec.Function.Name == "" {
			continue
		}
		params := spec.Function.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		ccTools = append(ccTools, map[string]any{
			"type":         "function",
			"name":         spec.Function.Name,
			"description":  spec.Function.Description,
			"input_schema": params,
		})
	}

	cwd, _ := os.Getwd()
	payload := map[string]any{
		"config": map[string]any{
			"workingDir":    cwd,
			"date":          time.Now().Format("2006-01-02"),
			"environment":   fmt.Sprintf("%s-%s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version()),
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"memory": nil,
		"taste":  nil,
		"skills": nil,
		"params": map[string]any{
			"model":       modelID,
			"messages":    ccMsgs,
			"tools":       ccTools,
			"system":      strings.Join(systemParts, "\n\n"),
			"max_tokens":  maxTokens,
			"temperature": 0.3,
			"stream":      true,
		},
		"threadId": newThreadID(),
	}
	return jsonpy.Marshal(payload)
}

// newThreadID mints a random v4-shaped UUID from the stdlib alone.
func newThreadID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// projectSlug slugifies the working directory (the CLI's x-project-slug).
func projectSlug(dir string) string {
	s := strings.ToLower(filepath.ToSlash(dir))
	s = strings.TrimPrefix(s, "/")
	var sb strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && sb.Len() > 0 {
				sb.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "project"
	}
	return out
}

// -- generate stream ----------------------------------------------------------------

// streamGenerate runs one POST /alpha/generate turn and maps its NDJSON
// event stream onto kaal's StreamEvents. Single attempt: failures arrive as
// an EventError (the endpoint has no documented retry semantics worth
// guessing at).
func (g *Gateway) streamGenerate(ctx context.Context, ch chan<- StreamEvent, msgs []any, tools []any, maxTokens int) {
	emit := func(ev StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	body, err := buildGenerateBody(g.Model, msgs, tools, maxTokens)
	if err != nil {
		ch <- StreamEvent{Kind: EventError, Text: "commandcode generate: marshal body: " + err.Error()}
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, generateEndpoint(g.BaseURL), bytes.NewReader(body))
	if err != nil {
		ch <- StreamEvent{Kind: EventError, Text: "commandcode generate: " + err.Error()}
		return
	}
	h := buildHeaders(g.APIKey)
	h.Set("x-command-code-version", commandCodeCLIVersion)
	h.Set("x-cli-environment", "production")
	cwd, _ := os.Getwd()
	h.Set("x-project-slug", projectSlug(cwd))
	h.Set("x-taste-learning", "true")
	h.Set("x-co-flag", "false")
	req.Header = h

	op := urlOpen
	if g.Opener != nil {
		op = g.Opener
	}
	resp, err := op.Do(req, config.RequestTimeout*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		ch <- StreamEvent{Kind: EventError, Text: "commandcode generate: " + err.Error()}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		// Diagnostic seam: dump the rejected payload for offline bisection.
		if path := os.Getenv("KAAL_DEBUG_CC"); path != "" && resp.StatusCode == 400 {
			_ = os.WriteFile(path, body, 0o600)
		}
		ch <- StreamEvent{Kind: EventError, Text: fmt.Sprintf(
			"commandcode generate HTTP %d: %s", resp.StatusCode, preview(errBody))}
		return
	}

	finishReason := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		payload, ok := parseGenerateLine(scanner.Text())
		if !ok {
			continue
		}
		if done := g.handleGenerateEvent(payload, &finishReason, emit); done {
			return
		}
	}
	// Server closed without a terminal finish: the loop treats a bare done
	// as turn completion; carry the last step's reason when we saw one.
	if finishReason != "" {
		reason := finishReason
		emit(StreamEvent{Kind: EventDone, FinishReason: &reason})
		return
	}
	emit(StreamEvent{Kind: EventDone})
}

// handleGenerateEvent maps one generate event; returns true when the stream
// is finished (or fatally errored).
func (g *Gateway) handleGenerateEvent(ev map[string]any, finishReason *string, emit func(StreamEvent) bool) bool {
	text := func(key string) string {
		s, _ := ev[key].(string)
		return s
	}
	switch ev["type"] {
	case "text-delta":
		if t := text("text"); t != "" {
			return !emit(StreamEvent{Kind: EventContent, Text: t})
		}
	case "reasoning-delta":
		if t := text("text"); t != "" {
			return !emit(StreamEvent{Kind: EventReasoning, Text: t})
		}
	case "tool-call":
		id, _ := ev["toolCallId"].(string)
		name, _ := ev["toolName"].(string)
		input, ok := ev["input"].(map[string]any)
		if !ok {
			for _, alt := range []string{"args", "arguments"} {
				if m, isMap := ev[alt].(map[string]any); isMap {
					input = m
					break
				}
			}
		}
		args, err := json.Marshal(input)
		if err != nil {
			args = []byte("{}")
		}
		if name != "" {
			if id == "" {
				id = fmt.Sprintf("call_%d", time.Now().UnixNano())
			}
			return !emit(StreamEvent{Kind: EventToolCall, ToolCall: messages.ToolCall{
				ID: id, Name: name, Arguments: string(args),
			}})
		}
	case "finish":
		reason := mapFinishGenerate(ev["finishReason"])
		*finishReason = reason
		emit(StreamEvent{Kind: EventDone, FinishReason: &reason})
		return true // terminal: skip the trailing bare-done emit
	case "finish-step":
		// One model call inside the stream; remember its reason for the
		// final done but keep scanning (a terminal finish may still come).
		raw, ok := ev["rawFinishReason"].(string)
		if !ok {
			raw, _ = ev["finishReason"].(string)
		}
		if raw != "" {
			*finishReason = mapFinishGenerate(raw)
		}
	case "error":
		msg := generateErrorMessage(ev)
		emit(StreamEvent{Kind: EventError, Text: msg})
		return true
	}
	return false
}

// parseGenerateLine tolerates NDJSON and stray SSE framing alike (pi's
// parseStreamEventLine rule); nil-ok for non-payload lines.
func parseGenerateLine(line string) (map[string]any, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, ":") || strings.HasPrefix(s, "event:") {
		return nil, false
	}
	if strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(s[len("data:"):])
	}
	if s == "" || s == "[DONE]" {
		return nil, false
	}
	var ev map[string]any
	if json.Unmarshal([]byte(s), &ev) != nil || ev == nil {
		return nil, false
	}
	return ev, true
}

// mapFinishGenerate translates the generate finish reasons onto the OpenAI
// vocabulary kaal's loop speaks.
func mapFinishGenerate(reason any) string {
	switch r := reason.(type) {
	case string:
		switch r {
		case "tool-calls", "toolUse", "tool_use":
			return "tool_calls"
		case "length", "max_tokens", "max-tokens", "max_output_tokens":
			return "length"
		}
	}
	return "stop"
}

// generateErrorMessage digs the human message out of a generate error event.
func generateErrorMessage(ev map[string]any) string {
	for _, key := range []string{"error", "message"} {
		switch v := ev[key].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if m, ok := v["message"].(string); ok && m != "" {
				return m
			}
		}
	}
	return "commandcode generate stream error"
}
