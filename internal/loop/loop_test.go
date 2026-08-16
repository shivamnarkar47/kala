// Ported from tests/test_loop.py — the whole loop contract. FakeGateway
// yields pre-scripted StreamEvents and records every Stream() invocation;
// StubRegistry is a minimal tool registry. Both are thread-safe so
// concurrent nested loops can share one gateway.
package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaal/kaal/internal/gateway"
	"github.com/kaal/kaal/internal/loop"
	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/messages"
	"github.com/kaal/kaal/internal/prompts"
	"github.com/kaal/kaal/internal/sessions"
	"github.com/kaal/kaal/internal/toolcache"
	"github.com/kaal/kaal/internal/tools"
)

// Unicode markers, built from escapes.
const (
	fw = "\uff5c" // fullwidth pipe ｜
	b  = "\u2581" // block glyph ▁
)

var dsmlWrite = "<" + fw + "DSML" + fw + "tool_calls>" +
	"<" + fw + "DSML" + fw + `invoke name="write">` +
	"<" + fw + "DSML" + fw + `parameter name="path" string="true">hello.txt</` + fw + "DSML" + fw + "parameter>" +
	"<" + fw + "DSML" + fw + `parameter name="content" string="true">hi</` + fw + "DSML" + fw + "parameter>" +
	"</" + fw + "DSML" + fw + "invoke>" +
	"</" + fw + "DSML" + fw + "tool_calls>"

// eventSleep is a fake-only pseudo-event pausing the stream.
const eventSleep = gateway.EventKind(99)

func sleepEvent(seconds float64) gateway.StreamEvent {
	return gateway.StreamEvent{Kind: eventSleep, Text: strconv.FormatFloat(seconds, 'f', -1, 64)}
}

// --- fixtures ----------------------------------------------------------------

func contentEv(text string) gateway.StreamEvent {
	return gateway.StreamEvent{Kind: gateway.EventContent, Text: text}
}

func reasoningEv(text string) gateway.StreamEvent {
	return gateway.StreamEvent{Kind: gateway.EventReasoning, Text: text}
}

func toolCallEv(call messages.ToolCall) gateway.StreamEvent {
	return gateway.StreamEvent{Kind: gateway.EventToolCall, ToolCall: call}
}

func doneEv(reason string) gateway.StreamEvent {
	var r *string
	if reason != "" {
		r = &reason
	}
	return gateway.StreamEvent{Kind: gateway.EventDone, FinishReason: r}
}

// fakeGateway yields pre-scripted StreamEvents; records every Stream()
// invocation. Thread-safe so concurrent nested loops can share one gateway.
type fakeGateway struct {
	mu      sync.Mutex
	scripts [][]gateway.StreamEvent
	calls   []streamCall
}

type streamCall struct {
	msgs  []any
	tools []any
}

func newFakeGateway(scripts ...[]gateway.StreamEvent) *fakeGateway {
	return &fakeGateway{scripts: scripts}
}

func (f *fakeGateway) Stream(ctx context.Context, msgs []any, toolsList []any, maxTokens int) <-chan gateway.StreamEvent {
	f.mu.Lock()
	script := f.scripts[0]
	f.scripts = f.scripts[1:]
	f.calls = append(f.calls, streamCall{msgs: msgs, tools: toolsList})
	f.mu.Unlock()
	ch := make(chan gateway.StreamEvent, len(script)+1)
	go func() {
		defer close(ch)
		for _, ev := range script {
			if ev.Kind == eventSleep {
				secs, _ := strconv.ParseFloat(ev.Text, 64)
				time.Sleep(time.Duration(secs * float64(time.Second)))
				continue
			}
			ch <- ev
		}
	}()
	return ch
}

func (f *fakeGateway) ModelID() string { return "" }

func (f *fakeGateway) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeGateway) callMsgs(i int) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i].msgs
}

func (f *fakeGateway) remainingScripts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scripts)
}

// stubRegistry is the minimal tool registry: sleepable read handler
// recording call order/worker ids. Reads with a path in failReads return an
// error (a tool failure that counts toward the consecutive-failure budget).
type stubRegistry struct {
	projectDir string
	readDelay  float64
	failReads  map[string]bool
	mu         sync.Mutex
	record     []recordEntry
	askHandler tools.AskHandler
}

type recordEntry struct {
	name   string
	path   string
	worker string
}

func newStubRegistry(projectDir string, readDelay float64, failReads ...string) *stubRegistry {
	fails := map[string]bool{}
	for _, p := range failReads {
		fails[p] = true
	}
	return &stubRegistry{projectDir: projectDir, readDelay: readDelay, failReads: fails}
}

func (s *stubRegistry) Schemas() []any                               { return nil }
func (s *stubRegistry) BeginBatch(names []string, sig string)        {}
func (s *stubRegistry) EndBatch(mutated bool)                        {}
func (s *stubRegistry) SetSpawnHandler(h tools.SpawnHandler)         {}
func (s *stubRegistry) SetSpawnManyHandler(h tools.SpawnManyHandler) {}
func (s *stubRegistry) SetAskHandler(h tools.AskHandler)             { s.askHandler = h }
func (s *stubRegistry) ProjectDir() string                           { return s.projectDir }

func (s *stubRegistry) Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	worker := "main"
	if id, ok := ctx.Value(loop.WorkerIDKey{}).(string); ok {
		worker = id
	}
	path := ""
	if p, ok := args["path"].(string); ok {
		path = p
	}
	s.mu.Lock()
	s.record = append(s.record, recordEntry{name: name, path: path, worker: worker})
	s.mu.Unlock()
	switch name {
	case "read":
		if s.readDelay > 0 {
			time.Sleep(time.Duration(s.readDelay * float64(time.Second)))
		}
		if s.failReads[path] {
			return "", &tools.ToolError{}
		}
		return "read " + path, nil
	case "write":
		return "wrote", nil
	case "grep":
		return "no matches", nil
	case "glob":
		return "[]", nil
	case "ask_user":
		if s.askHandler == nil {
			return "ask_user: not available in this context", nil
		}
		question, _ := args["question"].(string)
		var options []string
		if raw, ok := args["options"].([]any); ok {
			for _, o := range raw {
				if so, ok := o.(string); ok {
					options = append(options, so)
				}
			}
		}
		return s.askHandler(question, options), nil
	}
	return "", &tools.ToolError{}
}

// workerIDKey is gone: the parallel-batch worker id rides in the context
// under loop.WorkerIDKey (Go goroutines have no names like Python's pool
// threads).

// countingRegistry is the real ToolRegistry wired to a real ToolCache,
// counting read handler calls (a cache hit returns without the handler).
type countingRegistry struct {
	*tools.Registry
	cachePath string
}

func newCountingRegistry(projectDir string) *countingRegistry {
	cachePath := filepath.Join(projectDir, ".kaal", "tool-cache.json")
	return &countingRegistry{
		Registry:  tools.NewRegistry(projectDir, false, toolcache.NewToolCache(cachePath), nil),
		cachePath: cachePath,
	}
}

// -- helpers ------------------------------------------------------------------

type testEnv struct {
	dir       string
	mem       *memory.Memory
	sessionID string
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	return &testEnv{
		dir:       dir,
		mem:       memory.NewMemory(filepath.Join(dir, ".agent-memory")),
		sessionID: "test-loop",
	}
}

func (e *testEnv) realTools() *tools.Registry {
	return tools.NewRegistry(e.dir, false, nil, e.mem)
}

func agentLoop(t *testing.T, gw loop.Gateway, reg loop.Registry, mem *memory.Memory, sessionID string, opts ...loop.Option) *loop.AgentLoop {
	t.Helper()
	return loop.NewAgentLoop(gw, reg, mem, sessionID, opts...)
}

func eventsOf(kind loop.EventKind, events []loop.AgentEvent) []loop.AgentEvent {
	var out []loop.AgentEvent
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func joinedText(events []loop.AgentEvent) string {
	var sb strings.Builder
	for _, e := range events {
		if e.Kind == loop.EventContent {
			sb.WriteString(e.Text)
		}
	}
	return sb.String()
}

func joinedReasoning(events []loop.AgentEvent) string {
	var sb strings.Builder
	for _, e := range events {
		if e.Kind == loop.EventReasoning {
			sb.WriteString(e.Text)
		}
	}
	return sb.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func itoa(n int) string { return strconv.Itoa(n) }

func findSystem(msgs []any) string {
	for _, m := range msgs {
		if mm, ok := m.(messages.WireSystem); ok {
			return mm.Content
		}
	}
	return ""
}

// -- tests --------------------------------------------------------------------

func TestTwoTurnToolCallFlow(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		reasoningEv("Let me check the directory"),
		// Real envelopes are generation-leading: the envelope must be the
		// FIRST content after the think span, or DialectFeed reads it as a
		// prose quote of the envelope.
		contentEv(dsmlWrite),
		contentEv("I will write the file. "),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("Wrote hello.txt."), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	reg := env.realTools()
	var events []loop.AgentEvent
	l := agentLoop(t, gw, reg, env.mem, env.sessionID)
	answer, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Wrote hello.txt." {
		t.Fatalf("answer: want %q, got %q", "Wrote hello.txt.", answer)
	}
	if got := readFile(t, filepath.Join(env.dir, "hello.txt")); got != "hi" {
		t.Fatalf("hello.txt: want %q, got %q", "hi", got)
	}

	// Turn-2 wire history replays the assistant turn — reasoning verbatim,
	// tool call embedded — followed by the tool result.
	history := gw.callMsgs(1)
	var assistant *messages.WireAssistant
	var toolResult *messages.WireToolResult
	for _, m := range history {
		switch mm := m.(type) {
		case messages.WireAssistant:
			assistant = &mm
		case messages.WireToolResult:
			toolResult = &mm
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message in turn-2 wire")
	}
	if assistant.Content != "I will write the file. " {
		t.Fatalf("assistant content: %q", assistant.Content)
	}
	if assistant.Reasoning != "Let me check the directory" {
		t.Fatalf("reasoning replay: %q", assistant.Reasoning)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(assistant.ToolCalls))
	}
	callWire := assistant.ToolCalls[0]
	if callWire.Function.Name != "write" {
		t.Fatalf("call name: %s", callWire.Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(callWire.Function.Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "hello.txt" || args["content"] != "hi" {
		t.Fatalf("arguments: %v", args)
	}
	if toolResult == nil || toolResult.ToolCallID != callWire.ID {
		t.Fatalf("tool result: %+v", toolResult)
	}
	if !strings.Contains(toolResult.Content, "wrote") {
		t.Fatalf("tool content: %q", toolResult.Content)
	}

	// No DSML markers leak into visible content or the final answer.
	content := joinedText(events)
	if strings.Contains(content, "DSML") || strings.Contains(content, fw) {
		t.Fatalf("DSML leaked into content: %q", content)
	}
	if strings.Contains(answer, "DSML") || strings.Contains(answer, fw) {
		t.Fatalf("DSML leaked into answer: %q", answer)
	}

	// Ordered emit sequence (reasoning and step events filtered out).
	var kinds []string
	for _, e := range events {
		switch e.Kind {
		case loop.EventReasoning, loop.EventStep:
			continue
		case loop.EventContent:
			kinds = append(kinds, "content")
		case loop.EventToolStart:
			kinds = append(kinds, "tool_start")
		case loop.EventToolResult:
			kinds = append(kinds, "tool_result")
		case loop.EventDone:
			kinds = append(kinds, "done")
		}
	}
	want := []string{"content", "tool_start", "tool_result", "content", "done"}
	if len(kinds) != len(want) {
		t.Fatalf("emit kinds: want %v, got %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("emit kinds: want %v, got %v", want, kinds)
		}
	}
}

func TestThinkSpanIsReasoning(t *testing.T) {
	gw := newFakeGateway([]gateway.StreamEvent{contentEv("<think>hmm</think>answer"), doneEv("stop")})
	env := setup(t)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("Think", func(e loop.AgentEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if answer != "answer" {
		t.Fatalf("answer: %q", answer)
	}
	if got := joinedReasoning(events); got != "hmm" {
		t.Fatalf("reasoning: %q", got)
	}
	if strings.Contains(joinedText(events), "think") {
		t.Fatal("think leaked into content")
	}
}

func TestAgentPersonaInjectedIntoSystemPrompt(t *testing.T) {
	gw := newFakeGateway([]gateway.StreamEvent{contentEv("ok"), doneEv("stop")})
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID,
		loop.WithAgent(&prompts.Agent{Name: "Arjuna", Description: "precise"}))
	if _, err := l.Run("hi", nil); err != nil {
		t.Fatal(err)
	}
	system := findSystem(gw.callMsgs(0))
	if !strings.Contains(system, "Arjuna") || !strings.Contains(system, "precise") {
		t.Fatalf("persona missing from system prompt")
	}
}

func TestNoAgentNoPersona(t *testing.T) {
	gw := newFakeGateway([]gateway.StreamEvent{contentEv("ok"), doneEv("stop")})
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	if _, err := l.Run("hi", nil); err != nil {
		t.Fatal(err)
	}
	system := findSystem(gw.callMsgs(0))
	if strings.Contains(system, "Arjuna") || strings.Contains(system, "## Agent") {
		t.Fatalf("persona leaked without agent")
	}
}

func TestToolLoopAbort(t *testing.T) {
	script := []gateway.StreamEvent{contentEv(dsmlWrite), doneEv("tool_calls")}
	gw := newFakeGateway(script, script, script)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	_, err := l.Run("Loop", nil)
	if err == nil || !strings.Contains(err.Error(), "tool loop detected") {
		t.Fatalf("want tool loop detected, got %v", err)
	}
}

func TestMaxSteps(t *testing.T) {
	script := []gateway.StreamEvent{contentEv(dsmlWrite), doneEv("tool_calls")}
	gw := newFakeGateway(script, script, script)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID, loop.WithMaxSteps(2))
	_, err := l.Run("Loop", nil)
	if err == nil || !strings.Contains(err.Error(), "max steps reached") {
		t.Fatalf("want max steps reached, got %v", err)
	}
}

func TestOverflowRetryTruncatesAndRetries(t *testing.T) {
	gw := newFakeGateway(
		[]gateway.StreamEvent{doneEv("length")},
		[]gateway.StreamEvent{contentEv("ok"), doneEv("stop")},
	)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("Overflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "ok" {
		t.Fatalf("answer: %q", answer)
	}
	if gw.callCount() != 2 {
		t.Fatalf("want 2 stream calls, got %d", gw.callCount())
	}
}

func TestSessionsRoundTrip(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		reasoningEv("Let me check the directory"),
		contentEv(dsmlWrite),
		contentEv("I will write the file. "),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("Wrote hello.txt."), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	if _, err := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID).Run("Write the file", nil); err != nil {
		t.Fatal(err)
	}

	replay := sessions.LoadMessages(env.sessionID)
	if len(replay) == 0 || replay[0]["role"] != "user" {
		t.Fatalf("replay[0]: %v", replay)
	}
	var assistant map[string]any
	for _, m := range replay {
		if m["role"] == "assistant" {
			assistant = m
			break // the FIRST assistant turn (turn 2's has no reasoning)
		}
	}
	if assistant == nil || assistant["reasoning_content"] != "Let me check the directory" {
		t.Fatalf("assistant reasoning missing: %v", assistant)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	call0 := calls[0].(map[string]any)
	if call0["name"] != "write" {
		t.Fatalf("call name: %v", call0["name"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call0["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "hello.txt" || args["content"] != "hi" {
		t.Fatalf("arguments: %v", args)
	}
	foundTool := false
	for _, m := range replay {
		if m["role"] == "tool" {
			foundTool = true
			if m["tool_call_id"] != call0["id"] {
				t.Fatalf("tool_call_id: %v", m["tool_call_id"])
			}
			if !strings.Contains(m["content"].(string), "wrote") {
				t.Fatalf("tool content: %v", m["content"])
			}
		}
	}
	if !foundTool {
		t.Fatal("no tool message in replay")
	}
}

func TestStructureCacheRefreshedAfterTools(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		reasoningEv("Let me check the directory"),
		contentEv(dsmlWrite),
		contentEv("I will write the file. "),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("Wrote hello.txt."), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	if _, err := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID).Run("Write the file", nil); err != nil {
		t.Fatal(err)
	}
	doc := readFile(t, filepath.Join(env.dir, ".kaal", "STRUCTURE.md"))
	if !strings.Contains(doc, "hello.txt") {
		t.Fatalf("hello.txt missing from structure doc")
	}
	if !strings.Contains(doc, "<!-- sig: ") {
		t.Fatal("signature comment missing")
	}
}

func TestEmitNil(t *testing.T) {
	gw := newFakeGateway([]gateway.StreamEvent{contentEv("hi"), doneEv("stop")})
	env := setup(t)
	answer, err := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID).Run("Say hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "hi" {
		t.Fatalf("answer: %q", answer)
	}
}

func TestPreemptiveTruncationBeforeStream(t *testing.T) {
	env := setup(t)
	big := strings.Repeat("x", 700_000)
	_ = sessions.AppendEvent(env.sessionID, map[string]any{"type": "user", "data": map[string]any{"content": big}})
	_ = sessions.AppendEvent(env.sessionID, map[string]any{"type": "assistant", "data": map[string]any{"content": big}})
	_ = sessions.AppendEvent(env.sessionID, map[string]any{"type": "tool_result", "data": map[string]any{"tool_call_id": "t1", "content": big}})

	gw := newFakeGateway([]gateway.StreamEvent{contentEv("Wrote the file."), doneEv("stop")})
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID, loop.WithResume(true))
	answer, err := l.Run("finish", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Wrote the file." {
		t.Fatalf("answer: %q", answer)
	}

	recorded := gw.callMsgs(0)
	if tokens := messages.WireTokenCost(recorded); tokens > loop.PromptBudget {
		t.Fatalf("wire %d tokens exceeds budget %d", tokens, loop.PromptBudget)
	}
	jsonBytes, _ := json.Marshal(recorded)
	if strings.Contains(string(jsonBytes), big) {
		t.Fatal("huge content survived truncation")
	}
	if gw.callCount() != 1 {
		t.Fatalf("want 1 stream call, got %d", gw.callCount())
	}
	if l.Usage.InputTokens <= 0 || l.Usage.OutputTokens <= 0 {
		t.Fatalf("usage not recorded: %+v", l.Usage)
	}
}

func TestUsageAccountingTwoTurns(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		reasoningEv("Let me check the directory"),
		contentEv(dsmlWrite),
		contentEv("I will write the file. "),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("Wrote hello.txt."), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	if _, err := l.Run("Write the file", nil); err != nil {
		t.Fatal(err)
	}
	if l.Usage.InputTokens <= 0 || l.Usage.OutputTokens <= 0 {
		t.Fatalf("usage not recorded: %+v", l.Usage)
	}
	total := 0
	for i := 0; i < gw.callCount(); i++ {
		total += messages.WireTokenCost(gw.callMsgs(i))
	}
	if l.Usage.InputTokens != total {
		t.Fatalf("input tokens: loop %d, streamed %d", l.Usage.InputTokens, total)
	}
}

func TestProseEnvelopeQuoteNotTruncated(t *testing.T) {
	quoted := "Let me explain the wire format. The gateway streams the model's " +
		"output as SSE deltas, and when the model wants to call a tool it " +
		"emits an XML-style envelope `<|DSML|tool_calls>` followed by " +
		"invoke and parameter tags, which leak into the visible content " +
		"stream. Now, the important part: when the model quotes that " +
		"envelope in prose, the healer must not mistake the quote for a " +
		"real tool call, because doing so swallows everything that " +
		"follows and the answer sticks on half — cut off right where the " +
		"model explained the format. The rest of this answer must be " +
		"preserved completely, with every single word intact."
	gw := newFakeGateway([]gateway.StreamEvent{contentEv(quoted), doneEv("stop")})
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("Explain the wire format", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(answer, "with every single word intact.") {
		t.Fatalf("answer truncated: %q", answer)
	}
	if strings.Contains(answer, "DSML") || strings.Contains(answer, fw) {
		t.Fatalf("DSML leaked: %q", answer)
	}
	if gw.callCount() != 1 {
		t.Fatalf("want 1 stream call, got %d", gw.callCount())
	}
	if _, err := os.Stat(filepath.Join(env.dir, "hello.txt")); err == nil {
		t.Fatal("phantom write executed")
	}

	// Variant 2: a COMPLETE envelope quoted inside prose — the old bug
	// healed a phantom write call and executed it; now nothing runs.
	complete := "The write tool writes files; its envelope looks like this: " +
		"<|DSML|tool_calls>" +
		`<|DSML|invoke name="write">` +
		`<|DSML|parameter name="path" string="true">hello.txt</|DSML|parameter>` +
		`<|DSML|parameter name="content" string="true">boom</|DSML|parameter>` +
		"</|DSML|invoke>" +
		"</|DSML|tool_calls>" +
		" — but that is just an example, I will not actually call it."
	gw2 := newFakeGateway([]gateway.StreamEvent{contentEv(complete), doneEv("stop")})
	env2 := setup(t)
	l2 := agentLoop(t, gw2, env2.realTools(), env2.mem, env2.sessionID+"-2")
	answer2, err := l2.Run("Show the envelope", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env2.dir, "hello.txt")); err == nil {
		t.Fatal("phantom write executed")
	}
	if !strings.Contains(answer2, "but that is just an example") {
		t.Fatalf("answer: %q", answer2)
	}
	if !strings.HasSuffix(answer2, "I will not actually call it.") {
		t.Fatalf("answer truncated: %q", answer2)
	}
	if gw2.callCount() != 1 {
		t.Fatalf("want 1 stream call, got %d", gw2.callCount())
	}
}

func TestIncrementalWireMatchesFullRebuild(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		reasoningEv("Let me check the directory"),
		contentEv(dsmlWrite),
		contentEv("I will write the file. "),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("Wrote hello.txt."), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	if _, err := l.Run("Write the file", nil); err != nil {
		t.Fatal(err)
	}
	if got := messages.WireTokenCost(l.Wire()); got != l.WireTokens() {
		t.Fatalf("wire token cache out of sync: %d vs %d", got, l.WireTokens())
	}
}

func TestParallelReadsWallTimeAndOrder(t *testing.T) {
	delay := 0.4

	singleTurn1 := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "s1", Name: "read", Arguments: `{"path": "a.txt"}`}),
		doneEv("tool_calls"),
	}
	singleTurn2 := []gateway.StreamEvent{contentEv("ok"), doneEv("stop")}
	singleGW := newFakeGateway(singleTurn1, singleTurn2)
	env := setup(t)
	singleStub := newStubRegistry(env.dir, delay)
	t0 := time.Now()
	if _, err := agentLoop(t, singleGW, singleStub, env.mem, env.sessionID+"-single").Run("one", nil); err != nil {
		t.Fatal(err)
	}
	singleTime := time.Since(t0)

	batchTurn1 := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "c1", Name: "read", Arguments: `{"path": "a.txt"}`}),
		toolCallEv(messages.ToolCall{ID: "c2", Name: "read", Arguments: `{"path": "b.txt"}`}),
		toolCallEv(messages.ToolCall{ID: "c3", Name: "read", Arguments: `{"path": "c.txt"}`}),
		doneEv("tool_calls"),
	}
	batchTurn2 := []gateway.StreamEvent{contentEv("all read"), doneEv("stop")}
	gw := newFakeGateway(batchTurn1, batchTurn2)
	stub := newStubRegistry(env.dir, delay)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, stub, env.mem, env.sessionID+"-batch")
	t0 = time.Now()
	answer, err := l.Run("read three", func(e loop.AgentEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	batchTime := time.Since(t0)

	if answer != "all read" {
		t.Fatalf("answer: %q", answer)
	}
	// Parallel: a serial 3-read batch would take ~3x the delay.
	if batchTime >= 2*singleTime {
		t.Fatalf("batch not parallel: single %v, batch %v", singleTime, batchTime)
	}

	var reads []recordEntry
	stub.mu.Lock()
	for _, r := range stub.record {
		if r.name == "read" {
			reads = append(reads, r)
		}
	}
	stub.mu.Unlock()
	// All three reads ran (execution order across goroutines is not
	// deterministic — the Python test's exact order relied on pool thread
	// startup luck; the guaranteed call order lives in events/results/
	// persistence below).
	paths := map[string]bool{}
	for _, r := range reads {
		paths[r.path] = true
	}
	for _, want := range []string{"a.txt", "b.txt", "c.txt"} {
		if !paths[want] {
			t.Fatalf("missing read %s: %+v", want, reads)
		}
	}
	workers := map[string]bool{}
	for _, r := range reads {
		workers[r.worker] = true
	}
	if len(workers) < 2 {
		t.Fatalf("parallel reads must run on distinct workers: %+v", reads)
	}

	// Events: all tool_starts first (in order), then tool_results in order.
	starts := eventsOf(loop.EventToolStart, events)
	results := eventsOf(loop.EventToolResult, events)
	if len(starts) != 3 || starts[0].Call.ID != "c1" || starts[1].Call.ID != "c2" || starts[2].Call.ID != "c3" {
		t.Fatalf("starts: %+v", starts)
	}
	if len(results) != 3 || results[0].ToolCallID != "c1" || results[1].ToolCallID != "c2" || results[2].ToolCallID != "c3" {
		t.Fatalf("results: %+v", results)
	}
	if results[0].Text != "read a.txt" || results[1].Text != "read b.txt" || results[2].Text != "read c.txt" {
		t.Fatalf("result contents: %+v", results)
	}

	// Persisted order == call order (replayable through the session store).
	replay := sessions.LoadMessages(env.sessionID + "-batch")
	var toolMsgs []map[string]any
	for _, m := range replay {
		if m["role"] == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 3 ||
		toolMsgs[0]["tool_call_id"] != "c1" || toolMsgs[1]["tool_call_id"] != "c2" || toolMsgs[2]["tool_call_id"] != "c3" {
		t.Fatalf("persisted order: %+v", toolMsgs)
	}
}

func TestMixedBatchRunsSerially(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "c1", Name: "read", Arguments: `{"path": "a.txt"}`}),
		toolCallEv(messages.ToolCall{ID: "c2", Name: "write", Arguments: `{"path": "out.txt", "content": "x"}`}),
		toolCallEv(messages.ToolCall{ID: "c3", Name: "read", Arguments: `{"path": "c.txt"}`}),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("done"), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	stub := newStubRegistry(env.dir, 0)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, stub, env.mem, env.sessionID+"-mixed")
	if _, err := l.Run("mix", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}

	stub.mu.Lock()
	record := append([]recordEntry(nil), stub.record...)
	stub.mu.Unlock()
	if len(record) != 3 || record[0].name != "read" || record[1].name != "write" || record[2].name != "read" {
		t.Fatalf("execution order: %+v", record)
	}
	workers := map[string]bool{}
	for _, r := range record {
		workers[r.worker] = true
	}
	if len(workers) != 1 {
		t.Fatalf("serial batch must run on one worker: %+v", record)
	}
	// Emit interleaving preserved: start/result alternate per call.
	var filtered []string
	for _, e := range events {
		switch e.Kind {
		case loop.EventToolStart:
			filtered = append(filtered, "start:"+e.Call.ID)
		case loop.EventToolResult:
			filtered = append(filtered, "result:"+e.ToolCallID)
		}
	}
	want := []string{"start:c1", "result:c1", "start:c2", "result:c2", "start:c3", "result:c3"}
	if len(filtered) != len(want) {
		t.Fatalf("emit: want %v, got %v", want, filtered)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Fatalf("emit: want %v, got %v", want, filtered)
		}
	}
}

func TestConsecutiveFailureAbortAcrossParallelBatch(t *testing.T) {
	turn1 := []gateway.StreamEvent{}
	for i := 0; i < 5; i++ {
		turn1 = append(turn1, toolCallEv(messages.ToolCall{
			ID:        "f" + itoa(i),
			Name:      "read",
			Arguments: `{"path": "missing` + itoa(i) + `.txt"}`,
		}))
	}
	turn1 = append(turn1, doneEv("tool_calls"))
	gw := newFakeGateway(turn1)
	env := setup(t)
	stub := newStubRegistry(env.dir, 0, "missing0.txt", "missing1.txt", "missing2.txt", "missing3.txt", "missing4.txt")
	var events []loop.AgentEvent
	l := agentLoop(t, gw, stub, env.mem, env.sessionID+"-fail")
	_, err := l.Run("fail", func(e loop.AgentEvent) { events = append(events, e) })
	if err == nil || !strings.Contains(err.Error(), "5 consecutive tool failures") {
		t.Fatalf("want 5 consecutive tool failures, got %v", err)
	}
	starts := eventsOf(loop.EventToolStart, events)
	if len(starts) != 5 || starts[0].Call.ID != "f0" || starts[4].Call.ID != "f4" {
		t.Fatalf("starts: %+v", starts)
	}
	results := eventsOf(loop.EventToolResult, events)
	if len(results) != 4 || results[0].ToolCallID != "f0" || results[3].ToolCallID != "f3" {
		t.Fatalf("results: %+v", results)
	}
}

func TestToolCacheServesRepeatReadsAndDropsAfterWrite(t *testing.T) {
	turn1 := []gateway.StreamEvent{toolCallEv(messages.ToolCall{ID: "r1", Name: "read", Arguments: `{"path": "a.txt"}`}), doneEv("tool_calls")}
	turn2 := []gateway.StreamEvent{toolCallEv(messages.ToolCall{ID: "r2", Name: "read", Arguments: `{"path": "a.txt"}`}), doneEv("tool_calls")}
	turn3 := []gateway.StreamEvent{toolCallEv(messages.ToolCall{ID: "w1", Name: "write", Arguments: `{"path": "out.txt", "content": "x"}`}), doneEv("tool_calls")}
	turn4 := []gateway.StreamEvent{toolCallEv(messages.ToolCall{ID: "r3", Name: "read", Arguments: `{"path": "a.txt"}`}), doneEv("tool_calls")}
	turn5 := []gateway.StreamEvent{contentEv("done"), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2, turn3, turn4, turn5)
	env := setup(t)
	stub := newCountingRegistry(env.dir)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, stub, env.mem, env.sessionID+"-cache")
	if _, err := l.Run("cache test", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}

	// Steps 1 and 2 read the same file with no mutation between: the
	// handler ran ONCE (step 2 was a cache hit), results identical. Step 3
	// wrote -> step 4 missed and ran the handler again.
	if got := stub.ReadCalls(); got != 2 {
		t.Fatalf("read calls: want 2, got %d", got)
	}
	results := eventsOf(loop.EventToolResult, events)
	if len(results) < 4 {
		t.Fatalf("want >=4 results, got %d", len(results))
	}
	if results[0].Text != results[1].Text {
		t.Fatal("step-2 read must be served from cache")
	}
	if results[3].Text != results[0].Text {
		t.Fatal("post-write read must re-run the handler")
	}
	if got := readFile(t, filepath.Join(env.dir, "out.txt")); got != "x" {
		t.Fatalf("out.txt: %q", got)
	}
}

func TestSameStepWriteBypassesCacheLookups(t *testing.T) {
	turn1 := []gateway.StreamEvent{toolCallEv(messages.ToolCall{ID: "r1", Name: "read", Arguments: `{"path": "x.txt"}`}), doneEv("tool_calls")}
	turn2 := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "r2", Name: "read", Arguments: `{"path": "x.txt"}`}),
		toolCallEv(messages.ToolCall{ID: "w1", Name: "write", Arguments: `{"path": "y.txt", "content": "y"}`}),
		toolCallEv(messages.ToolCall{ID: "r3", Name: "read", Arguments: `{"path": "x.txt"}`}),
		doneEv("tool_calls"),
	}
	turn3 := []gateway.StreamEvent{contentEv("done"), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2, turn3)
	env := setup(t)
	stub := newCountingRegistry(env.dir)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, stub, env.mem, env.sessionID+"-bypass")
	if _, err := l.Run("mix", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}

	// Step 1 read + the two same-batch reads = 3 handler runs; a cache
	// lookup for r2/r3 would have left read_calls at 1.
	if got := stub.ReadCalls(); got != 3 {
		t.Fatalf("read calls: want 3, got %d", got)
	}
	if got := readFile(t, filepath.Join(env.dir, "y.txt")); got != "y" {
		t.Fatalf("y.txt: %q", got)
	}
	results := eventsOf(loop.EventToolResult, events)
	if len(results) < 4 || results[1].Text != results[3].Text {
		t.Fatalf("same-batch reads must both run the handler: %+v", results)
	}
}

func TestToolCacheDisabledWithoutSignature(t *testing.T) {
	env := setup(t)
	stub := newCountingRegistry(env.dir)
	reg := stub.Registry
	_, _ = reg.Execute(context.Background(), "read", map[string]any{"path": "a.txt"})
	_, _ = reg.Execute(context.Background(), "read", map[string]any{"path": "a.txt"})
	if got := stub.ReadCalls(); got != 2 {
		t.Fatalf("read calls: want 2 (never cached), got %d", got)
	}
	if _, err := os.Stat(stub.cachePath); err == nil {
		t.Fatal("cache file must not be created without a signature")
	}
}

// -- verify hooks -------------------------------------------------------------

func writeHooks(t *testing.T, dir string, verifyCmd any) {
	t.Helper()
	hooksPath := filepath.Join(dir, ".kaal", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"verify": verifyCmd})
	if err := os.WriteFile(hooksPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTurnScripts() [][]gateway.StreamEvent {
	return [][]gateway.StreamEvent{
		{reasoningEv("Let me check"), contentEv(dsmlWrite), doneEv("tool_calls")},
		{contentEv("Wrote hello.txt."), doneEv("stop")},
	}
}

func TestVerifyHookRunsAfterMutation(t *testing.T) {
	env := setup(t)
	writeHooks(t, env.dir, []any{"python", "-c", "print('verify-ok')"})
	gw := newFakeGateway(writeTurnScripts()...)
	var events []loop.AgentEvent
	sid := env.sessionID + "-verify"
	l := agentLoop(t, gw, env.realTools(), env.mem, sid)
	answer, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Wrote hello.txt." {
		t.Fatalf("answer: %q", answer)
	}

	verifyEvents := eventsOf(loop.EventVerify, events)
	if len(verifyEvents) != 1 || !strings.Contains(verifyEvents[0].Text, "verify-ok") {
		t.Fatalf("verify events: %+v", verifyEvents)
	}

	// Persisted as a user event carrying the [verify] note.
	persisted := sessions.ReadEvents(sid)
	foundVerify := false
	for _, r := range persisted {
		if r["type"] == "user" {
			if data, ok := r["data"].(map[string]any); ok {
				if content, ok := data["content"].(string); ok && strings.Contains(content, "[verify]") {
					foundVerify = true
					if !strings.Contains(content, "verify-ok") {
						t.Fatalf("verify note missing output: %q", content)
					}
				}
			}
		}
	}
	if !foundVerify {
		t.Fatal("no [verify] user event persisted")
	}

	// The next stream's wire carries the verify note as the LAST user
	// message (the truncation-protected position).
	wire := gw.callMsgs(gw.callCount() - 1)
	var lastUser string
	for _, m := range wire {
		if mm, ok := m.(messages.WireUser); ok {
			lastUser = mm.Content
		}
	}
	if !strings.Contains(lastUser, "[verify]") || !strings.Contains(lastUser, "verify-ok") {
		t.Fatalf("wire last user missing verify note: %q", lastUser)
	}
}

func TestNoHooksFileNoVerify(t *testing.T) {
	gw := newFakeGateway(writeTurnScripts()...)
	env := setup(t)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID+"-noverify")
	if _, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(eventsOf(loop.EventVerify, events)) != 0 {
		t.Fatal("verify event without hooks file")
	}
}

func TestInvalidHooksJSONDisablesVerify(t *testing.T) {
	env := setup(t)
	hooksPath := filepath.Join(env.dir, ".kaal", "hooks.json")
	_ = os.MkdirAll(filepath.Dir(hooksPath), 0o755)
	_ = os.WriteFile(hooksPath, []byte("{not json"), 0o644)
	gw := newFakeGateway(writeTurnScripts()...)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID+"-badhooks")
	if _, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(eventsOf(loop.EventVerify, events)) != 0 {
		t.Fatal("verify event with invalid hooks JSON")
	}
}

func TestEmptyVerifyArrayDisablesVerify(t *testing.T) {
	env := setup(t)
	writeHooks(t, env.dir, []any{})
	gw := newFakeGateway(writeTurnScripts()...)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID+"-emptyhooks")
	if _, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(eventsOf(loop.EventVerify, events)) != 0 {
		t.Fatal("verify event with empty hooks array")
	}
}

func TestEnableVerifyFalseForcesOff(t *testing.T) {
	env := setup(t)
	writeHooks(t, env.dir, []any{"python", "-c", "print('verify-ok')"})
	gw := newFakeGateway(writeTurnScripts()...)
	var events []loop.AgentEvent
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID+"-noverifyflag", loop.WithEnableVerify(false))
	if _, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(eventsOf(loop.EventVerify, events)) != 0 {
		t.Fatal("verify event with enable_verify=false")
	}
}

func TestVerifyDoesNotAbortOnFailure(t *testing.T) {
	env := setup(t)
	writeHooks(t, env.dir, []any{"python", "-c", "import sys; sys.exit(1)"})
	gw := newFakeGateway(writeTurnScripts()...)
	var events []loop.AgentEvent
	sid := env.sessionID + "-verifyfail"
	l := agentLoop(t, gw, env.realTools(), env.mem, sid)
	answer, err := l.Run("Write the file", func(e loop.AgentEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Wrote hello.txt." {
		t.Fatalf("answer: %q", answer)
	}
	// Exit code 1 with empty output still produced the note.
	persisted := sessions.ReadEvents(sid)
	foundVerify := false
	for _, r := range persisted {
		if r["type"] == "user" {
			if data, ok := r["data"].(map[string]any); ok {
				if content, ok := data["content"].(string); ok && strings.Contains(content, "[verify]") {
					foundVerify = true
				}
			}
		}
	}
	if !foundVerify {
		t.Fatal("no [verify] note persisted on failing verify")
	}
}

// -- spawn_agent ----------------------------------------------------------------

func TestSpawnAgentNestedLoop(t *testing.T) {
	parentSpawn := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "sp1", Name: "spawn_agent", Arguments: `{"task": "nested task"}`}),
		doneEv("tool_calls"),
	}
	nestedAnswer := []gateway.StreamEvent{contentEv("nested answer"), doneEv("stop")}
	parentFinal := []gateway.StreamEvent{contentEv("parent final"), doneEv("stop")}
	gw := newFakeGateway(parentSpawn, nestedAnswer, parentFinal)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("run spawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "parent final" {
		t.Fatalf("answer: %q", answer)
	}
	// Three stream() calls total: parent turn 1, nested turn, parent turn 2.
	if gw.callCount() != 3 {
		t.Fatalf("want 3 stream calls, got %d", gw.callCount())
	}

	// The spawn tool result persisted in the parent session is the JSON
	// summary {answer, steps, usage, session_id}.
	parentEvents := sessions.ReadEvents(env.sessionID)
	var summary map[string]any
	for _, r := range parentEvents {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				content, _ := data["content"].(string)
				if strings.Contains(content, "session_id") {
					_ = json.Unmarshal([]byte(content), &summary)
				}
			}
		}
	}
	if summary == nil {
		t.Fatal("no spawn summary persisted")
	}
	if summary["answer"] != "nested answer" {
		t.Fatalf("summary answer: %v", summary["answer"])
	}
	nestedSid, _ := summary["session_id"].(string)
	if nestedSid == "" {
		t.Fatal("no session_id in summary")
	}
	steps, _ := summary["steps"].(float64)
	if steps <= 0 {
		t.Fatalf("steps: %v", summary["steps"])
	}
	if _, ok := summary["usage"]; !ok {
		t.Fatal("no usage in summary")
	}
	// The nested session was persisted and replays the nested conversation.
	nestedEvents := sessions.ReadEvents(nestedSid)
	if len(nestedEvents) == 0 {
		t.Fatal("nested session not persisted")
	}
	if nestedEvents[0]["type"] != "meta" {
		t.Fatalf("nested first event: %v", nestedEvents[0]["type"])
	}
	replay := sessions.LoadMessages(nestedSid)
	if len(replay) == 0 || !strings.Contains(replay[0]["content"].(string), "nested task") {
		t.Fatalf("nested replay: %v", replay)
	}
}

func TestSpawnAgentRecursionLimit(t *testing.T) {
	parentSpawn := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "sp1", Name: "spawn_agent", Arguments: `{"task": "outer"}`}),
		doneEv("tool_calls"),
	}
	nestedSpawn := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "sp2", Name: "spawn_agent", Arguments: `{"task": "inner"}`}),
		doneEv("tool_calls"),
	}
	nestedFinal := []gateway.StreamEvent{contentEv("nested done"), doneEv("stop")}
	parentFinal := []gateway.StreamEvent{contentEv("parent done"), doneEv("stop")}
	gw := newFakeGateway(parentSpawn, nestedSpawn, nestedFinal, parentFinal)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("spawn twice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "parent done" {
		t.Fatalf("answer: %q", answer)
	}
	// No third loop ran: exactly these four stream() calls.
	if gw.callCount() != 4 {
		t.Fatalf("want 4 stream calls, got %d", gw.callCount())
	}
	if gw.remainingScripts() != 0 {
		t.Fatalf("unconsumed scripts: %d", gw.remainingScripts())
	}

	parentEvents := sessions.ReadEvents(env.sessionID)
	var nestedSid string
	for _, r := range parentEvents {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				content, _ := data["content"].(string)
				if strings.Contains(content, "session_id") {
					var summary map[string]any
					_ = json.Unmarshal([]byte(content), &summary)
					nestedSid, _ = summary["session_id"].(string)
				}
			}
		}
	}
	if nestedSid == "" {
		t.Fatal("no nested session id")
	}
	// The nested loop's own spawn_agent tool result is the limit string.
	nestedEvents := sessions.ReadEvents(nestedSid)
	foundLimit := false
	for _, r := range nestedEvents {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				if content, _ := data["content"].(string); content == "spawn_agent: recursion limit reached" {
					foundLimit = true
				}
			}
		}
	}
	if !foundLimit {
		t.Fatal("nested spawn limit string not persisted")
	}
}

func TestSpawnAgentDirEscapeBlocked(t *testing.T) {
	spawnCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "sp1", Name: "spawn_agent", Arguments: `{"task": "t", "dir": "../evil"}`}),
		doneEv("tool_calls"),
	}
	final := []gateway.StreamEvent{contentEv("done"), doneEv("stop")}
	gw := newFakeGateway(spawnCall, final)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("spawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("answer: %q", answer)
	}
	if gw.callCount() != 2 { // no nested loop ran
		t.Fatalf("want 2 stream calls, got %d", gw.callCount())
	}
	results := sessions.ReadEvents(env.sessionID)
	for _, r := range results {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				content, _ := data["content"].(string)
				if !strings.HasPrefix(content, "spawn_agent: blocked:") {
					t.Fatalf("want blocked prefix, got %q", content)
				}
			}
		}
	}
}

func TestSpawnAgentDirNotADirectory(t *testing.T) {
	env := setup(t)
	_ = os.WriteFile(filepath.Join(env.dir, "plain.txt"), []byte("x"), 0o644)
	spawnCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "sp1", Name: "spawn_agent", Arguments: `{"task": "t", "dir": "plain.txt"}`}),
		doneEv("tool_calls"),
	}
	final := []gateway.StreamEvent{contentEv("done"), doneEv("stop")}
	gw := newFakeGateway(spawnCall, final)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	if _, err := l.Run("spawn", nil); err != nil {
		t.Fatal(err)
	}
	results := sessions.ReadEvents(env.sessionID)
	for _, r := range results {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				content, _ := data["content"].(string)
				if !strings.HasPrefix(content, "spawn_agent: not a directory:") {
					t.Fatalf("want not-a-directory prefix, got %q", content)
				}
			}
		}
	}
}

// -- ask_user ------------------------------------------------------------------

func TestAskUserFlow(t *testing.T) {
	askCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "a1", Name: "ask_user", Arguments: `{"question": "Proceed?", "options": ["yes", "no"]}`}),
		doneEv("tool_calls"),
	}
	final := []gateway.StreamEvent{contentEv("Proceeding with your answer."), doneEv("stop")}
	gw := newFakeGateway(askCall, final)
	env := setup(t)
	var calls [][2]any
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID,
		loop.WithAskHandler(func(question string, options []string) string {
			calls = append(calls, [2]any{question, options})
			return "yes"
		}))
	var events []loop.AgentEvent
	answer, err := l.Run("ask", func(e loop.AgentEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Proceeding with your answer." {
		t.Fatalf("answer: %q", answer)
	}
	if len(calls) != 1 || calls[0][0] != "Proceed?" {
		t.Fatalf("handler calls: %+v", calls)
	}
	if opts, ok := calls[0][1].([]string); !ok || len(opts) != 2 || opts[0] != "yes" || opts[1] != "no" {
		t.Fatalf("handler options: %+v", calls[0][1])
	}
	results := eventsOf(loop.EventToolResult, events)
	if len(results) != 1 || results[0].Text != "yes" {
		t.Fatalf("tool results: %+v", results)
	}
	// Turn 2's wire carries the tool result (the model sees it).
	history := gw.callMsgs(1)
	foundTool := false
	for _, m := range history {
		if mm, ok := m.(messages.WireToolResult); ok {
			foundTool = true
			if mm.ToolCallID != "a1" || mm.Content != "yes" {
				t.Fatalf("tool wire: %+v", mm)
			}
		}
	}
	if !foundTool {
		t.Fatal("no tool result in turn-2 wire")
	}
}

func TestAskUserInBatchRunsSerially(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "c1", Name: "read", Arguments: `{"path": "a.txt"}`}),
		toolCallEv(messages.ToolCall{ID: "c2", Name: "ask_user", Arguments: `{"question": "Go?"}`}),
		toolCallEv(messages.ToolCall{ID: "c3", Name: "read", Arguments: `{"path": "c.txt"}`}),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("done"), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	stub := newStubRegistry(env.dir, 0)
	var asked []string
	l := agentLoop(t, gw, stub, env.mem, env.sessionID+"-askserial",
		loop.WithAskHandler(func(question string, options []string) string {
			asked = append(asked, question)
			return "yes"
		}))
	var events []loop.AgentEvent
	if _, err := l.Run("ask batch", func(e loop.AgentEvent) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	record := append([]recordEntry(nil), stub.record...)
	stub.mu.Unlock()
	if len(record) != 3 || record[0].name != "read" || record[1].name != "ask_user" || record[2].name != "read" {
		t.Fatalf("execution order: %+v", record)
	}
	workers := map[string]bool{}
	for _, r := range record {
		workers[r.worker] = true
	}
	if len(workers) != 1 {
		t.Fatalf("ask batch must run on one worker: %+v", record)
	}
	if len(asked) != 1 || asked[0] != "Go?" {
		t.Fatalf("asked: %v", asked)
	}
	var filtered []string
	for _, e := range events {
		switch e.Kind {
		case loop.EventToolStart:
			filtered = append(filtered, "start:"+e.Call.ID)
		case loop.EventToolResult:
			filtered = append(filtered, "result:"+e.ToolCallID)
		}
	}
	want := []string{"start:c1", "result:c1", "start:c2", "result:c2", "start:c3", "result:c3"}
	if len(filtered) != len(want) {
		t.Fatalf("emit: want %v, got %v", want, filtered)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Fatalf("emit: want %v, got %v", want, filtered)
		}
	}
}

// -- spawn_parallel_task ---------------------------------------------------------

func TestSpawnParallelTaskTwoNestedLoops(t *testing.T) {
	delay := 0.4

	// Baseline: ONE nested spawn (serial path) with the same delay.
	singleCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "s1", Name: "spawn_agent", Arguments: `{"task": "single"}`}),
		doneEv("tool_calls"),
	}
	singleNested := []gateway.StreamEvent{sleepEvent(delay), contentEv("nested"), doneEv("stop")}
	singleFinal := []gateway.StreamEvent{contentEv("single done"), doneEv("stop")}
	singleGW := newFakeGateway(singleCall, singleNested, singleFinal)
	env := setup(t)
	t0 := time.Now()
	if _, err := agentLoop(t, singleGW, env.realTools(), env.mem, env.sessionID+"-single").Run("single", nil); err != nil {
		t.Fatal(err)
	}
	singleTime := time.Since(t0)

	parentCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "p1", Name: "spawn_parallel_task", Arguments: `{"tasks": [{"task": "task one"}, {"task": "task two"}]}`}),
		doneEv("tool_calls"),
	}
	nestedOne := []gateway.StreamEvent{sleepEvent(delay), contentEv("nested one"), doneEv("stop")}
	nestedTwo := []gateway.StreamEvent{sleepEvent(delay), contentEv("nested two"), doneEv("stop")}
	parentFinal := []gateway.StreamEvent{contentEv("parent done"), doneEv("stop")}
	gw := newFakeGateway(parentCall, nestedOne, nestedTwo, parentFinal)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	t0 = time.Now()
	answer, err := l.Run("parallel spawn", nil)
	if err != nil {
		t.Fatal(err)
	}
	parallelTime := time.Since(t0)
	if answer != "parent done" {
		t.Fatalf("answer: %q", answer)
	}
	// Parallel: a serial 2-spawn run would take ~2x the single-spawn time.
	if parallelTime >= time.Duration(1.8*float64(singleTime)) {
		t.Fatalf("spawns not parallel: single %v, parallel %v", singleTime, parallelTime)
	}

	parentEvents := sessions.ReadEvents(env.sessionID)
	var records []any
	for _, r := range parentEvents {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				content, _ := data["content"].(string)
				var parsed []any
				if json.Unmarshal([]byte(content), &parsed) == nil {
					records = parsed
				}
			}
		}
	}
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d", len(records))
	}
	// Records are in the ORIGINAL index order.
	for i, r := range records {
		rec := r.(map[string]any)
		if int(rec["index"].(float64)) != i {
			t.Fatalf("record order: %v", records)
		}
	}
	answers := []string{}
	for _, r := range records {
		rec := r.(map[string]any)
		if _, hasErr := rec["error"]; hasErr {
			t.Fatalf("unexpected error record: %v", rec)
		}
		if steps, ok := rec["steps"].(float64); !ok || steps <= 0 {
			t.Fatalf("steps: %v", rec)
		}
		if _, ok := rec["usage"]; !ok {
			t.Fatalf("usage missing: %v", rec)
		}
		sid, _ := rec["session_id"].(string)
		if sid == "" || len(sessions.ReadEvents(sid)) == 0 {
			t.Fatalf("nested session not persisted: %v", rec)
		}
		answers = append(answers, rec["answer"].(string))
	}
	sortStrings := func(ss []string) {
		for i := 0; i < len(ss); i++ {
			for j := i + 1; j < len(ss); j++ {
				if ss[j] < ss[i] {
					ss[i], ss[j] = ss[j], ss[i]
				}
			}
		}
	}
	sortStrings(answers)
	if answers[0] != "nested one" || answers[1] != "nested two" {
		t.Fatalf("answers: %v", answers)
	}
}

func TestSpawnParallelTaskRecursionLimit(t *testing.T) {
	parentCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "p1", Name: "spawn_parallel_task", Arguments: `{"tasks": [{"task": "outer"}]}`}),
		doneEv("tool_calls"),
	}
	nestedCall := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "p2", Name: "spawn_parallel_task", Arguments: `{"tasks": [{"task": "inner"}]}`}),
		doneEv("tool_calls"),
	}
	nestedFinal := []gateway.StreamEvent{contentEv("nested done"), doneEv("stop")}
	parentFinal := []gateway.StreamEvent{contentEv("parent done"), doneEv("stop")}
	gw := newFakeGateway(parentCall, nestedCall, nestedFinal, parentFinal)
	env := setup(t)
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID)
	answer, err := l.Run("parallel twice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "parent done" {
		t.Fatalf("answer: %q", answer)
	}
	if gw.callCount() != 4 { // no third loop ran
		t.Fatalf("want 4 stream calls, got %d", gw.callCount())
	}
	if gw.remainingScripts() != 0 {
		t.Fatalf("unconsumed scripts: %d", gw.remainingScripts())
	}

	parentEvents := sessions.ReadEvents(env.sessionID)
	var nestedSid string
	for _, r := range parentEvents {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				content, _ := data["content"].(string)
				var records []any
				if json.Unmarshal([]byte(content), &records) == nil && len(records) == 1 {
					rec := records[0].(map[string]any)
					if rec["answer"] != "nested done" {
						t.Fatalf("record answer: %v", rec)
					}
					nestedSid, _ = rec["session_id"].(string)
				}
			}
		}
	}
	if nestedSid == "" {
		t.Fatal("no nested session id")
	}
	// The nested loop's own spawn_parallel_task result is the limit string.
	nestedEvents := sessions.ReadEvents(nestedSid)
	foundLimit := false
	for _, r := range nestedEvents {
		if r["type"] == "tool_result" {
			if data, ok := r["data"].(map[string]any); ok {
				if content, _ := data["content"].(string); content == "spawn_parallel_task: recursion limit reached" {
					foundLimit = true
				}
			}
		}
	}
	if !foundLimit {
		t.Fatal("nested parallel limit string not persisted")
	}
}

func TestAsyncSessionWriterRoundTrip(t *testing.T) {
	turn1 := []gateway.StreamEvent{
		reasoningEv("Let me check the directory"),
		contentEv(dsmlWrite),
		contentEv("I will write the file. "),
		doneEv("tool_calls"),
	}
	turn2 := []gateway.StreamEvent{contentEv("Wrote hello.txt."), doneEv("stop")}
	gw := newFakeGateway(turn1, turn2)
	env := setup(t)
	writer := sessions.NewAsyncWriter()
	defer writer.Close()
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID, loop.WithSessionWriter(writer))
	answer, err := l.Run("Write the file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Wrote hello.txt." {
		t.Fatalf("answer: %q", answer)
	}
	// Flush-on-turn-end: the session store is durable the moment Run returns.
	replay := sessions.LoadMessages(env.sessionID)
	if len(replay) == 0 {
		t.Fatal("session not persisted through the async writer")
	}
	foundAssistant := false
	for _, m := range replay {
		if m["role"] == "assistant" && m["reasoning_content"] == "Let me check the directory" {
			foundAssistant = true
		}
	}
	if !foundAssistant {
		t.Fatalf("assistant turn missing from replay: %v", replay)
	}
}

func TestCancelDuringAskAbortsWithoutPersisting(t *testing.T) {
	// A cancelled turn with a blocked ask handler must answer
	// "(cancelled)" and abort WITHOUT persisting the partial turn (Python's
	// TurnCancelled discards the batch).
	turn1 := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "a1", Name: "ask_user", Arguments: `{"question": "Go?"}`}),
		doneEv("tool_calls"),
	}
	gw := newFakeGateway(turn1)
	env := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{})
	l := agentLoop(t, gw, env.realTools(), env.mem, env.sessionID,
		loop.WithContext(ctx),
		loop.WithAskHandler(func(question string, options []string) string {
			select {
			case <-called:
			default:
				close(called)
			}
			<-called // block until the test's second close
			return "yes"
		}))
	result := make(chan error, 1)
	go func() {
		_, err := l.Run("ask", nil)
		result <- err
	}()
	// Wait until the ask handler is blocked inside the tool execution.
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("ask handler never called")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, loop.ErrCancelled) {
			t.Fatalf("want ErrCancelled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not abort on cancel")
	}
	// The partial turn was NOT persisted (only meta + user from run start).
	replay := sessions.LoadMessages(env.sessionID)
	if len(replay) != 1 || replay[0]["role"] != "user" {
		t.Fatalf("partial turn must not persist: %v", replay)
	}
}
