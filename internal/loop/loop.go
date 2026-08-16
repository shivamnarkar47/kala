// Package loop ports harness/loop.py: the agent loop — stream, heal DSML,
// execute tools, persist.
//
// Runs one agent task end to end: builds the system prompt from memory and
// project context, streams the conversation through the gateway one turn at
// a time, heals leaked DSML envelopes back into ToolCall objects via
// dialect.DialectFeed, executes the resolved calls against the tool
// registry, and persists every turn to the JSONL session store.
//
// Loop-level failures (context overflow, max steps, tool loops, consecutive
// tool failures, gateway errors) return LoopError (exit code 2) after
// emitting a final error AgentEvent and recording a session summary.
package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ctxutil "github.com/kaal/kaal/internal/context"
	"github.com/kaal/kaal/internal/dialect"
	"github.com/kaal/kaal/internal/gateway"
	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/messages"
	"github.com/kaal/kaal/internal/prompts"
	"github.com/kaal/kaal/internal/sessions"
	"github.com/kaal/kaal/internal/structure"
	"github.com/kaal/kaal/internal/tools"
)

// PromptBudget: how much wire history is sent per request. Explicit, NOT
// derived from the window: the model catalog advertises a 1M context, but
// requests that big are slow. 128k keeps each round trip light while still
// carrying a long working session; truncation drops old turns past it.
const PromptBudget = 128_000

// ParallelReadTools: the parallel pool is strictly the read-only trio.
var parallelReadTools = map[string]bool{"read": true, "grep": true, "glob": true}

// mutators: write/edit/bash mutate the tree (structure refresh + verify).
var mutators = map[string]bool{"write": true, "edit": true, "bash": true}

// LoopError is a loop-level failure: overflow, max steps, tool loop, abort.
// Exit code 2.
type LoopError struct{ msg string }

func (e *LoopError) Error() string { return e.msg }

func loopErr(format string, args ...any) *LoopError {
	return &LoopError{msg: fmt.Sprintf(format, args...)}
}

// GatewayError is a gateway-level failure (stream error, retries exhausted,
// 4xx). Exit code 1. It propagates WITHOUT a session summary, exactly like
// Python's GatewayError escaping the loop.
type GatewayError struct{ msg string }

func (e *GatewayError) Error() string { return e.msg }

func gatewayErr(format string, args ...any) *GatewayError {
	return &GatewayError{msg: fmt.Sprintf(format, args...)}
}

// EventKind classifies one AgentEvent.
type EventKind int

const (
	EventContent EventKind = iota
	EventReasoning
	EventToolStart
	EventToolResult
	EventVerify
	EventStep
	EventDone
	EventError
)

// AgentEvent is one event emitted to the front end synchronously as it
// happens: content | reasoning | tool_start | tool_result | verify | step |
// done | error.
type AgentEvent struct {
	Kind       EventKind
	Text       string
	Call       messages.ToolCall
	ToolCallID string
	Step       int
}

// Usage accumulates input/output token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Gateway is the streaming seam the loop drives (the real gateway or a fake
// in tests).
type Gateway interface {
	Stream(ctx context.Context, msgs []any, tools []any, maxTokens int) <-chan gateway.StreamEvent
	ModelID() string
}

// Registry is the tool seam (the real registry or a stub in tests).
type Registry interface {
	Schemas() []any
	BeginBatch(names []string, sig string)
	EndBatch(mutated bool)
	SetAskHandler(handler tools.AskHandler)
	SetSpawnHandler(handler tools.SpawnHandler)
	SetSpawnManyHandler(handler tools.SpawnManyHandler)
	Execute(ctx context.Context, name string, args map[string]any) (string, error)
	ProjectDir() string
}

// ErrCancelled aborts a turn's context (the TUI's Ctrl+C hard cancel): the
// turn ends quietly — no error event, no session summary, no retry.
var ErrCancelled = errors.New("turn cancelled")

// Option configures an AgentLoop.
type Option func(*AgentLoop)

// WithMaxSteps sets the maximum agent turns (default 20).
func WithMaxSteps(n int) Option { return func(l *AgentLoop) { l.maxSteps = n } }

// WithAllowDangerous skips the destructive-command DENY list.
func WithAllowDangerous(b bool) Option { return func(l *AgentLoop) { l.allowDangerous = b } }

// WithResume continues a session.
func WithResume(b bool) Option { return func(l *AgentLoop) { l.resume = b } }

// WithEnableVerify turns the verify hooks on/off.
func WithEnableVerify(b bool) Option { return func(l *AgentLoop) { l.enableVerify = b } }

// WithSpawnDepth sets the nested-agent nesting level (top-level = 1).
func WithSpawnDepth(n int) Option { return func(l *AgentLoop) { l.spawnDepth = n } }

// WithAgent injects a persona into the system prompt (nil = none).
func WithAgent(a *prompts.Agent) Option { return func(l *AgentLoop) { l.agent = a } }

// WithAskHandler injects the ask_user handler (nil = headless stdin default).
func WithAskHandler(h func(question string, options []string) string) Option {
	return func(l *AgentLoop) { l.askHandler = h }
}

// WithStructure injects the structure manager (the TUI passes its own so
// the cache survives across turns).
func WithStructure(st *structure.StructureManager) Option {
	return func(l *AgentLoop) { l.structure = st }
}

// WithSessionWriter routes session persistence through the async writer
// (P6: flush-on-turn-end; nil keeps synchronous appends).
func WithSessionWriter(w *sessions.AsyncWriter) Option {
	return func(l *AgentLoop) { l.sessionWriter = w }
}

// WithContext sets the turn context (the TUI's hard-cancel seam: cancelling
// it aborts the in-flight SSE stream and ends the turn with ErrCancelled).
func WithContext(ctx context.Context) Option {
	return func(l *AgentLoop) { l.ctx = ctx }
}

// AgentLoop is one runnable task loop over a streaming gateway and a tool
// registry.
type AgentLoop struct {
	gateway        Gateway
	tools          Registry
	memory         *memory.Memory
	sessionID      string
	maxSteps       int
	allowDangerous bool
	resume         bool
	structure      *structure.StructureManager
	enableVerify   bool
	spawnDepth     int
	agent          *prompts.Agent
	askHandler     func(question string, options []string) string
	ctx            context.Context
	sessionWriter  *sessions.AsyncWriter

	verifyCmd []string // from .kaal/hooks.json, read once at Run() start

	messages   []messages.Message
	wire       []any // incremental wire cache (byte-identical to rebuild)
	wireTokens int
	system     *messages.SystemMessage

	consecutiveFailures int
	lastCallKey         string
	sameCallCount       int

	ran   bool
	Steps int
	Usage Usage
}

// NewAgentLoop builds one loop.
func NewAgentLoop(gw Gateway, reg Registry, mem *memory.Memory, sessionID string, opts ...Option) *AgentLoop {
	l := &AgentLoop{
		gateway: gw, tools: reg, memory: mem, sessionID: sessionID,
		maxSteps: 20, spawnDepth: 1, enableVerify: true, ctx: context.Background(),
	}
	for _, opt := range opts {
		opt(l)
	}
	// Nested-agent runners are injected here (cli builds the registry before
	// the loop); registries without the setters (stubs) get no spawn support.
	l.tools.SetSpawnHandler(l.spawn)
	l.tools.SetSpawnManyHandler(l.spawnMany)
	return l
}

// Wire exposes the incremental wire cache (parity test seam: must equal a
// full rebuild).
func (l *AgentLoop) Wire() []any { return l.wire }

// WireTokens exposes the cached wire token cost.
func (l *AgentLoop) WireTokens() int { return l.wireTokens }

// Run runs one task and returns the final answer, emitting events as they
// happen.
func (l *AgentLoop) Run(task string, emit func(AgentEvent)) (string, error) {
	if l.ran {
		return "", errors.New("run() may only be called once per AgentLoop instance")
	}
	l.ran = true
	l.Steps = 0
	l.Usage = Usage{}

	handler := l.askHandler
	if handler == nil {
		handler = defaultAsk
	}
	// Cancel-aware wrapper: a cancelled turn must not hang on a blocking
	// ask handler (the TUI modal) — it answers "(cancelled)" instead, and
	// the turn is discarded at the batch boundary below. base holds the
	// ORIGINAL handler (the closure must not capture the reassigned var).
	base := handler
	wrapped := func(question string, options []string) string {
		done := make(chan string, 1)
		go func() { done <- base(question, options) }()
		select {
		case ans := <-done:
			return ans
		case <-l.ctx.Done():
			return "(cancelled)"
		}
	}
	l.tools.SetAskHandler(wrapped)

	l.loadVerifyCmd()

	if l.structure == nil {
		l.structure = structure.NewStructureManager(l.tools.ProjectDir())
	}
	_ = l.structure.Ensure() // cache is best-effort; never break the turn

	system := messages.SystemMessage{Text: prompts.BuildSystemPrompt(
		l.memory.LoadDigest(),
		prompts.BuildProjectContext(l.tools.ProjectDir()),
		l.agent,
	)}
	l.system = &system
	l.messages = []messages.Message{system}

	if l.resume {
		for _, wire := range sessions.LoadMessages(l.sessionID) {
			if m := fromWire(wire); m != nil {
				l.messages = append(l.messages, m)
			}
		}
	}
	l.messages = append(l.messages, messages.UserMessage{Text: task})
	// Wire built once per run: system (coalesced once) + resumed history +
	// task. Kept incrementally up to date from here on.
	l.rebuildWire()

	metaData := map[string]any{"kind": "start"}
	if model := l.gateway.ModelID(); model != "" {
		metaData["model"] = model
	}
	l.appendSessionEvents(map[string]any{"type": "meta", "data": metaData})
	l.appendSessionEvents(map[string]any{"type": "user", "data": map[string]any{"content": task}})

	answer, err := l.stepLoop(emit)
	// P6 flush-on-turn-end: the async writer drains before the caller reads
	// the session store.
	if l.sessionWriter != nil {
		l.sessionWriter.Flush()
	}
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			return "", err // quiet end: the TUI's Ctrl+C, not a failure
		}
		var le *LoopError
		if errors.As(err, &le) {
			if emit != nil {
				emit(AgentEvent{Kind: EventError, Text: err.Error()})
			}
			l.memory.RecordSessionSummary(task, "error: "+err.Error())
		}
		return "", err
	}
	if emit != nil {
		emit(AgentEvent{Kind: EventDone, Text: answer})
	}
	l.memory.RecordSessionSummary(task, "ok")
	return answer, nil
}

// appendSessionEvents routes persistence through the async writer when
// configured (nil keeps the synchronous store).
func (l *AgentLoop) appendSessionEvents(events ...map[string]any) {
	if l.sessionWriter != nil {
		_ = l.sessionWriter.AppendEvents(l.sessionID, events)
		return
	}
	_ = sessions.AppendEvents(l.sessionID, events)
}

// stepLoop drives one full turn per step until an answer is produced. An
// empty content answer IS an answer (Python returns "" — not None — as the
// final answer); only a tool-executing turn continues.
func (l *AgentLoop) stepLoop(emit func(AgentEvent)) (string, error) {
	for range l.maxSteps {
		answer, done, err := l.oneStep(emit)
		if err != nil {
			return "", err
		}
		if done {
			return answer, nil
		}
	}
	return "", loopErr("max steps reached")
}

// oneStep runs one full turn: stream, heal, resolve, persist, execute.
// Returns (answer, done, err): done=true when the turn produced a final
// answer (possibly empty); done=false when tool calls were executed and the
// loop should continue with another step. An overflow retry re-runs the
// turn inside this step and must not count as a new step.
func (l *AgentLoop) oneStep(emit func(AgentEvent)) (string, bool, error) {
	l.Steps++
	if emit != nil {
		emit(AgentEvent{Kind: EventStep, Step: l.Steps})
	}
	retried := false
	var content string
	var reasoning string
	var calls []messages.ToolCall
	for { // overflow retry re-runs the turn without consuming a step
		content = ""
		reasoning = ""
		calls = nil
		var contentParts, reasoningParts []string
		var healedCalls, structuredCalls []messages.ToolCall
		finishReason := ""

		// Preemptive truncation: if the full wire history would exceed the
		// prompt budget, drop old turns BEFORE streaming. The budget check
		// is O(1) against the incremental wire cache.
		if l.wireTokens > PromptBudget {
			l.messages = ctxutil.TruncateHistory(l.messages, *l.system, PromptBudget)
			l.rebuildWire()
		}
		l.Usage.InputTokens += l.wireTokens

		feed := dialect.NewDialectFeed()
		for ev := range l.gateway.Stream(l.ctx, l.wire, l.tools.Schemas(), 0) {
			switch ev.Kind {
			case gateway.EventContent:
				for _, e := range feed.Feed(ev.Text) {
					l.route(e, &contentParts, &reasoningParts, &healedCalls, emit)
				}
			case gateway.EventReasoning:
				reasoningParts = append(reasoningParts, ev.Text)
				if emit != nil {
					emit(AgentEvent{Kind: EventReasoning, Text: ev.Text})
				}
			case gateway.EventToolCall:
				structuredCalls = append(structuredCalls, ev.ToolCall)
			case gateway.EventError:
				return "", false, gatewayErr("gateway error: %s", ev.Text)
			case gateway.EventDone:
				if ev.FinishReason != nil {
					finishReason = *ev.FinishReason
				}
			}
			if finishReason != "" {
				break
			}
		}
		for _, e := range feed.Flush() {
			l.route(e, &contentParts, &reasoningParts, &healedCalls, emit)
		}
		if l.ctx.Err() != nil {
			return "", false, ErrCancelled
		}

		if len(structuredCalls) > 0 {
			calls = structuredCalls
		} else {
			calls = healedCalls
		}
		if finishReason == "length" && len(contentParts) == 0 && len(calls) == 0 {
			if retried {
				return "", false, loopErr("context overflow: model hit max output with empty turn")
			}
			retried = true
			l.messages = ctxutil.TruncateHistory(l.messages, *l.system, PromptBudget/2)
			l.rebuildWire()
			continue
		}
		content = strings.Join(contentParts, "")
		reasoning = strings.Join(reasoningParts, "")
		break
	}

	l.Usage.OutputTokens += ctxutil.EstimateTokens(content + reasoning)
	reasoningOrNil := (*string)(nil)
	if reasoning != "" {
		reasoningOrNil = &reasoning
	}
	l.appendWire(messages.AssistantMessage{Content: content, ReasoningContent: reasoningOrNil, ToolCalls: calls})
	sessionEvents := []map[string]any{
		{
			"type": "assistant",
			"data": map[string]any{
				"content":           content,
				"reasoning_content": reasonOrNil(reasoning),
				"tool_calls":        compactCalls(calls),
			},
		},
	}

	if len(calls) == 0 {
		l.appendSessionEvents(sessionEvents...)
		return content, true, nil
	}

	sig := ""
	if l.structure != nil {
		sig = l.structure.LastSignature
	}
	l.tools.BeginBatch(callNames(calls), sig)
	var err error
	sessionEvents, err = l.executeMany(calls, emit, sessionEvents)
	if l.ctx.Err() != nil {
		// Cancelled: the partial turn's events are NOT persisted (Python's
		// TurnCancelled discards the batch).
		return "", false, ErrCancelled
	}
	if err != nil {
		// Aborted (tool loop / 5 failures): the partial batch is discarded.
		return "", false, err
	}
	l.appendSessionEvents(sessionEvents...)
	return "", false, nil
}

func reasonOrNil(reasoning string) any {
	if reasoning == "" {
		return nil
	}
	return reasoning
}

func compactCalls(calls []messages.ToolCall) any {
	if len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{"id": c.ID, "name": c.Name, "arguments": c.Arguments})
	}
	return out
}

func callNames(calls []messages.ToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return names
}

// route dispatches one DialectFeed event into the turn accumulators.
func (l *AgentLoop) route(ev dialect.Event, contentParts, reasoningParts *[]string, healedCalls *[]messages.ToolCall, emit func(AgentEvent)) {
	switch ev.Kind {
	case dialect.EventText:
		*contentParts = append(*contentParts, ev.Text)
		if emit != nil {
			emit(AgentEvent{Kind: EventContent, Text: ev.Text})
		}
	case dialect.EventReasoning:
		*reasoningParts = append(*reasoningParts, ev.Text)
		if emit != nil {
			emit(AgentEvent{Kind: EventReasoning, Text: ev.Text})
		}
	case dialect.EventToolCall:
		*healedCalls = append(*healedCalls, ev.Call)
	}
}

// rebuildWire rebuilds the incremental wire cache from scratch (after
// truncation).
func (l *AgentLoop) rebuildWire() {
	l.wire = messages.ToWireMessages(l.messages)
	l.wireTokens = messages.WireTokenCost(l.wire)
}

// appendWire appends one message to the history AND the incremental wire
// cache. Only the new message is converted to wire and token-costed; the
// system coalescing happened once in the first build.
func (l *AgentLoop) appendWire(message messages.Message) {
	l.messages = append(l.messages, message)
	wire := message.ToWire()
	l.wire = append(l.wire, wire)
	if b, err := jsonpy.Marshal(wire); err == nil {
		l.wireTokens += jsonpy.RuneCount(b) / 3
	}
}

// fromWire converts one session wire dict back into a message object (or
// nil). Assistant tool_calls may arrive in OpenAI wire form
// ({"id", "type", "function": {...}}) or the compact persisted form
// ({"id", "name", "arguments"}); both are accepted.
func fromWire(wire map[string]any) messages.Message {
	role, _ := wire["role"].(string)
	content, _ := wire["content"].(string)
	switch role {
	case "user":
		return messages.UserMessage{Text: content}
	case "assistant":
		var calls []messages.ToolCall
		if rawCalls, ok := wire["tool_calls"].([]any); ok {
			for _, rc := range rawCalls {
				tc, ok := rc.(map[string]any)
				if !ok {
					continue
				}
				name, _ := tc["name"].(string)
				arguments, _ := tc["arguments"].(string)
				if fn, ok := tc["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok {
						name = n
					}
					if a, ok := fn["arguments"].(string); ok {
						arguments = a
					}
				}
				id, _ := tc["id"].(string)
				calls = append(calls, messages.ToolCall{ID: id, Name: name, Arguments: arguments})
			}
		}
		var reasoning *string
		if rc, ok := wire["reasoning_content"].(string); ok && rc != "" {
			reasoning = &rc
		}
		if len(calls) == 0 {
			calls = nil
		}
		return messages.AssistantMessage{Content: content, ReasoningContent: reasoning, ToolCalls: calls}
	case "tool":
		toolCallID, _ := wire["tool_call_id"].(string)
		return messages.ToolResultMessage{ToolCallID: toolCallID, Content: content}
	}
	return nil
}

// -- tool execution ----------------------------------------------------------

// outcome is one tool execution result.
type outcome struct {
	result string
	args   map[string]any
	failed bool
}

// runOne executes one tool call in isolation; worker-safe, no side effects.
func (l *AgentLoop) runOne(ctx context.Context, call messages.ToolCall) outcome {
	var args map[string]any
	parsed := map[string]any{}
	if strings.TrimSpace(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &parsed); err != nil {
			return outcome{result: fmt.Sprintf("invalid tool arguments: %s", err), failed: true}
		}
	}
	args = parsed
	result, err := l.tools.Execute(ctx, call.Name, args)
	if err != nil {
		return outcome{result: err.Error(), args: args, failed: true}
	}
	return outcome{result: result, args: args}
}

// recordResult records one tool outcome on the main thread, in original call
// order: applies the defensive cap, emits the tool_result event, persists
// the tool_call + tool_result events, appends the ToolResultMessage to the
// wire cache, and maintains ALL consecutive-failure counting and tool-loop
// detection. Raises LoopError at 5 consecutive tool failures or on the 3rd
// consecutive identical (name, args) tuple.
func (l *AgentLoop) recordResult(call messages.ToolCall, res outcome, emit func(AgentEvent), sessionEvents []map[string]any) ([]map[string]any, error) {
	if utf8RuneCount(res.result) > tools.MaxResultChars {
		res.result = truncateRunes(res.result, tools.MaxResultChars) + tools.TruncatedSuffix
	}

	if res.failed {
		l.consecutiveFailures++
		if l.consecutiveFailures >= 5 {
			return sessionEvents, loopErr("5 consecutive tool failures")
		}
	} else {
		l.consecutiveFailures = 0
	}

	// Tool-loop detection: the same (name, args) tuple 3x in a row aborts.
	key := call.Name + "\x00" + marshalArgsKey(res.args)
	if key == l.lastCallKey {
		l.sameCallCount++
	} else {
		l.lastCallKey = key
		l.sameCallCount = 1
	}
	if l.sameCallCount >= 3 {
		return sessionEvents, loopErr("tool loop detected")
	}

	if emit != nil {
		emit(AgentEvent{Kind: EventToolResult, ToolCallID: call.ID, Text: res.result})
	}
	sessionEvents = append(sessionEvents,
		map[string]any{"type": "tool_call", "data": map[string]any{"id": call.ID, "name": call.Name, "arguments": call.Arguments}},
		map[string]any{"type": "tool_result", "data": map[string]any{"tool_call_id": call.ID, "content": res.result}},
	)
	l.appendWire(messages.ToolResultMessage{ToolCallID: call.ID, Content: res.result})
	return sessionEvents, nil
}

func marshalArgsKey(args map[string]any) string {
	if args == nil {
		return "nil"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprint(args)
	}
	return string(b)
}

// WorkerIDKey is the context key carrying the parallel-batch worker id
// (test seam: Go goroutines have no names like Python's pool threads).
type WorkerIDKey struct{}

// executeMany executes a tool-call batch; results recorded in original call
// order. All-read batches (read/grep/glob) of more than one call run
// concurrently (≤4 workers); every other batch runs fully serially in call
// order. tool_start events are emitted first (in order) for the parallel
// path; on both paths recordResult runs on the calling goroutine in call
// order. The structure refresh runs only when the batch mutated the tree.
func (l *AgentLoop) executeMany(calls []messages.ToolCall, emit func(AgentEvent), sessionEvents []map[string]any) ([]map[string]any, error) {
	parallel := len(calls) > 1
	if parallel {
		for _, c := range calls {
			if !parallelReadTools[c.Name] {
				parallel = false
				break
			}
		}
	}
	if parallel {
		if emit != nil {
			for _, call := range calls {
				emit(AgentEvent{Kind: EventToolStart, Call: call})
			}
		}
		results := make([]outcome, len(calls))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for i, call := range calls {
			wg.Add(1)
			go func(i int, call messages.ToolCall) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ctx := context.WithValue(context.Background(), WorkerIDKey{}, fmt.Sprintf("worker-%d", i))
				results[i] = l.runOne(ctx, call)
			}(i, call)
		}
		wg.Wait()
		for i, call := range calls {
			var err error
			if sessionEvents, err = l.recordResult(call, results[i], emit, sessionEvents); err != nil {
				return sessionEvents, err
			}
		}
	} else {
		for _, call := range calls {
			if emit != nil {
				emit(AgentEvent{Kind: EventToolStart, Call: call})
			}
			res := l.runOne(l.ctx, call)
			var err error
			if sessionEvents, err = l.recordResult(call, res, emit, sessionEvents); err != nil {
				return sessionEvents, err
			}
		}
	}

	// Only write/edit/bash mutate the tree; read/grep/glob never do, so skip
	// the full-tree signature scan for them.
	mutated := false
	for _, call := range calls {
		if mutators[call.Name] {
			mutated = true
			break
		}
	}
	if mutated {
		_ = l.structure.Refresh()
	}
	l.tools.EndBatch(mutated)
	if mutated {
		sessionEvents = l.runVerify(emit, sessionEvents)
	}
	return sessionEvents, nil
}

// -- verify hooks ------------------------------------------------------------

// loadVerifyCmd reads .kaal/hooks.json once into l.verifyCmd (nil = off).
// Explicit config only: missing file, invalid JSON, a non-list or empty
// `verify` value, or enableVerify=False all turn the feature off.
func (l *AgentLoop) loadVerifyCmd() {
	l.verifyCmd = nil
	if !l.enableVerify {
		return
	}
	raw, err := os.ReadFile(filepath.Join(l.tools.ProjectDir(), ".kaal", "hooks.json"))
	if err != nil {
		return // missing file -> feature off
	}
	var hooks map[string]any
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return // invalid JSON -> feature off
	}
	cmdVal, ok := hooks["verify"].([]any)
	if !ok || len(cmdVal) == 0 {
		return
	}
	cmd := make([]string, 0, len(cmdVal))
	for _, part := range cmdVal {
		s, ok := part.(string)
		if !ok {
			return
		}
		cmd = append(cmd, s)
	}
	l.verifyCmd = cmd
}

// runVerify runs the configured verify command after a mutating batch. The
// output is CONTENT for the model, never a loop abort: it becomes a user
// message on the wire (the last user message), persists as a user event, and
// emits a verify AgentEvent kind. Synchronous (v1); 30s worst case.
func (l *AgentLoop) runVerify(emit func(AgentEvent), sessionEvents []map[string]any) []map[string]any {
	if len(l.verifyCmd) == 0 {
		return sessionEvents
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.verifyCmd[0], l.verifyCmd[1:]...)
	cmd.Dir = l.tools.ProjectDir()
	out, err := cmd.CombinedOutput()
	content := string(out)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			content = "[verify] timed out after 30s"
		} else if _, ok := err.(*exec.Error); ok {
			content = fmt.Sprintf("[verify] failed to run: %s", err)
		}
	}
	if utf8RuneCount(content) > tools.MaxResultChars {
		content = truncateRunes(content, tools.MaxResultChars) + tools.TruncatedSuffix
	}
	message := content
	if !strings.HasPrefix(content, "[verify]") {
		message = "[verify]\n" + content
	}
	if emit != nil {
		emit(AgentEvent{Kind: EventVerify, Text: content})
	}
	sessionEvents = append(sessionEvents, map[string]any{"type": "user", "data": map[string]any{"content": message}})
	l.appendWire(messages.UserMessage{Text: message})
	return sessionEvents
}

// -- nested agents -----------------------------------------------------------

// defaultAsk is the headless ask_user handler: print the question, read a
// line from stdin. Empty input — a blank line or EOF on a closed stdin —
// becomes "(no answer)" so a non-interactive run never blocks or crashes.
func defaultAsk(question string, options []string) string {
	fmt.Printf("[ask] %s\n", question)
	if len(options) > 0 {
		for i, option := range options {
			fmt.Printf("  %d. %s\n", i+1, option)
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.TrimSpace(line)
	if err != nil || answer == "" {
		return "(no answer)"
	}
	return answer
}

// spawnSummary is the JSON summary a spawn_agent tool result carries.
type spawnSummary struct {
	Answer    string `json:"answer"`
	Steps     int    `json:"steps"`
	Usage     Usage  `json:"usage"`
	SessionID string `json:"session_id"`
}

// spawn runs a nested AgentLoop on a sub-task and returns its JSON summary.
// Guardrails FIRST: recursion is capped — a loop at spawnDepth >= 2 returns
// the limit string without creating anything. The nested run gets a FRESH
// tool registry (no tool cache), fresh Memory under the resolved dir, its
// own session id, allow_dangerous=false, enable_verify=false, max_steps <=
// 5, and a wall-clock timeout. The nested loop always runs on its own
// goroutine; emit=nil: no events bubble up.
func (l *AgentLoop) spawn(task string, dir *string, maxSteps, timeout int) string {
	if l.spawnDepth >= 2 {
		return "spawn_agent: recursion limit reached"
	}
	target := l.tools.ProjectDir()
	if dir != nil {
		resolved, err := tools.ResolveRelative(*dir, l.tools.ProjectDir())
		if err != nil {
			return "spawn_agent: " + err.Error()
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return fmt.Sprintf("spawn_agent: not a directory: %s", *dir)
		}
		target = resolved
	}
	sessionID := sessions.NewSessionID()
	mem := memory.NewMemory(filepath.Join(target, ".agent-memory"))
	nestedTools := tools.NewRegistry(target, false, nil, mem)
	nested := NewAgentLoop(
		l.gateway, // shared gateway
		nestedTools,
		mem,
		sessionID,
		WithMaxSteps(min(maxSteps, 5)),
		WithAllowDangerous(false),
		WithEnableVerify(false),
		WithSpawnDepth(l.spawnDepth+1),
		// ask_user handler is inherited: a batch worker's nested agent must
		// not block on stdin either, and a TUI modal stays a modal.
		WithAskHandler(l.askHandler),
		// no agent=: the persona is NOT inherited by nested loops.
	)
	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := nested.Run(task, nil) // emit=nil: nothing bubbles up
		done <- result{answer, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return "spawn_agent: " + res.err.Error()
		}
		summary, err := jsonpy.Marshal(spawnSummary{
			Answer:    truncateRunes(res.answer, 50_000),
			Steps:     nested.Steps,
			Usage:     nested.Usage,
			SessionID: sessionID,
		})
		if err != nil {
			return "spawn_agent: " + err.Error()
		}
		return string(summary)
	case <-time.After(time.Duration(timeout) * time.Second):
		// The orphaned goroutine keeps running (bounded by its own max_steps
		// and gateway timeouts); never block the parent on it.
		return fmt.Sprintf("spawn_agent: timed out after %ds", timeout)
	}
}

// spawnMany runs several nested AgentLoops in parallel and returns a JSON
// array. Guardrails mirror spawn: recursion is capped at spawnDepth >= 2,
// and every nested run gets a FRESH registry/memory/session on its own
// goroutine with a per-task wall timeout. Records are collected in the
// ORIGINAL index order; a failed nested run becomes {"index", "error"}.
func (l *AgentLoop) spawnMany(tasks []map[string]any, timeout int) string {
	if l.spawnDepth >= 2 {
		return "spawn_parallel_task: recursion limit reached"
	}
	records := make([]any, len(tasks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for index, task := range tasks {
		wg.Add(1)
		go func(index int, task map[string]any) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			taskText := fmt.Sprint(task["task"])
			maxSteps, _ := toInt(task["max_steps"], 5)
			maxSteps = clampInt(maxSteps, 1, 5)
			taskTimeout, _ := toInt(task["timeout"], timeout)
			taskTimeout = clampInt(taskTimeout, 1, 300)
			var dir *string
			if d, ok := task["dir"].(string); ok && d != "" {
				dir = &d
			}
			raw := l.spawn(taskText, dir, maxSteps, taskTimeout)
			var summary map[string]any
			// UseNumber keeps ints ints — a float round-trip would emit
			// "steps": 1.0 where Python emits "steps": 1.
			dec := json.NewDecoder(strings.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&summary); err != nil {
				// _spawn returned an error string (blocked dir, timeout,
				// recursion) rather than a summary JSON.
				records[index] = map[string]any{"index": index, "error": raw}
				return
			}
			record := map[string]any{"index": index}
			for k, v := range summary {
				record[k] = v
			}
			records[index] = record
		}(index, task)
	}
	wg.Wait()
	b, err := jsonpy.Marshal(records)
	if err != nil {
		return "spawn_parallel_task: " + err.Error()
	}
	return string(b)
}

// toInt coerces a JSON number to int; def on absence.
func toInt(v any, def int) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return def, false
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// truncateRunes truncates to the first n runes.
func truncateRunes(s string, n int) string {
	if utf8RuneCount(s) <= n {
		return s
	}
	out := make([]rune, 0, n)
	for i, r := range s {
		if i >= n {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

func utf8RuneCount(s string) int {
	return len([]rune(s))
}
