// TUI tests: drive the Model directly through Update/View (the Elm
// architecture is a pure function of (msg) -> (model, cmd)), with a fake
// gateway feeding scripted stream events through the same sendFn seam the
// production program uses.
package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kaal/kaal/internal/agents"
	"github.com/kaal/kaal/internal/config"
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

func TestNewStartsWithoutAPIKey(t *testing.T) {
	// The zen free tier is keyless: the default model runs with no login at
	// all. Only switching to a keyed (paid / command-code) model without a
	// key flags the missing-key state and blocks sending.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "")

	m, err := New(dir, 20, false)
	if err != nil {
		t.Fatalf("New must succeed without an API key: %v", err)
	}
	if m.keyMissing {
		t.Fatal("free-tier default must not flag a missing key")
	}
	if !m.freeKeyless {
		t.Fatal("keyless free-tier start should be marked as such")
	}

	// Switch to a paid model with no key anywhere: sending is blocked and
	// the notice points at /connect.
	m.setModel("deepseek-v4-flash") // paid route
	if !m.keyMissing {
		t.Fatal("paid model without a key must flag keyMissing")
	}
	if cmd := m.Submit("hi"); cmd != nil {
		t.Fatal("Submit without a key must not start a turn")
	}
	if m.turnActive {
		t.Fatal("turn must not be active without a key")
	}
	// And back to the free tier: usable again.
	m.setModel(config.ModelID)
	if m.keyMissing {
		t.Fatal("returning to the free tier clears keyMissing")
	}
	// /connect still saves the key and makes it live for keyed models.
	if err := config.SaveUserAPIKey("sk-test"); err != nil {
		t.Fatal(err)
	}
	m.applySavedKey()
	gw, ok := m.gateway.(*gateway.Gateway)
	if !ok {
		t.Fatalf("gateway type: %T", m.gateway)
	}
	if gw.APIKey != "sk-test" {
		t.Fatalf("gateway key not updated: %q", gw.APIKey)
	}
}

func TestConnectSavesToActiveProviderStore(t *testing.T) {
	// /connect writes into the active model's provider store: a Command Code
	// model must land in api_key.commandcode, leaving opencode's store alone.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("CMD_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")
	env := setupTUI(t)
	env.m.keyMissing = true
	env.m.modelID = "stealth/ox-alpha" // Command Code route

	env.submit(t, "/connect sk-cmd-key")
	if got := config.LoadUserAPIKeyFor(config.ProviderCommandCode); got != "sk-cmd-key" {
		t.Fatalf("commandcode store: %q", got)
	}
	if config.LoadUserAPIKey() != "" {
		t.Fatal("opencode store must stay untouched")
	}
	if env.m.keyMissing {
		t.Fatal("keyMissing must clear after saving")
	}
}

func TestConnectViaEnterOpensDialogAndSaves(t *testing.T) {
	// Regression: pressing Enter on a slash command must run the command, not
	// submit it as a prompt. Typing /connect keyless must open the PROVIDER
	// PICKER; choosing opencode with no key opens the key dialog; saving the
	// key persists it, clears the missing-key state, and chains straight into
	// opencode's model list.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "")
	env := setupTUI(t)
	env.m.keyMissing = true // simulate a keyless start

	env.submit(t, "/connect")
	if env.m.modal == nil || env.m.modal.kind != modalProviders {
		t.Fatalf("enter on /connect must open the provider picker, modal=%+v", env.m.modal)
	}
	pickerView := plain(env.m.modalView())
	if !strings.Contains(pickerView, "opencode · zen free") ||
		!strings.Contains(pickerView, "opencode · go plan") ||
		!strings.Contains(pickerView, "command-code") ||
		!strings.Contains(pickerView, "add another provider") {
		t.Fatalf("picker must offer all four choices: %q", pickerView)
	}
	if env.m.turnActive {
		t.Fatal("a slash command must not start a turn")
	}

	// Choose opencode (cursor 0): no key resolves, so the key dialog follows.
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal == nil || env.m.modal.kind != modalConnect {
		t.Fatalf("opencode without a key must open the dialog, modal=%+v", env.m.modal)
	}
	if view := plain(env.view()); strings.Contains(view, "no API key") {
		t.Fatalf("no API key error must not show inside the flow: %q", view)
	}

	// Paste a key into the dialog and press enter: saved + made live, and
	// the opencode model list opens.
	env.m.modal.input.SetValue("sk-dialog-key")
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal == nil || env.m.modal.kind != modalModels {
		t.Fatalf("model list must follow the saved key, modal=%+v", env.m.modal)
	}
	if got := config.LoadUserAPIKey(); got != "sk-dialog-key" {
		t.Fatalf("saved key: %q", got)
	}
	if env.m.keyMissing {
		t.Fatal("keyMissing must clear after the dialog saves")
	}
	for _, id := range env.m.modal.items {
		if config.ModelProvider(id) != config.ProviderOpencode {
			t.Fatalf("non-opencode model leaked into the list: %s", id)
		}
	}
}

func TestProviderPickerCommandCodeGatesOnKey(t *testing.T) {
	// command-code demands its key before anything else; after the save the
	// flow lands on its own model list only.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("CMD_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")
	env := setupTUI(t)

	env.submit(t, "/connect")
	env.m.modal.cursor = 2 // command-code
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal == nil || env.m.modal.kind != modalConnect {
		t.Fatalf("command-code without a key must ask for one, modal=%+v", env.m.modal)
	}
	env.m.modal.input.SetValue("sk-cmd")
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal == nil || env.m.modal.kind != modalModels {
		t.Fatalf("models list must follow the key, modal=%+v", env.m.modal)
	}
	if len(env.m.modal.items) == 0 {
		t.Fatal("command-code model list is empty")
	}
	for _, id := range env.m.modal.items {
		if config.ModelProvider(id) != config.ProviderCommandCode {
			t.Fatalf("foreign model in command-code list: %s", id)
		}
	}
	if got := config.LoadUserAPIKeyFor(config.ProviderCommandCode); got != "sk-cmd" {
		t.Fatalf("command-code store: %q", got)
	}
}

func TestProviderPickerOpencodeWithKeySkipsToModels(t *testing.T) {
	// With a resolvable opencode key the picker goes straight to models —
	// no key dialog.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "sk-env")
	env := setupTUI(t)

	env.submit(t, "/connect")
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // cursor 0 = opencode
	if env.m.modal == nil || env.m.modal.kind != modalModels {
		t.Fatalf("opencode with a ready key must open models directly, modal=%+v", env.m.modal)
	}
	if len(env.m.modal.items) == 0 {
		t.Fatal("opencode model list is empty")
	}
	for _, id := range env.m.modal.items {
		if config.ModelProvider(id) != config.ProviderOpencode {
			t.Fatalf("foreign model in opencode list: %s", id)
		}
	}
}

func TestAddAnotherProviderFetchesModelsAndPersists(t *testing.T) {
	// The BYOK form probes <base>/models live, lists what came back, and the
	// pick persists the whole provider (base URL + key + chosen model).
	probePaths := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case probePaths <- r.URL.Path:
		default:
		}
		if r.Header.Get("Authorization") != "Bearer sk-byok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"id":"zz-model"},{"id":"aa-model"}]}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "")
	env := setupTUI(t)

	env.m.openCustomProviderForm()
	if env.m.modal == nil || env.m.modal.kind != modalCustomProvider {
		t.Fatal("BYOK form did not open")
	}
	env.m.modal.input.SetValue(srv.URL + "/v1")
	env.m.modal.input2.SetValue("sk-byok")
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	// The fetch runs on a worker goroutine; drain until the picker lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		env.drain(t)
		if env.m.modal != nil && env.m.modal.kind == modalModels {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fetched model list never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(probePaths)
	got := ""
	for p := range probePaths {
		got = p
	}
	if got != "/v1/models" {
		t.Fatalf("probe hit %q", got)
	}
	if len(env.m.modal.items) != 2 || env.m.modal.items[0] != "aa-model" || env.m.modal.items[1] != "zz-model" {
		t.Fatalf("fetched items: %v", env.m.modal.items)
	}

	// Pick one: the provider is finalized and owns the id for routing.
	env.m.modal.cursor = 0
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal != nil {
		t.Fatal("modal must close after the pick")
	}
	cp := config.LoadCustomProvider()
	if cp == nil {
		t.Fatal("custom provider was not persisted")
	}
	if cp.BaseURL != srv.URL+"/v1" || cp.APIKey != "sk-byok" || cp.Model != "aa-model" {
		t.Fatalf("persisted custom provider: %+v", cp)
	}
	if got := config.ModelBaseURL("aa-model"); got != srv.URL+"/v1" {
		t.Fatalf("routing after pick: %s", got)
	}
	t.Cleanup(func() { _ = config.ClearCustomProvider() })
}

func TestEnterOnSlashCommandDoesNotSubmit(t *testing.T) {
	// A leading / must route to the command parser even when a key exists,
	// instead of being sent to the model as a prompt.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	if err := config.SaveUserAPIKey("sk-present"); err != nil {
		t.Fatal(err)
	}
	env := setupTUI(t)
	env.m.applySavedKey()
	if env.m.keyMissing {
		t.Fatal("keyMissing should be false with a stored key")
	}

	env.submit(t, "/model")
	if env.m.turnActive {
		t.Fatal("/model must not start a turn")
	}
	if env.gw.calls != 0 {
		t.Fatalf("gateway must not be called: %d calls", env.gw.calls)
	}
	last := env.m.blocks[len(env.m.blocks)-1]
	if !strings.Contains(last.text, "deepseek-v4-flash") {
		t.Fatalf("/model notice missing from blocks: %q", last.text)
	}
	if got := env.m.input.Value(); got != "" {
		t.Fatalf("composer must clear after a command, got %q", got)
	}
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
		if c.name == "/resume" {
			found = true
			if c.desc == "" {
				t.Fatal("every palette entry must carry a description")
			}
		}
	}
	if !found {
		t.Fatalf("suggestions: %+v", env.m.suggestions)
	}
	// tab completes the cursor entry (args hint becomes a trailing space).
	env.m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := env.m.input.Value(); got != "/resume " {
		t.Fatalf("tab completion: %q", got)
	}
	// plain text hides the popup
	env.m.input.SetValue("hello")
	env.m.updateSuggestions()
	if env.m.suggestionsVisible {
		t.Fatal("suggestions must hide without a leading slash")
	}
}

func TestSlashPaletteNavigationAndDismiss(t *testing.T) {
	env := setupTUI(t)
	env.m.input.SetValue("/")
	env.m.updateSuggestions()
	if len(env.m.suggestions) < 8 {
		t.Fatalf("bare / must list the commands, got %d", len(env.m.suggestions))
	}
	// ↓ moves the cursor; esc dismisses until the input changes.
	start := env.m.suggestIndex
	env.m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if env.m.suggestIndex != start+1 {
		t.Fatalf("down must move the cursor: %d", env.m.suggestIndex)
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if env.m.suggestionsVisible {
		t.Fatal("esc must dismiss the palette")
	}
	env.m.input.SetValue("/he")
	env.m.updateSuggestions()
	if !env.m.suggestionsVisible {
		t.Fatal("typing again must bring the palette back")
	}
	// description search: "/persona" finds /agents by its description.
	env.m.input.SetValue("/persona")
	env.m.updateSuggestions()
	if !env.m.suggestionsVisible {
		t.Fatal("description matches must surface")
	}
	found := false
	for _, c := range env.m.suggestions {
		if c.name == "/agents" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/agents not found by description: %+v", env.m.suggestions)
	}
}

func TestSlashEnterCompletesPartialCommand(t *testing.T) {
	// "/res" + enter used to die as an unknown command; now it completes
	// from the palette and waits for arguments.
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("nope\n"), doneEv("stop")},
	)
	env.m.input.SetValue("/res")
	env.m.updateSuggestions()
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := env.m.input.Value(); got != "/resume " {
		t.Fatalf("enter must complete the partial: %q", got)
	}
	if env.m.turnActive {
		t.Fatal("completion must not start a turn")
	}
	// A bare exact command still executes on the first enter.
	env.m.input.Reset()
	env.m.updateSuggestions()
	env.m.input.SetValue("/help")
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(env.m.blocks) == 0 || !strings.Contains(env.m.blocks[0].text, "Commands") {
		t.Fatalf("/help panel missing: %+v", env.m.blocks)
	}
}

func TestSlashUnknownSuggestsNearest(t *testing.T) {
	env := setupTUI(t)
	env.m.runCommand("/sesion")
	last := env.m.blocks[len(env.m.blocks)-1].text
	if !strings.Contains(last, "unknown command") || !strings.Contains(last, "/sessions") {
		t.Fatalf("nearest-command hint missing: %q", last)
	}
	env.m.runCommand("/zzz")
	last = env.m.blocks[len(env.m.blocks)-1].text
	if !strings.Contains(last, "(try /help)") {
		t.Fatalf("fallback hint missing: %q", last)
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
	// Tall enough that the whole catalog fits without scrolling.
	env.m.Update(tea.WindowSizeMsg{Width: 100, Height: 70})
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
	// Dialogs render as centered cards with an 8-row top margin: the first
	// non-blank line is the frame, indented from the left edge.
	lines := strings.Split(env.m.modalView(), "\n")
	frame := 0
	for len(lines) > frame && strings.TrimSpace(lines[frame]) == "" {
		frame++
	}
	if frame != modalTopMargin {
		t.Fatalf("expected %d blank margin rows, got %d", modalTopMargin, frame)
	}
	if frame >= len(lines) {
		t.Fatal("empty modal frame")
	}
	pad := len(lines[frame]) - len(strings.TrimLeft(lines[frame], " "))
	if pad < 4 {
		t.Fatalf("modal is not centered (leading pad %d at width 100)", pad)
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

// -- the Kurukshetra skin ------------------------------------------------------------

func TestIdentityForKnownAndCustomAgents(t *testing.T) {
	// The five Pandavas carry sigil + byname; invented agents (Karna via
	// ctrl+g) fall back to the saffron default instead of missing.
	if id := identityFor("Bhima"); id.glyph != "⚒" || id.epithet != "vrikodara" {
		t.Fatalf("Bhima identity: %+v", id)
	}
	if id := identityFor("Arjuna"); id.color != colorPeacock {
		t.Fatalf("Arjuna color: %+v", id)
	}
	custom := identityFor("Karna")
	if custom.glyph != defaultGlyph || custom.epithet != defaultEpi {
		t.Fatalf("custom agent must get the saffron default: %+v", custom)
	}
}

func TestHomeShowsVerseAndRegalia(t *testing.T) {
	env := setupTUI(t)
	view := plain(env.view())
	for _, want := range []string{
		gitaVerse,
		"Bhagavad Gita 2.47",
		"dharmaraja",
		"☸ Yudhishthira",
		"➶ Arjuna",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("home missing %q in %q", want, view)
		}
	}
}

func TestAssistantBlockSpeaksWithActivePandava(t *testing.T) {
	// The answering block carries the active persona's name, not a generic
	// label; with no persona it falls back to kaal.
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("By my mace.\n"), doneEv("stop")},
	)
	env.m.activateAgent("Bhima") // activateAgent appends a notice block
	env.runTurn(t, "strike")

	// The notice from activation is fine; find the assistant answer.
	found := false
	for _, b := range env.m.blocks {
		if b.kind == blockAssistant && strings.Contains(b.text, "By my mace.") {
			found = true
		}
	}
	if !found {
		t.Fatalf("assistant answer block missing: %+v", env.m.blocks)
	}
	view := plain(env.view())
	if !strings.Contains(view, "▌ Bhima") {
		t.Fatalf("assistant label must use the active Pandava: %q", view)
	}

	// No persona: the voice falls back to kaal.
	env2 := setupTUI(t, []gateway.StreamEvent{contentEv("ok\n"), doneEv("stop")})
	env2.m.agent = nil
	env2.runTurn(t, "hi")
	if !strings.Contains(plain(env2.view()), "▌ kaal") {
		t.Fatal("no persona must render the plain kaal voice")
	}
}

func TestAgentsModalShowsSigils(t *testing.T) {
	env := setupTUI(t)
	env.m.runCommand("/agents")
	view := plain(env.m.modalView())
	for _, want := range []string{"Sabha", "☸ Yudhishthira — dharmaraja", "✦ Sahadeva — daivajna"} {
		if !strings.Contains(view, want) {
			t.Fatalf("agents modal missing %q in %q", want, view)
		}
	}
}

func TestStatusBarBadgeKeepsSegments(t *testing.T) {
	// The themed badge replaces the old chip but the metric segments stay
	// machine-readable (scripts and tests parse them).
	env := setupTUI(t)
	status := plain(env.m.statusBar())
	for _, want := range []string{"Yudhishthira", "step 0/20", "tok/s"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status bar missing %q in %q", want, status)
		}
	}
	if strings.Contains(status, "dharmaraja") {
		t.Fatal("status bar badge must stay compact (no epithet)")
	}
}

func TestProviderPickerOpencodeGoListsPaidOnly(t *testing.T) {
	// The go-plan choice shares opencode's key chain but lists ONLY the paid
	// route's models — no free tier leakage.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	t.Setenv("HOME", dir)
	t.Setenv("OPENCODE_API_KEY", "")
	env := setupTUI(t)

	// Without a key: gated by the dialog first.
	env.submit(t, "/connect")
	for i, item := range env.m.modal.items {
		if item == pickOpencodeGo {
			env.m.modal.cursor = i
		}
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal == nil || env.m.modal.kind != modalConnect {
		t.Fatalf("go plan without a key must ask for one, modal=%+v", env.m.modal)
	}
	if !strings.Contains(env.m.modal.title, "opencode go") {
		t.Fatalf("dialog title: %q", env.m.modal.title)
	}
	env.m.modal.input.SetValue("sk-go-key")
	env.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if env.m.modal == nil || env.m.modal.kind != modalModels {
		t.Fatalf("paid model list must follow the key, modal=%+v", env.m.modal)
	}
	if !strings.Contains(plain(env.m.modalView()), "opencode go") {
		t.Fatalf("list title: %q", plain(env.m.modalView()))
	}
	if len(env.m.modal.items) == 0 {
		t.Fatal("go model list is empty")
	}
	for _, id := range env.m.modal.items {
		if config.FreeTierModel(id) || config.ModelProvider(id) != config.ProviderOpencode {
			t.Fatalf("non-paid model in go list: %s", id)
		}
	}
	if got := config.LoadUserAPIKey(); got != "sk-go-key" {
		t.Fatalf("opencode store: %q", got)
	}
}

func TestModelsModalScrollsOnSmallScreens(t *testing.T) {
	// A short terminal must clip the list — never overflow the screen — and
	// the scroll window must follow the cursor.
	env := setupTUI(t)
	env.m.Update(tea.WindowSizeMsg{Width: 100, Height: 16})
	env.m.runCommand("/models")
	if env.m.modal == nil {
		t.Fatal("models modal not open")
	}
	view := plain(env.m.modalView())
	total := len(env.m.modal.items)
	if total < 10 {
		t.Fatalf("catalog too small to scroll: %d", total)
	}
	if !strings.Contains(view, "of "+itoa(total)) {
		t.Fatalf("scroll indicator missing: %q", tailOf(view, 400))
	}
	// The rendered card must fit inside the screen height.
	lineCount := strings.Count(env.m.modalView(), "\n")
	if lineCount > 16 {
		t.Fatalf("modal overflows the screen: %d lines", lineCount)
	}
	// Walk the cursor deep into the list; the window must slide along.
	for i := 0; i < 20 && env.m.modal.cursor < total-1; i++ {
		env.m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	last := env.m.modal.items[env.m.modal.cursor]
	if !strings.Contains(plain(env.m.modalView()), last) {
		t.Fatalf("cursor row fell out of the scroll window: %q", last)
	}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestAppFramePadsWindow(t *testing.T) {
	// The whole application floats inside a 4-cell frame: blank rows above,
	// an even left inset, and never more lines than the screen.
	env := setupTUI(t)
	env.m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	lines := strings.Split(env.m.View(), "\n")
	if len(lines) > 30 {
		t.Fatalf("view exceeds the screen: %d lines", len(lines))
	}
	for i := 0; i < appPadding; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			t.Fatalf("top frame row %d not blank: %q", i, lines[i])
		}
	}
	if !strings.HasPrefix(lines[appPadding], strings.Repeat(" ", appPadding)) {
		t.Fatalf("left frame missing: %q", lines[appPadding])
	}
}

func TestConnectingIndicatorShowsBeforeFirstByte(t *testing.T) {
	// Enter → instant indicator ("opening <host>"), even while the gateway
	// is silent; it disappears once the stream shows life.
	env := setupTUI(t,
		[]gateway.StreamEvent{sleepEvent(0.3), contentEv("late answer\n"), doneEv("stop")},
	)
	env.submit(t, "slow turn")
	if !env.m.connecting {
		t.Fatal("connecting must be set the moment the prompt is sent")
	}
	if env.m.turnActive && !strings.Contains(plain(env.view()), "opening") {
		t.Fatalf("connect indicator missing from view: %q", plain(env.view()))
	}
	env.waitTurn(t)
	env.drain(t)
	if env.m.connecting {
		t.Fatal("connecting must clear once the stream showed life")
	}
	if strings.Contains(plain(env.view()), "opening") {
		t.Fatal("connect indicator must not outlive the connection")
	}
	if !strings.Contains(plain(env.view()), "late answer") {
		t.Fatal("answer missing")
	}
}

func TestResponseTimeFooter(t *testing.T) {
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("answer one\n"), doneEv("stop")},
	)
	env.runTurn(t, "first")
	view := plain(env.view())
	if !strings.Contains(view, "⏱") || !strings.Contains(view, "step 1") {
		t.Fatalf("timing receipt missing: %q", tailOf(view, 300))
	}

	// Cancelled turns leave no receipt (nothing was kept).
	env2 := setupTUI(t,
		[]gateway.StreamEvent{sleepEvent(0.3), contentEv("late\n"), doneEv("stop")},
	)
	env2.submit(t, "slow")
	time.Sleep(50 * time.Millisecond)
	env2.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	env2.waitTurn(t)
	env2.drain(t)
	if strings.Contains(plain(env2.view()), "⏱") {
		t.Fatal("cancelled turn must not print a timing receipt")
	}
}

func TestChatScrollFollowAndRelease(t *testing.T) {
	// Wheel-up releases the follow pin and scrolls; new output must NOT
	// yank the reader back. Returning to the live edge re-pins.
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("answer one\n"), doneEv("stop")},
	)
	env.runTurn(t, "first")
	for i := 0; i < 30; i++ {
		env.m.appendNotice(fmt.Sprintf("filler line %02d — padding the scroll range", i))
	}
	env.m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	if env.m.viewport.AtBottom() && env.m.viewport.YOffset == 0 {
		t.Log("note: no scroll range at this size")
	}
	bottomY := env.m.viewport.YOffset

	env.m.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	env.m.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	if env.m.follow {
		t.Fatal("wheel-up must release the follow pin")
	}
	heldY := env.m.viewport.YOffset
	if heldY >= bottomY {
		t.Fatalf("wheel-up did not scroll: y=%d bottom=%d", heldY, bottomY)
	}

	// New content arrives while unpinned: no jump.
	env.m.appendNotice("new output should not yank")
	if y := env.m.viewport.YOffset; y != heldY {
		t.Fatalf("unpinned viewport moved on append: %d → %d", heldY, y)
	}

	// Wheel back to the live edge re-pins.
	for i := 0; i < 40 && !env.m.follow; i++ {
		env.m.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	}
	if !env.m.follow || !env.m.viewport.AtBottom() {
		t.Fatal("returning to the bottom must re-pin follow")
	}
}

func TestChatScrollKeyboardPinning(t *testing.T) {
	// pgup releases the pin; pgdown at the bottom and ctrl+l re-pin.
	env := setupTUI(t,
		[]gateway.StreamEvent{contentEv("answer\n"), doneEv("stop")},
	)
	env.runTurn(t, "first")
	for i := 0; i < 40; i++ {
		env.m.appendNotice(fmt.Sprintf("scroll filler %02d", i))
	}
	env.m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})

	env.m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if env.m.follow {
		t.Fatal("pgup must release the pin")
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	if !env.m.follow || !env.m.viewport.AtBottom() {
		t.Fatal("ctrl+l must re-pin at the bottom")
	}
	env.m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	env.m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	env.m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if env.m.viewport.AtBottom() && !env.m.follow {
		t.Fatal("at-bottom pgdown must re-pin")
	}
}
