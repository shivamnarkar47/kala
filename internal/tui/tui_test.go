// TUI tests: drive the Model directly through Update/View (the Elm
// architecture is a pure function of (msg) -> (model, cmd)), with a fake
// gateway feeding scripted stream events through the same sendFn seam the
// production program uses.
package tui

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kaal/kaal/internal/agents"
	"github.com/kaal/kaal/internal/gateway"
	"github.com/kaal/kaal/internal/loop"
	"github.com/kaal/kaal/internal/messages"
)

const eventSleep = gateway.EventKind(99)

func sleepEvent(seconds float64) gateway.StreamEvent {
	return gateway.StreamEvent{Kind: eventSleep, Text: strconv.FormatFloat(seconds, 'f', -1, 64)}
}

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

// fakeGateway is the loop's Gateway seam: pre-scripted events, recorded
// calls, thread-safe.
type fakeGateway struct {
	mu      sync.Mutex
	scripts [][]gateway.StreamEvent
	calls   int
}

func newFakeGateway(scripts ...[]gateway.StreamEvent) *fakeGateway {
	return &fakeGateway{scripts: scripts}
}

func (f *fakeGateway) Stream(ctx context.Context, msgs []any, tools []any, maxTokens int) <-chan gateway.StreamEvent {
	f.mu.Lock()
	script := f.scripts[0]
	f.scripts = f.scripts[1:]
	f.calls++
	f.mu.Unlock()
	ch := make(chan gateway.StreamEvent, len(script)+1)
	go func() {
		defer close(ch)
		for _, ev := range script {
			if ev.Kind == eventSleep {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

func (f *fakeGateway) ModelID() string { return "test-model" }

// testEnv wires a model with a fake gateway and a captured event stream.
type testEnv struct {
	mu       sync.Mutex
	m        *Model
	gw       *fakeGateway
	events   []tea.Msg
	turnDone chan struct{} // buffered 1: one done signal per turn
}

func setupTUI(t *testing.T, scripts ...[]gateway.StreamEvent) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", dir+"/sessions")
	gw := newFakeGateway(scripts...)
	m, err := NewWithGateway(gw, dir, "deepseek-v4-flash", 20, false)
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{m: m, gw: gw, turnDone: make(chan struct{}, 1)}
	env.m.sendFn = func(msg tea.Msg) {
		env.mu.Lock()
		env.events = append(env.events, msg)
		env.mu.Unlock()
		if _, ok := msg.(turnDoneMsg); ok {
			select {
			case env.turnDone <- struct{}{}:
			default:
			}
		}
	}
	// Window size so rendering has real dimensions.
	env.m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return env
}

// submit types the task into the composer and presses enter.
func (e *testEnv) submit(t *testing.T, task string) {
	t.Helper()
	e.m.input.SetValue(task)
	e.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

// waitTurn blocks until the loop goroutine finished.
func (e *testEnv) waitTurn(t *testing.T) {
	t.Helper()
	select {
	case <-e.turnDone:
	case <-time.After(10 * time.Second):
		t.Fatal("turn did not finish")
	}
}

// plain strips ANSI escape codes so substring assertions read the text.
func plain(s string) string {
	var sb strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
				inEscape = false
			}
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// drain delivers every captured message to the model's Update.
func (e *testEnv) drain(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	events := e.events
	e.events = nil
	e.mu.Unlock()
	for _, msg := range events {
		e.m.Update(msg)
	}
}

func (e *testEnv) runTurn(t *testing.T, task string) {
	t.Helper()
	e.submit(t, task)
	e.waitTurn(t)
	e.drain(t)
}

func (e *testEnv) view() string {
	return e.m.View()
}

func TestSubmitRendersUserAndAnswer(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("Hello from the model.\n"), doneEv("stop")},
	)
	env.runTurn(t, "Say hello")
	if env.m.turnActive {
		t.Fatal("turn still active")
	}
	view := plain(env.view())
	if !strings.Contains(view, "Say hello") {
		t.Fatalf("user block missing: %q", view)
	}
	if !strings.Contains(view, "Hello from the model.") {
		t.Fatalf("answer missing: %q", view)
	}
	joined := strings.Join(env.m.transcript, "")
	if !strings.Contains(joined, "Hello from the model.") {
		t.Fatalf("transcript missing answer")
	}
	if !strings.Contains(view, "step 1/20") {
		t.Fatalf("status bar step missing: %q", view)
	}
	if !strings.Contains(view, "ready") {
		t.Fatalf("composer state should be ready: %q", view)
	}
}

func TestToolEventsRenderInConversationAndTrace(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{
			toolCallEv(messages.ToolCall{ID: "c1", Name: "read", Arguments: `{"path": "a.txt"}`}),
			doneEv("tool_calls"),
		},
		[]gateway.StreamEvent{contentEv("Found it.\n"), doneEv("stop")},
	)
	env.runTurn(t, "read the file")
	view := plain(env.view())
	if !strings.Contains(view, "⚙ read") {
		t.Fatalf("tool start missing: %q", view)
	}
	if !strings.Contains(view, "Found it.") {
		t.Fatalf("answer missing: %q", view)
	}
	if len(env.m.traceLines) != 2 || !strings.Contains(env.m.traceLines[0], "read") {
		t.Fatalf("trace: %v", env.m.traceLines)
	}
}

func TestReasoningHiddenUnlessVerbose(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{reasoningEv("let me think"), contentEv("answer\n"), doneEv("stop")},
	)
	env.runTurn(t, "think")
	if strings.Contains(strings.Join(env.m.transcript, ""), "let me think") {
		t.Fatal("reasoning must not mirror by default")
	}
	env.m.runCommand("/verbose")
	env2 := setupTUI(t,
		[]gateway.StreamEvent{reasoningEv("let me think"), contentEv("answer\n"), doneEv("stop")},
	)
	env2.m.verbose = true
	env2.runTurn(t, "think")
	joined := strings.Join(env2.m.transcript, "")
	if !strings.Contains(joined, "[think] let me think") {
		t.Fatalf("verbose reasoning missing: %q", joined)
	}
}

func TestCancelAbortsTurn(t *testing.T) {
	// The script sleeps mid-stream so the turn is in flight when we cancel.
	env := setupTUI(t,
		[]gateway.StreamEvent{sleepEvent(0.3), contentEv("late answer\n"), doneEv("stop")},
	)
	env.submit(t, "slow turn")
	time.Sleep(50 * time.Millisecond) // let the turn start
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	env.waitTurn(t)
	env.drain(t)
	if env.m.turnActive {
		t.Fatal("turn must be inactive after cancel")
	}
	joined := strings.Join(env.m.transcript, "")
	if !strings.Contains(joined, "turn cancelled") {
		t.Fatalf("cancel notice missing: %q", joined)
	}
	if strings.Contains(joined, "late answer") {
		t.Fatal("cancelled turn must not render late content")
	}
}

func TestAskUserModalFlow(t *testing.T) {
	askScript := []gateway.StreamEvent{
		toolCallEv(messages.ToolCall{ID: "a1", Name: "ask_user", Arguments: `{"question": "Proceed?", "options": ["yes", "no"]}`}),
		doneEv("tool_calls"),
	}
	env := setupTUI(t, askScript, []gateway.StreamEvent{contentEv("Proceeding.\n"), doneEv("stop")})
	env.submit(t, "ask")
	// Wait until the ask modal opened (the loop goroutine blocks on the
	// answer channel).
	deadline := time.Now().Add(5 * time.Second)
	var askMsg openAskMsg
	for {
		env.mu.Lock()
		for _, msg := range env.events {
			if am, ok := msg.(openAskMsg); ok {
				askMsg = am
			}
		}
		env.mu.Unlock()
		if askMsg.answerCh != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if askMsg.answerCh == nil {
		t.Fatal("ask modal never opened")
	}
	if askMsg.question != "Proceed?" || len(askMsg.options) != 2 {
		t.Fatalf("ask msg: %+v", askMsg)
	}
	// Answer the modal: the blocked handler returns and the turn continues.
	askMsg.answerCh <- "yes"
	env.waitTurn(t)
	env.drain(t)
	joined := strings.Join(env.m.transcript, "")
	if !strings.Contains(joined, "Proceeding.") {
		t.Fatalf("answer after ask missing: %q", joined)
	}
	// The tool result block carried the answer.
	found := false
	for _, b := range env.m.blocks {
		if b.kind == blockTool && strings.Contains(b.text, "yes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ask answer missing from tool blocks: %+v", env.m.blocks)
	}
}

func TestSlashCommands(t *testing.T) {
	env := setupTUI(t)
	env.m.runCommand("/help")
	if len(env.m.blocks) == 0 || !strings.Contains(env.m.blocks[0].text, "/resume") {
		t.Fatalf("help block missing: %+v", env.m.blocks)
	}
	oldSession := env.m.sessionID
	env.m.runCommand("/new")
	if env.m.sessionID == oldSession {
		t.Fatal("/new must rotate the session id")
	}
	if len(env.m.blocks) != 0 {
		t.Fatal("/new must clear the conversation")
	}
	env.m.runCommand("/model")
	last := env.m.blocks[len(env.m.blocks)-1]
	if !strings.Contains(last.text, "deepseek-v4-flash") {
		t.Fatalf("/model: %q", last.text)
	}
	env.m.runCommand("/verbose")
	if !env.m.verbose {
		t.Fatal("/verbose must toggle on")
	}
	env.m.runCommand("/unknown-cmd")
	if !strings.Contains(env.m.blocks[len(env.m.blocks)-1].text, "unknown command") {
		t.Fatal("unknown command notice missing")
	}
}

func TestPromptHistory(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("ok\n"), doneEv("stop")},
		[]gateway.StreamEvent{contentEv("ok\n"), doneEv("stop")},
	)
	env.runTurn(t, "first task")
	env.runTurn(t, "second task")
	// ctrl+p: newest first, then older.
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if got := env.m.input.Value(); got != "second task" {
		t.Fatalf("history prev: %q", got)
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if got := env.m.input.Value(); got != "first task" {
		t.Fatalf("history prev 2: %q", got)
	}
	// ctrl+n walks back to the draft (empty).
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if got := env.m.input.Value(); got != "" {
		t.Fatalf("history next to draft: %q", got)
	}
}

func TestSidebarToggle(t *testing.T) {
	env := setupTUI(t)
	if env.m.sidebarVisible {
		t.Fatal("sidebar must start hidden")
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !env.m.sidebarVisible {
		t.Fatal("ctrl+s must show the sidebar")
	}
	view := plain(env.view())
	if !strings.Contains(view, "Workspace") {
		t.Fatalf("sidebar missing from view: %q", view)
	}
}

func TestSessionsModalResume(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("one\n"), doneEv("stop")},
		[]gateway.StreamEvent{contentEv("two\n"), doneEv("stop")},
	)
	env.runTurn(t, "first")  // session A persisted
	env.m.runCommand("/new") // rotate
	env.runTurn(t, "second") // session B persisted
	target := env.m.sessionID
	env.m.runCommand("/new") // a fresh, not-yet-persisted session
	env.m.runCommand("/sessions")
	if env.m.modal == nil || env.m.modal.kind != modalSessions {
		t.Fatal("sessions modal not open")
	}
	// newest first: target is the second-newest of two sessions.
	found := false
	for _, id := range env.m.modal.items {
		if id == target {
			found = true
		}
	}
	if !found {
		t.Fatalf("target session missing from modal: %v", env.m.modal.items)
	}
	// select the target entry
	for i, id := range env.m.modal.items {
		if id == target {
			env.m.modal.cursor = i
		}
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal != nil {
		t.Fatal("modal must close on enter")
	}
	if env.m.sessionID != target || !env.m.resumeNext {
		t.Fatalf("resume: session %s resumeNext %v", env.m.sessionID, env.m.resumeNext)
	}
}

func TestStatusBarShowsAgentAndMetrics(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("hi\n"), doneEv("stop")},
	)
	env.runTurn(t, "hi")
	view := plain(env.view())
	if !strings.Contains(view, "Yudhishthira") {
		t.Fatalf("agent name missing from status bar: %q", view)
	}
	if !strings.Contains(view, "step 1/20") || !strings.Contains(view, "tok/s") {
		t.Fatalf("metrics missing: %q", view)
	}
}

func TestErrorRendersInConversation(t *testing.T) {
	// A gateway error surfaces as a loop error event.
	env := setupTUI(t)
	env.m.Update(loopEvent(loop.EventError, "gateway exploded"))
	view := plain(env.view())
	if !strings.Contains(view, "gateway exploded") {
		t.Fatalf("error missing: %q", view)
	}
}

func loopEvent(kind loop.EventKind, text string) turnEventMsg {
	return turnEventMsg{ev: loop.AgentEvent{Kind: kind, Text: text}}
}

func TestTopbarHiddenByDefaultAndToggle(t *testing.T) {
	env := setupTUI(t)
	if env.m.topbarVisible {
		t.Fatal("topbar must start hidden")
	}
	view := plain(env.view())
	if strings.Contains(view, "KAAL") {
		t.Fatalf("topbar must be hidden by default: %q", view)
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !env.m.topbarVisible {
		t.Fatal("ctrl+t must show the topbar")
	}
	view = plain(env.view())
	if !strings.Contains(view, "KAAL") || !strings.Contains(view, "deepseek-v4-flash") {
		t.Fatalf("topbar content missing: %q", view)
	}
}

func TestSlashSuggestions(t *testing.T) {
	env := setupTUI(t)
	env.m.input.SetValue("/res")
	env.m.updateSuggestions()
	if !env.m.suggestionsVisible || len(env.m.suggestions) == 0 {
		t.Fatal("suggestions must appear for /res")
	}
	found := false
	for _, c := range env.m.suggestions {
		if c == "/resume" {
			found = true
		}
	}
	if !found {
		t.Fatalf("suggestions: %v", env.m.suggestions)
	}
	// tab cycles + completes
	env.m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if env.m.input.Value() == "" {
		t.Fatal("tab must complete the suggestion")
	}
	// plain text hides the popup
	env.m.input.SetValue("hello")
	env.m.updateSuggestions()
	if env.m.suggestionsVisible {
		t.Fatal("suggestions must hide without a leading slash")
	}
}

func TestMermaidMissingTermaidNotice(t *testing.T) {
	// A ```mermaid fence at turn end with termaid absent (PATH emptied) lands
	// a notice instead of art.
	t.Setenv("PATH", "")
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("Here is the plan:\n\n```mermaid\nflowchart LR\nA --> B\n```\n"), doneEv("stop")},
	)
	env.runTurn(t, "draw")
	// Drain again: the diagram worker sends its result after the turn. The
	// 20s guard absorbs -race ./... load (the render is sub-ms normally).
	deadline := time.Now().Add(20 * time.Second)
	for {
		env.drain(t)
		found := false
		for _, b := range env.m.blocks {
			if b.kind == blockNotice && strings.Contains(b.text, "termaid") {
				found = true
			}
		}
		if found || time.Now().After(deadline) {
			if !found {
				t.Fatalf("termaid notice missing: %+v", env.m.blocks)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDiagramsToggleOffSkipsRender(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("```mermaid\nflowchart LR\nA --> B\n```\n"), doneEv("stop")},
	)
	env.m.diagramsEnabled = false
	env.runTurn(t, "draw")
	time.Sleep(50 * time.Millisecond)
	env.drain(t)
	for _, b := range env.m.blocks {
		if b.kind == blockNotice && strings.Contains(b.text, "termaid") {
			t.Fatalf("diagram rendered with diagrams off: %+v", env.m.blocks)
		}
	}
}

func TestModelsModalShowsPrices(t *testing.T) {
	env := setupTUI(t)
	env.m.runCommand("/models")
	if env.m.modal == nil || env.m.modal.kind != modalModels {
		t.Fatal("models modal not open")
	}
	view := plain(env.m.modalView())
	if !strings.Contains(view, "deepseek-v4-flash") {
		t.Fatalf("model missing: %q", view)
	}
	if !strings.Contains(view, "per M") {
		t.Fatalf("price lines missing: %q", view)
	}
}

func TestAgentsModalActivates(t *testing.T) {
	env := setupTUI(t)
	env.m.runCommand("/agents")
	if env.m.modal == nil || env.m.modal.kind != modalAgents {
		t.Fatal("agents modal not open")
	}
	found := false
	for i, name := range env.m.modal.items {
		if name == "Bhima" {
			env.m.modal.cursor = i
			found = true
		}
	}
	if !found {
		t.Fatalf("Bhima missing from agents: %v", env.m.modal.items)
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal != nil {
		t.Fatal("modal must close on enter")
	}
	if env.m.agent == nil || env.m.agent.Name != "Bhima" {
		t.Fatalf("agent not activated: %+v", env.m.agent)
	}
	// Persisted.
	state := agents.Load(env.m.projectDir)
	if state.Active != "Bhima" {
		t.Fatalf("persisted active: %q", state.Active)
	}
}

func TestAgentGeneratorFlow(t *testing.T) {
	// The generator streams a completion on the gateway: script it to reply
	// with a JSON persona, then check it was added + activated + persisted.
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv(`{"name": "Karna", "description": "the relentless executor"}`), doneEv("stop")},
	)
	env.m.generateAgent("a loyal warrior", AgentGeneratorSystemPrompt, "generated and active")
	deadline := time.Now().Add(5 * time.Second)
	for {
		env.drain(t)
		if !env.m.generatingAgent && strings.Contains(strings.Join(env.m.transcript, ""), "Karna") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generator never landed: transcript %q", strings.Join(env.m.transcript, ""))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if env.m.agent == nil || env.m.agent.Name != "Karna" {
		t.Fatalf("agent not activated: %+v", env.m.agent)
	}
	state := agents.Load(env.m.projectDir)
	if state.Active != "Karna" {
		t.Fatalf("persisted active: %q", state.Active)
	}
}

func TestAgentGeneratorReentryGuarded(t *testing.T) {
	env := setupTUI(t)
	env.m.generatingAgent = true
	env.m.generateAgent("another one", AgentGeneratorSystemPrompt, "x")
	if !strings.Contains(strings.Join(env.m.transcript, ""), "already running") {
		t.Fatalf("re-entry must be refused: %q", strings.Join(env.m.transcript, ""))
	}
}

func TestAgentGeneratorUnparsableReply(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("I cannot do that"), doneEv("stop")},
	)
	env.m.generateAgent("whatever", AgentGeneratorSystemPrompt, "x")
	deadline := time.Now().Add(5 * time.Second)
	for {
		env.drain(t)
		if !env.m.generatingAgent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("generator never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	joined := strings.Join(env.m.transcript, "")
	if !strings.Contains(joined, "could not parse") {
		t.Fatalf("parse-failure notice missing: %q", joined)
	}
}

func TestAskModalRendersOptions(t *testing.T) {
	env := setupTUI(t)
	answerCh := make(chan string, 1)
	env.m.Update(openAskMsg{question: "Proceed?", options: []string{"yes", "no"}, answerCh: answerCh})
	if env.m.modal == nil || env.m.modal.kind != modalAsk {
		t.Fatal("ask modal not open")
	}
	view := plain(env.m.modalView())
	if !strings.Contains(view, "Proceed?") || !strings.Contains(view, "1. yes") || !strings.Contains(view, "2. no") {
		t.Fatalf("ask modal content: %q", view)
	}
	// Number key picks an option.
	env.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	select {
	case ans := <-answerCh:
		if ans != "no" {
			t.Fatalf("answer: %q", ans)
		}
	default:
		t.Fatal("answer never delivered")
	}
}

func TestHomeBannerShowsOnFreshSession(t *testing.T) {
	env := setupTUI(t)
	// The visible window shows the theme's title, tagline, and first action.
	view := plain(env.view())
	for _, want := range []string{"KURUKSHETRA", "field of dharma", "ask a task", "kaal 0.3"} {
		if !strings.Contains(view, want) {
			t.Fatalf("home window missing %q in %q", want, view)
		}
	}
	// The render pulls the Pandava cast from agentsState (covered by the
	// agents tests); the wordmark + title are mirrored to the transcript once.
	joined := strings.Join(env.m.transcript, "")
	if !strings.Contains(joined, "KURUKSHETRA") {
		t.Fatalf("transcript missing home: %q", joined)
	}
}

func TestNewShowsHomeAgain(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("answer\n"), doneEv("stop")},
	)
	env.runTurn(t, "hello")
	if strings.Contains(plain(env.view()), "KURUKSHETRA") {
		t.Fatal("home must hide during a conversation")
	}
	env.m.runCommand("/new")
	if !strings.Contains(plain(env.view()), "KURUKSHETRA") {
		t.Fatal("/new must return to the home banner")
	}
}
