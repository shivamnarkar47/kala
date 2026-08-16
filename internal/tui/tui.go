// Package tui ports harness/tui.py: the bubbletea workbench — conversation
// pane, collapsible sidebar (Trace/Memory/Sessions), composer with prompt
// history, slash commands, tmux-style status bar, and modals (sessions
// switcher, connect, models, ask_user). The ONLY package importing the
// charmbracelet family.
//
// The loop runs on its own goroutine and marshals events through a send
// function (program.Send in production — the Elm Cmd/Msg pattern replaces
// Python's call_from_thread); Ctrl+C is a HARD cancel — it cancels the
// turn's context, which aborts the in-flight SSE stream for real.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/kaal/kaal/internal/agents"
	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/gateway"
	"github.com/kaal/kaal/internal/loop"
	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/messages"
	"github.com/kaal/kaal/internal/prompts"
	"github.com/kaal/kaal/internal/sessions"
	"github.com/kaal/kaal/internal/structure"
	"github.com/kaal/kaal/internal/tools"
)

// Streaming markdown flush cadence (the Python MD_FLUSH_SECONDS/adaptive
// rule): small deltas flush synchronously, bursts are throttled.
const (
	mdFlushSeconds      = 0.1
	mdFlushSecondsLong  = 0.25
	mdLongTurnChars     = 20_000
	mdInstantFlushChars = 2_000
	thinkingTick        = 120 * time.Millisecond
	rateTick            = 1 * time.Second
	clockTick           = 30 * time.Second
	maxResultPreview    = 160
	maxComposerLines    = 5
	sidebarWidth        = 34
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// AgentGeneratorSystemPrompt is the AI agent designer's system prompt
// (verbatim from tui.py): the model answers with ONLY a JSON persona.
const AgentGeneratorSystemPrompt = `You are an agent designer for a coding harness. The user describes an agent
persona they want. Respond with ONLY a JSON object: {"name": "<a strong,
fitting name — prefer Sanskrit/epic-flavored names in the spirit of the
Pandavas>", "description": "<one or two sentences describing the persona's
strengths and style>"}.`

const agentGeneratorMaxTokens = 300

var slashCommands = []string{
	"/help", "/new", "/resume", "/sessions", "/models", "/connect",
	"/memory", "/model", "/verbose", "/sidebar", "/structure",
	"/diagram", "/diagrams", "/topbar", "/agents", "/quit",
}

var mermaidFenceRe = regexp.MustCompile("(?s)```mermaid\n(.*?)```")

// -- messages ------------------------------------------------------------------

type turnEventMsg struct{ ev loop.AgentEvent }

type turnDoneMsg struct{ err error }

type flushMDMsg struct{}

type rateTickMsg struct{}

type clockTickMsg struct{}

type thinkTickMsg struct{ frame int }

// openAskMsg asks the user (from the loop goroutine); the handler blocks on
// answerCh until the modal is answered.
type openAskMsg struct {
	question string
	options  []string
	answerCh chan string
}

type diagramArtMsg struct {
	text string
	err  error
}

type agentDoneMsg struct {
	reply  string
	phrase string
}

type agentErrorMsg struct{ message string }

// -- blocks ---------------------------------------------------------------------

type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockTool
	blockNotice
	blockError
)

type block struct {
	kind blockKind
	text string
}

// -- modals ----------------------------------------------------------------------

type modalKind int

const (
	modalNone modalKind = iota
	modalSessions
	modalConnect
	modalModels
	modalAsk
	modalAgents
	modalAgentForm
	modalAgentIntent
)

type modal struct {
	kind        modalKind
	title       string
	items       []string // filtered list (sessions / models / agents)
	allItems    []string
	cursor      int
	filter      string
	input       *textarea.Model // connect key entry + models filter
	input2      *textarea.Model // agent form description
	formFocus   int             // 0 = name, 1 = description
	askQuestion string
	askOptions  []string
	askChan     chan string
}

// -- model -----------------------------------------------------------------------

type Model struct {
	// static
	gateway        loop.Gateway
	tools          *tools.Registry
	mem            *memory.Memory
	structure      *structure.StructureManager
	projectDir     string
	maxSteps       int
	allowDangerous bool
	modelID        string
	agent          *prompts.Agent

	// event marshaling seam (program.Send in production; tests swap it)
	sendFn func(tea.Msg)

	// agent personas (seeded + persisted on activation)
	agentsState agents.State

	// session state
	sessionID     string
	resumeNext    bool
	verbose       bool
	promptHistory []string
	historyIndex  int
	draft         string
	steps         int
	totalUsage    loop.Usage
	totalCost     float64

	// view state
	width, height  int
	viewport       viewport.Model
	input          textarea.Model
	sidebarVisible bool
	sidebarTab     int // 0 trace, 1 memory, 2 sessions
	topbarVisible  bool
	traceLines     []string
	memoryText     string
	sessionEntries []map[string]any
	transcript     []string
	modal          *modal

	// slash-command suggestions popup
	suggestionsVisible bool
	suggestions        []string
	suggestIndex       int

	// mermaid auto-render + AI agent generator
	diagramsEnabled bool
	generatingAgent bool

	// turn state
	turnActive    bool
	thinking      bool
	thinkFrame    int
	cancelTurn    context.CancelFunc
	blocks        []block
	mdPending     string
	flushArmed    bool
	streamChars   int
	lastRateChars int
	turnStart     time.Time
	lastTickTime  time.Time
	rate          float64
	mdRenderer    *glamour.TermRenderer

	// styles
	agentStyle     lipgloss.Style
	rightStyle     lipgloss.Style
	dimStyle       lipgloss.Style
	errorStyle     lipgloss.Style
	userLabelStyle lipgloss.Style
	assistantLabel lipgloss.Style
}

// New builds the workbench (mirrors HarnessTui.__init__): resolves the key,
// seeds the persona (first Pandava active by default, persisted), and wires
// the real registry/memory/structure.
func New(projectDir string, maxSteps int, allowDangerous bool) (*Model, error) {
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		projectDir = cwd
	}
	modelID := config.ResolveModelID("")
	key, err := config.GetAPIKey()
	if err != nil {
		return nil, err
	}
	gw := &gateway.Gateway{BaseURL: config.ModelBaseURL(modelID), APIKey: key, Model: modelID}
	return NewWithGateway(gw, projectDir, modelID, maxSteps, allowDangerous)
}

// NewWithGateway builds the workbench around an injected gateway (tests
// pass a fake; New uses the real one).
func NewWithGateway(gw loop.Gateway, projectDir, modelID string, maxSteps int, allowDangerous bool) (*Model, error) {
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		projectDir = cwd
	}
	if modelID == "" {
		modelID = config.ResolveModelID("")
	}
	mem := memory.NewMemory(projectDir + "/.agent-memory")
	toolRegistry := tools.NewRegistry(projectDir, allowDangerous, nil, mem)
	st := structure.NewStructureManager(projectDir)
	_ = st.Ensure() // best-effort

	// Persona: seeded with the five Pandavas when .kaal/agents.json is
	// missing; when nothing is active, activate the first agent and persist.
	agentState := agents.Load(projectDir)
	var agent *prompts.Agent
	if agents.ActiveAgent(agentState) == nil && len(agentState.Agents) > 0 {
		agentState.Active = agentState.Agents[0].Name
		_ = agents.Save(projectDir, agentState)
	}
	if a := agents.ActiveAgent(agentState); a != nil {
		agent = &prompts.Agent{Name: a.Name, Description: a.Description}
	}

	m := &Model{
		gateway:         gw,
		tools:           toolRegistry,
		mem:             mem,
		structure:       st,
		projectDir:      projectDir,
		maxSteps:        maxSteps,
		allowDangerous:  allowDangerous,
		modelID:         modelID,
		agent:           agent,
		sessionID:       sessions.NewSessionID(),
		historyIndex:    -1,
		diagramsEnabled: true,
		agentsState:     agentState,
		sendFn:          func(tea.Msg) {}, // no-op until Main wires the program
		agentStyle:      lipgloss.NewStyle().Background(lipgloss.Color("24")).Foreground(lipgloss.Color("15")).Bold(true),
		rightStyle:      lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		dimStyle:        lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		errorStyle:      lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		userLabelStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("222")).Bold(true),
		assistantLabel:  lipgloss.NewStyle().Foreground(lipgloss.Color("79")).Bold(true),
	}
	m.input = textarea.New()
	m.input.Placeholder = "Message kaal — /help for commands"
	m.input.ShowLineNumbers = false
	m.input.CharLimit = 0
	m.input.SetWidth(80)
	m.input.Focus()
	m.viewport = viewport.New(80, 20)
	m.refreshMemory()
	m.refreshSessions()
	return m, nil
}

// -- tea lifecycle ---------------------------------------------------------------

func (m *Model) Init() tea.Cmd {
	return tea.Tick(clockTick, func(time.Time) tea.Msg { return clockTickMsg{} })
}

// Submit starts a turn; returns the follow-up tick cmds (thinking animation
// + rate). The loop runs on its own goroutine; events marshal via sendFn.
func (m *Model) Submit(task string) tea.Cmd {
	task = strings.TrimSpace(task)
	if task == "" || m.turnActive {
		return nil
	}
	m.appendUserBlock(task)
	m.promptHistory = append(m.promptHistory, task)
	m.historyIndex = -1
	m.input.Reset()
	m.steps = 0
	m.turnActive = true
	m.thinking = true
	m.thinkFrame = 0
	m.mdPending = ""
	m.flushArmed = false
	m.streamChars = 0
	m.lastRateChars = 0
	m.rate = 0
	m.turnStart = time.Now()
	m.lastTickTime = m.turnStart

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel

	agentLoop := loop.NewAgentLoop(
		m.gateway, m.tools, m.mem, m.sessionID,
		loop.WithMaxSteps(m.maxSteps),
		loop.WithAllowDangerous(m.allowDangerous),
		loop.WithResume(m.resumeNext),
		loop.WithStructure(m.structure),
		loop.WithAgent(m.agent),
		loop.WithAskHandler(m.askHandler),
		loop.WithContext(ctx),
	)
	m.resumeNext = false
	send := m.sendFn
	go func() {
		emit := func(ev loop.AgentEvent) {
			send(turnEventMsg{ev: ev})
		}
		_, err := agentLoop.Run(task, emit)
		send(turnDoneMsg{err: err})
	}()
	return tea.Batch(
		tea.Tick(thinkingTick, func(t time.Time) tea.Msg { return thinkTickMsg{frame: 0} }),
		tea.Tick(rateTick, func(time.Time) tea.Msg { return rateTickMsg{} }),
	)
}

// askHandler runs ON the loop goroutine: it asks the main program to open
// the ask modal and blocks until the answer arrives.
func (m *Model) askHandler(question string, options []string) string {
	answerCh := make(chan string, 1)
	m.sendFn(openAskMsg{question: question, options: options, answerCh: answerCh})
	select {
	case answer := <-answerCh:
		return answer
	case <-time.After(24 * time.Hour):
		return "(cancelled)"
	}
}

// -- update ----------------------------------------------------------------------

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.renderConversation()
	case tea.KeyMsg:
		if cmd := m.handleKey(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case turnEventMsg:
		if cmd := m.onLoopEvent(msg.ev); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case turnDoneMsg:
		m.onTurnDone(msg.err)
	case flushMDMsg:
		m.flushArmed = false
		m.flushMD()
	case rateTickMsg:
		if m.turnActive {
			m.updateRate()
			cmds = append(cmds, tea.Tick(rateTick, func(time.Time) tea.Msg { return rateTickMsg{} }))
		}
	case clockTickMsg:
		cmds = append(cmds, tea.Tick(clockTick, func(time.Time) tea.Msg { return clockTickMsg{} }))
	case thinkTickMsg:
		if m.thinking && m.turnActive {
			m.thinkFrame = msg.frame + 1
			cmds = append(cmds, tea.Tick(thinkingTick, func(t time.Time) tea.Msg {
				return thinkTickMsg{frame: m.thinkFrame}
			}))
		}
	case openAskMsg:
		m.openAskModal(msg)
	case diagramArtMsg:
		m.onDiagramArt(msg)
	case agentDoneMsg:
		m.onAgentGenerated(msg.reply, msg.phrase)
	case agentErrorMsg:
		m.onAgentGeneratorError(msg.message)
	}
	return m, tea.Batch(cmds...)
}

// parseAgentJSON extracts a {name, description} agent from a generator
// reply: json first; on failure, tolerantly grabs the first { to the last }
// (models love surrounding prose). nil when no usable name survives.
func parseAgentJSON(reply string) *agents.Agent {
	text := strings.TrimSpace(reply)
	var candidates []string
	candidates = append(candidates, text)
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			candidates = append(candidates, text[i:j+1])
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		var parsed agents.Agent
		if err := json.Unmarshal([]byte(c), &parsed); err != nil {
			continue
		}
		parsed.Name = strings.TrimSpace(parsed.Name)
		if parsed.Name != "" {
			parsed.Description = strings.TrimSpace(parsed.Description)
			return &parsed
		}
	}
	return nil
}

// generateAgent runs one agent-generation completion on a worker goroutine
// (Ctrl+G and the /agents -> n form share this). On success the new agent is
// added + activated + persisted; on failure a notice is written. Re-entry
// guard: while a generator is in flight (or a turn is active) a second start
// is refused.
func (m *Model) generateAgent(prompt, systemPrompt, phrase string) {
	if m.generatingAgent || m.turnActive {
		m.appendNotice("agent generator: already running")
		return
	}
	m.generatingAgent = true
	m.thinking = true
	send := m.sendFn
	go func() {
		msgs := []any{
			messages.WireSystem{Role: "system", Content: systemPrompt},
			messages.WireUser{Role: "user", Content: prompt},
		}
		var reply strings.Builder
		for ev := range m.gateway.Stream(context.Background(), msgs, nil, agentGeneratorMaxTokens) {
			switch ev.Kind {
			case gateway.EventContent:
				reply.WriteString(ev.Text)
			case gateway.EventError:
				send(agentErrorMsg{message: ev.Text})
				return
			}
		}
		send(agentDoneMsg{reply: reply.String(), phrase: phrase})
	}()
}

func (m *Model) onAgentGenerated(reply, phrase string) {
	m.generatingAgent = false
	m.thinking = false
	agent := parseAgentJSON(reply)
	if agent == nil {
		m.appendError("agent generator: could not parse a name/description")
		return
	}
	m.addAgent(*agent, "agent: "+agent.Name+" "+phrase)
}

func (m *Model) onAgentGeneratorError(message string) {
	m.generatingAgent = false
	m.thinking = false
	m.appendError("agent generator: " + message)
}

// addAgent appends, activates, and persists an agent.
func (m *Model) addAgent(agent agents.Agent, notice string) {
	state := agents.Load(m.projectDir)
	state.Agents = append(state.Agents, agent)
	state.Active = agent.Name
	_ = agents.Save(m.projectDir, state)
	m.agentsState = state
	m.agent = &prompts.Agent{Name: agent.Name, Description: agent.Description}
	m.appendNotice(notice)
}

// deleteAgent removes an agent by name and persists.
func (m *Model) deleteAgent(name string) {
	state := agents.Load(m.projectDir)
	kept := state.Agents[:0]
	for _, a := range state.Agents {
		if a.Name != name {
			kept = append(kept, a)
		}
	}
	state.Agents = kept
	if state.Active == name {
		state.Active = ""
		if len(kept) > 0 {
			state.Active = kept[0].Name
			m.agent = &prompts.Agent{Name: kept[0].Name, Description: kept[0].Description}
		} else {
			m.agent = nil
		}
	}
	_ = agents.Save(m.projectDir, state)
	m.agentsState = state
	m.appendNotice("agent deleted: " + name)
}

// activateAgent persists the active persona and updates the status bar.
func (m *Model) activateAgent(name string) {
	state := agents.Load(m.projectDir)
	state.Active = name
	_ = agents.Save(m.projectDir, state)
	m.agentsState = state
	for i := range state.Agents {
		if state.Agents[i].Name == name {
			m.agent = &prompts.Agent{Name: state.Agents[i].Name, Description: state.Agents[i].Description}
		}
	}
	m.appendNotice("agent: " + name + " active")
}

// handleKey routes one keypress; returns follow-up cmds.
func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if m.modal != nil {
		return m.handleModalKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		if m.turnActive && m.cancelTurn != nil {
			m.cancelTurn() // hard cancel: the in-flight stream aborts
			m.thinking = false
			m.appendNotice("⏹ turn cancelled")
		}
	case "ctrl+q":
		return tea.Quit
	case "ctrl+p":
		m.historyPrev()
	case "ctrl+n":
		m.historyNext()
	case "ctrl+l":
		m.viewport.GotoBottom()
	case "ctrl+s":
		m.sidebarVisible = !m.sidebarVisible
		m.resize()
	case "ctrl+t":
		m.topbarVisible = !m.topbarVisible
	case "ctrl+d":
		m.diagramsEnabled = !m.diagramsEnabled
		if m.diagramsEnabled {
			m.appendNotice("diagrams on")
		} else {
			m.appendNotice("diagrams off")
		}
	case "ctrl+g":
		if m.generatingAgent {
			m.appendNotice("agent generator: already running")
			return nil
		}
		if m.turnActive {
			m.appendNotice("(busy — wait for the current turn)")
			return nil
		}
		if m.modal != nil {
			return nil // a modal is already up; don't stack another
		}
		m.openAgentIntentModal()
	case "tab", "shift+tab":
		if m.suggestionsVisible && len(m.suggestions) > 0 {
			if msg.String() == "shift+tab" {
				m.suggestIndex = (m.suggestIndex - 1 + len(m.suggestions)) % len(m.suggestions)
			} else {
				m.suggestIndex = (m.suggestIndex + 1) % len(m.suggestions)
			}
			m.input.SetValue(m.suggestions[m.suggestIndex])
			m.input.CursorEnd()
			return nil
		}
		m.input.InsertString("\t")
		return nil
	case "enter":
		if !isShift(msg) {
			return m.Submit(m.input.Value())
		}
		m.input.InsertString("\n")
	}
	// Everything else goes to the composer textarea.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.updateSuggestions()
	return cmd
}

// updateSuggestions shows the slash-command popup when the input starts
// with "/".
func (m *Model) updateSuggestions() {
	value := m.input.Value()
	if strings.HasPrefix(value, "/") {
		filter := strings.ToLower(value[1:])
		var matches []string
		for _, c := range slashCommands {
			if strings.Contains(c, "/"+filter) {
				matches = append(matches, c)
			}
		}
		m.suggestions = matches
		m.suggestionsVisible = len(matches) > 0
		if m.suggestIndex >= len(m.suggestions) {
			m.suggestIndex = 0
		}
	} else {
		m.suggestionsVisible = false
		m.suggestions = nil
		m.suggestIndex = 0
	}
}

// -- slash commands ---------------------------------------------------------------

func (m *Model) runCommand(text string) {
	parts := strings.SplitN(strings.TrimSpace(text), " ", 2)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}
	switch cmd {
	case "/help":
		m.appendNotice("commands: /help /new /resume <id> /sessions /models /connect /memory /model /verbose /sidebar /structure /quit")
		m.appendNotice("keys: enter send · shift+enter newline · ctrl+p/n history · ctrl+l bottom · ctrl+s sidebar · ctrl+c cancel · ctrl+q quit")
	case "/new":
		m.sessionID = sessions.NewSessionID()
		m.resumeNext = false
		m.blocks = nil
		m.traceLines = nil
		m.steps = 0
		m.totalUsage = loop.Usage{}
		m.totalCost = 0
		m.renderConversation()
		m.refreshSessions()
	case "/resume":
		if arg == "" {
			m.appendNotice("usage: /resume <session-id>")
			return
		}
		m.resumeSession(arg)
	case "/sessions":
		entries := sessions.ListSessions()
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			if id, ok := e["id"].(string); ok {
				ids = append(ids, id)
			}
		}
		// newest first
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
		m.openListModal(modalSessions, "Sessions", ids)
	case "/models":
		ids := make([]string, 0, len(config.Models))
		for _, model := range config.Models {
			ids = append(ids, model.ID)
		}
		m.openListModal(modalModels, "Models", ids)
	case "/agents":
		state := agents.Load(m.projectDir)
		m.agentsState = state
		names := agents.AgentNames(state)
		m.openListModal(modalAgents, "Agents", names)
	case "/diagram":
		if arg == "" {
			m.appendNotice("usage: /diagram <file.mmd>")
			return
		}
		m.renderMermaidFile(arg)
	case "/diagrams":
		m.diagramsEnabled = !m.diagramsEnabled
		if m.diagramsEnabled {
			m.appendNotice("diagrams on")
		} else {
			m.appendNotice("diagrams off")
		}
	case "/topbar":
		m.topbarVisible = !m.topbarVisible
	case "/connect":
		if arg != "" {
			_ = config.SaveUserAPIKey(arg) // inline key, no popup
			m.appendNotice("api key saved")
		} else {
			m.openConnectModal()
		}
	case "/memory":
		digest := m.mem.LoadDigest()
		if digest == "" {
			m.appendNotice("(memory empty)")
		} else {
			m.appendNotice(digest)
		}
		m.refreshMemory()
	case "/model":
		m.appendNotice(m.modelID)
	case "/verbose":
		m.verbose = !m.verbose
		if m.verbose {
			m.appendNotice("verbose on")
		} else {
			m.appendNotice("verbose off")
		}
	case "/sidebar":
		m.sidebarVisible = !m.sidebarVisible
		m.resize()
	case "/structure":
		doc := m.structure.Refresh()
		m.appendNotice("structure: " + m.structure.CachePath())
		for _, line := range strings.Split(doc, "\n")[:100] {
			m.appendNotice(line)
		}
	case "/quit":
		m.sendFn(tea.Quit())
	default:
		m.appendNotice("unknown command: " + cmd + " (try /help)")
	}
}

// -- turn events -------------------------------------------------------------------

func (m *Model) onLoopEvent(ev loop.AgentEvent) tea.Cmd {
	switch ev.Kind {
	case loop.EventStep:
		m.steps = ev.Step
		if m.steps > 1 {
			// A new model generation started: flush the previous one's
			// streaming markdown and open a fresh block.
			m.flushMD()
		}
		m.thinking = true
	case loop.EventContent:
		m.streamChars += len(ev.Text)
		m.transcript = append(m.transcript, ev.Text)
		m.mdPending += ev.Text
		if len(m.mdPending) < mdInstantFlushChars {
			m.flushMD()
		} else if !m.flushArmed {
			m.flushArmed = true
			return m.armFlush()
		}
	case loop.EventReasoning:
		m.thinking = true
		if m.verbose {
			m.transcript = append(m.transcript, "[think] "+ev.Text+"\n")
			m.appendNotice("[think] " + ev.Text)
		}
	case loop.EventToolStart:
		m.flushMD()
		m.blocks = append(m.blocks, block{kind: blockTool, text: "⚙ " + ev.Call.Name})
		m.traceLines = append(m.traceLines, fmt.Sprintf("⚙ %s %s", ev.Call.Name, ev.Call.Arguments))
		m.renderConversation()
		m.thinking = false
	case loop.EventToolResult:
		m.flushMD()
		preview := ev.Text
		if len(preview) > maxResultPreview {
			preview = preview[:maxResultPreview] + "…"
		}
		m.blocks = append(m.blocks, block{kind: blockTool, text: "  ✓ " + preview})
		m.traceLines = append(m.traceLines, fmt.Sprintf("✓ %s (%d chars)", ev.ToolCallID, len(ev.Text)))
		m.renderConversation()
	case loop.EventVerify:
		preview := ev.Text
		if len(preview) > maxResultPreview {
			preview = preview[:maxResultPreview] + "…"
		}
		m.appendNotice("🧪 verify: " + preview)
	case loop.EventError:
		m.appendError(ev.Text)
		m.flushMD()
	}
	return nil
}

func (m *Model) onTurnDone(err error) {
	m.thinking = false
	m.flushMD()
	m.closeUnclosedFence()
	m.renderConversation()
	if err != nil && !errors.Is(err, loop.ErrCancelled) {
		m.appendError(err.Error())
		m.renderConversation()
	} else if errors.Is(err, loop.ErrCancelled) {
		m.appendNotice("(partial turn discarded)")
	}
	m.turnActive = false
	if m.cancelTurn != nil {
		m.cancelTurn()
		m.cancelTurn = nil
	}
	m.input.Focus()
	m.viewport.GotoBottom()
	m.renderMermaidDiagrams()
}

// -- rendering helpers ----------------------------------------------------------------

func (m *Model) resize() {
	sidebar := 0
	if m.sidebarVisible {
		sidebar = sidebarWidth
	}
	convW := m.width - sidebar
	if convW < 20 {
		convW = 20
	}
	statusH := 1
	composerH := 4
	convH := m.height - statusH - composerH
	if convH < 5 {
		convH = 5
	}
	m.viewport.Width = convW
	m.viewport.Height = convH
	m.input.SetWidth(convW - 4)
	if m.input.Height() > maxComposerLines {
		m.input.SetHeight(maxComposerLines)
	}
	m.rebuildRenderer()
}

func (m *Model) rebuildRenderer() {
	width := m.width - 2
	if m.sidebarVisible {
		width -= sidebarWidth
	}
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err == nil {
		m.mdRenderer = r
	}
}

func (m *Model) renderMarkdown(text string) string {
	if m.mdRenderer == nil {
		return text
	}
	rendered, err := m.mdRenderer.Render(text)
	if err != nil {
		return text
	}
	return rendered
}

// renderConversation rebuilds the viewport content from the blocks.
func (m *Model) renderConversation() {
	var sb strings.Builder
	for _, b := range m.blocks {
		switch b.kind {
		case blockUser:
			sb.WriteString(m.userLabelStyle.Render("▌ you"))
			sb.WriteString("\n")
			sb.WriteString(m.renderMarkdown(b.text))
		case blockAssistant:
			sb.WriteString(m.assistantLabel.Render("▌ kaal"))
			sb.WriteString("\n")
			sb.WriteString(m.renderMarkdown(b.text))
		case blockTool:
			sb.WriteString(m.dimStyle.Render(b.text))
			sb.WriteString("\n")
		case blockNotice:
			sb.WriteString(m.dimStyle.Render(b.text))
			sb.WriteString("\n")
		case blockError:
			sb.WriteString(m.errorStyle.Render(b.text))
			sb.WriteString("\n")
		}
	}
	if m.thinking && m.turnActive {
		sb.WriteString(m.dimStyle.Render(spinnerFrames[m.thinkFrame%len(spinnerFrames)] + " thinking"))
		sb.WriteString("\n")
	}
	m.viewport.SetContent(sb.String())
	m.viewport.GotoBottom()
}

func (m *Model) appendUserBlock(text string) {
	m.transcript = append(m.transcript, "▌ you\n"+text+"\n")
	m.blocks = append(m.blocks, block{kind: blockUser, text: text})
	m.renderConversation()
}

func (m *Model) flushMD() {
	if m.mdPending == "" {
		return
	}
	if len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].kind != blockAssistant {
		m.blocks = append(m.blocks, block{kind: blockAssistant})
	}
	m.blocks[len(m.blocks)-1].text += m.mdPending
	m.mdPending = ""
	m.renderConversation()
}

func (m *Model) armFlush() tea.Cmd {
	interval := mdFlushSeconds
	if m.turnMDLen() > mdLongTurnChars {
		interval = mdFlushSecondsLong
	}
	delay := time.Duration(interval * float64(time.Second))
	return tea.Tick(delay, func(time.Time) tea.Msg { return flushMDMsg{} })
}

func (m *Model) turnMDLen() int {
	n := 0
	for _, b := range m.blocks {
		if b.kind == blockAssistant {
			n += len(b.text)
		}
	}
	return n
}

// closeUnclosedFence appends a closing fence when the turn's markdown has an
// unbalanced code fence (the Python _close_unclosed_fence).
func (m *Model) closeUnclosedFence() {
	text := ""
	for _, b := range m.blocks {
		if b.kind == blockAssistant {
			text += b.text
		}
	}
	if strings.Count(text, "```")%2 == 1 {
		if len(m.blocks) == 0 || m.blocks[len(m.blocks)-1].kind != blockAssistant {
			m.blocks = append(m.blocks, block{kind: blockAssistant})
		}
		m.blocks[len(m.blocks)-1].text += "\n```\n"
	}
}

func (m *Model) appendNotice(text string) {
	m.transcript = append(m.transcript, text+"\n")
	m.blocks = append(m.blocks, block{kind: blockNotice, text: text})
	m.renderConversation()
}

func (m *Model) appendError(text string) {
	m.transcript = append(m.transcript, "error: "+text+"\n")
	m.blocks = append(m.blocks, block{kind: blockError, text: "error: " + text})
	m.renderConversation()
}

// -- mermaid rendering ----------------------------------------------------------------

// renderMermaidDiagrams auto-renders ```mermaid fences at turn end: each
// fence goes to a worker goroutine (termaid) and the art lands below the
// answer. Skipped when diagrams are toggled off or termaid is missing (a
// notice replaces the art).
func (m *Model) renderMermaidDiagrams() {
	if !m.diagramsEnabled {
		return
	}
	var text strings.Builder
	for _, b := range m.blocks {
		if b.kind == blockAssistant {
			text.WriteString(b.text)
		}
	}
	fences := mermaidFenceRe.FindAllStringSubmatch(text.String(), -1)
	if len(fences) == 0 {
		return
	}
	send := m.sendFn
	for _, f := range fences {
		body := f[1]
		go func(body string) {
			art, err := renderMermaid(body)
			send(diagramArtMsg{text: art, err: err})
		}(body)
	}
}

// renderMermaidFile renders one .mmd file via termaid (/diagram).
func (m *Model) renderMermaidFile(path string) {
	send := m.sendFn
	go func() {
		out, err := exec.Command("termaid", path).CombinedOutput()
		send(diagramArtMsg{text: string(out), err: err})
	}()
}

// renderMermaid runs termaid on a fence body (written to a temp .mmd).
func renderMermaid(body string) (string, error) {
	if _, err := exec.LookPath("termaid"); err != nil {
		return "", errors.New("termaid not found — install it with: uv tool install termaid")
	}
	tmp, err := os.CreateTemp("", "kaal-diagram-*.mmd")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	out, err := exec.Command("termaid", name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("termaid: %s", strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (m *Model) onDiagramArt(msg diagramArtMsg) {
	if msg.err != nil {
		m.appendNotice("diagram: " + msg.err.Error())
		return
	}
	text := strings.TrimRight(msg.text, "\n")
	if text == "" {
		m.appendNotice("diagram: (empty)")
		return
	}
	m.blocks = append(m.blocks, block{kind: blockNotice, text: text})
	m.renderConversation()
}

// -- history -------------------------------------------------------------------------

func (m *Model) historyPrev() {
	if len(m.promptHistory) == 0 {
		return
	}
	if m.historyIndex == -1 {
		m.draft = m.input.Value()
		m.historyIndex = len(m.promptHistory) - 1
	} else if m.historyIndex > 0 {
		m.historyIndex--
	}
	m.input.SetValue(m.promptHistory[m.historyIndex])
	m.input.CursorEnd()
}

func (m *Model) historyNext() {
	if m.historyIndex == -1 {
		return
	}
	m.historyIndex++
	if m.historyIndex >= len(m.promptHistory) {
		m.historyIndex = -1
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.promptHistory[m.historyIndex])
	}
	m.input.CursorEnd()
}

// -- sidebar ---------------------------------------------------------------------------

func (m *Model) refreshMemory() {
	m.memoryText = m.mem.LoadDigest()
}

func (m *Model) refreshSessions() {
	m.sessionEntries = sessions.ListSessions()
}

func (m *Model) sidebarView() string {
	switch m.sidebarTab {
	case 0:
		if len(m.traceLines) == 0 {
			return m.dimStyle.Render("(no tool activity)")
		}
		return strings.Join(m.traceLines, "\n")
	case 1:
		if m.memoryText == "" {
			return m.dimStyle.Render("(memory empty)")
		}
		return m.memoryText
	default:
		if len(m.sessionEntries) == 0 {
			return m.dimStyle.Render("(no sessions)")
		}
		lines := make([]string, 0, len(m.sessionEntries))
		for _, e := range m.sessionEntries {
			id, _ := e["id"].(string)
			prompt, _ := e["prompt"].(string)
			if len(prompt) > 24 {
				prompt = prompt[:24] + "…"
			}
			lines = append(lines, fmt.Sprintf("%s  %s", id, prompt))
		}
		return strings.Join(lines, "\n")
	}
}

// -- status bar --------------------------------------------------------------------------

func (m *Model) statusBar() string {
	agentName := "—"
	if m.agent != nil {
		agentName = m.agent.Name
	}
	clock := time.Now().Format("Mon 02 Jan 15:04")
	rate := m.tools.CacheHitRate()
	cache := "cache –"
	if rate >= 0 {
		cache = fmt.Sprintf("cache %.0f%%", rate*100)
	}
	var sb strings.Builder
	sb.WriteString(m.agentStyle.Render(" " + agentName + " "))
	sb.WriteString("│")
	sb.WriteString(fmt.Sprintf(" step %d/%d ", m.steps, m.maxSteps))
	sb.WriteString("│")
	sb.WriteString(fmt.Sprintf(" %.0f tok/s ", m.rate))
	sb.WriteString("│")
	sb.WriteString(" " + cache + " ")
	sb.WriteString("│")
	sb.WriteString(fmt.Sprintf(" $%.4f ", m.totalCost))
	sb.WriteString("│")
	sb.WriteString(m.rightStyle.Render(" " + clock + " "))
	return sb.String()
}

func (m *Model) updateRate() {
	now := time.Now()
	elapsed := now.Sub(m.lastTickTime).Seconds()
	chars := m.streamChars - m.lastRateChars
	if elapsed > 0 {
		m.rate = float64(chars) / 3 / elapsed
	}
	m.lastRateChars = m.streamChars
	m.lastTickTime = now
}

// -- modals -------------------------------------------------------------------------------

func (m *Model) openListModal(kind modalKind, title string, items []string) {
	m.modal = &modal{kind: kind, title: title, items: items, allItems: items}
	if kind == modalModels {
		in := textarea.New()
		in.Placeholder = "filter models…"
		in.ShowLineNumbers = false
		in.SetWidth(40)
		in.Focus()
		m.modal.input = &in
	}
}

func (m *Model) openConnectModal() {
	in := textarea.New()
	in.Placeholder = "paste API key"
	in.ShowLineNumbers = false
	in.SetWidth(50)
	in.Focus()
	m.modal = &modal{kind: modalConnect, title: "Connect", input: &in}
}

func (m *Model) openAgentIntentModal() {
	in := textarea.New()
	in.Placeholder = "describe the agent you want…"
	in.ShowLineNumbers = false
	in.SetWidth(60)
	in.Focus()
	m.modal = &modal{kind: modalAgentIntent, title: "New agent (AI)", input: &in}
}

func (m *Model) openAgentFormModal() {
	name := textarea.New()
	name.Placeholder = "agent name"
	name.ShowLineNumbers = false
	name.SetWidth(40)
	name.Focus()
	desc := textarea.New()
	desc.Placeholder = "one or two sentences about the persona"
	desc.ShowLineNumbers = false
	desc.SetWidth(60)
	m.modal = &modal{kind: modalAgentForm, title: "New agent", input: &name, input2: &desc, formFocus: 0}
}

func (m *Model) openAskModal(msg openAskMsg) {
	in := textarea.New()
	in.Placeholder = "answer…"
	in.ShowLineNumbers = false
	in.SetWidth(50)
	in.Focus()
	m.modal = &modal{
		kind: modalAsk, title: msg.question, input: &in,
		askOptions: msg.options, askChan: msg.answerCh,
	}
}

func (m *Model) handleModalKey(msg tea.KeyMsg) tea.Cmd {
	mod := m.modal
	if mod == nil {
		return nil
	}
	switch mod.kind {
	case modalSessions, modalModels:
		switch msg.String() {
		case "up", "k":
			if mod.cursor > 0 {
				mod.cursor--
			}
		case "down", "j":
			if mod.cursor < len(mod.items)-1 {
				mod.cursor++
			}
		case "enter":
			if mod.cursor < len(mod.items) {
				m.closeModal(mod.items[mod.cursor])
			}
		case "esc":
			m.closeModal(nil)
		default:
			if mod.kind == modalModels && mod.input != nil {
				prev := mod.filter
				var cmd tea.Cmd
				*mod.input, cmd = mod.input.Update(msg)
				m.filterModels()
				if mod.filter != prev {
					mod.cursor = 0
				}
				return cmd
			}
		}
	case modalAgents:
		switch msg.String() {
		case "up", "k":
			if mod.cursor > 0 {
				mod.cursor--
			}
		case "down", "j":
			if mod.cursor < len(mod.items)-1 {
				mod.cursor++
			}
		case "enter":
			if mod.cursor < len(mod.items) {
				m.closeModal(mod.items[mod.cursor])
			}
		case "n":
			m.openAgentFormModal()
		case "d":
			if mod.cursor < len(mod.items) {
				name := mod.items[mod.cursor]
				m.deleteAgent(name)
				mod.items = agents.AgentNames(m.agentsState)
				if mod.cursor >= len(mod.items) {
					mod.cursor = len(mod.items) - 1
				}
			}
		case "esc":
			m.closeModal(nil)
		}
	case modalAgentForm:
		if mod.input == nil || mod.input2 == nil {
			return nil
		}
		switch msg.String() {
		case "tab", "shift+tab":
			mod.formFocus = 1 - mod.formFocus
			if mod.formFocus == 0 {
				mod.input.Focus()
				mod.input2.Blur()
			} else {
				mod.input2.Focus()
				mod.input.Blur()
			}
		case "enter":
			if !isShift(msg) {
				name := strings.TrimSpace(mod.input.Value())
				desc := strings.TrimSpace(mod.input2.Value())
				if name == "" {
					m.appendNotice("agent form: name required")
					return nil
				}
				m.closeModal(agents.Agent{Name: name, Description: desc})
			} else {
				if mod.formFocus == 0 {
					mod.input.InsertString("\n")
				} else {
					mod.input2.InsertString("\n")
				}
			}
		case "esc":
			m.closeModal(nil)
		default:
			var cmd tea.Cmd
			if mod.formFocus == 0 {
				*mod.input, cmd = mod.input.Update(msg)
			} else {
				*mod.input2, cmd = mod.input2.Update(msg)
			}
			return cmd
		}
	case modalAgentIntent:
		if mod.input == nil {
			return nil
		}
		switch msg.String() {
		case "enter":
			if !isShift(msg) {
				m.closeModal(mod.input.Value())
			} else {
				mod.input.InsertString("\n")
			}
		case "esc":
			m.closeModal(nil)
		default:
			var cmd tea.Cmd
			*mod.input, cmd = mod.input.Update(msg)
			return cmd
		}
	case modalConnect, modalAsk:
		if mod.input == nil {
			return nil
		}
		switch msg.String() {
		case "enter":
			if !isShift(msg) {
				m.closeModal(mod.input.Value())
			} else {
				mod.input.InsertString("\n")
			}
		case "esc":
			m.closeModal("")
		default:
			if mod.kind == modalAsk && len(mod.askOptions) > 0 && msg.Type == tea.KeyRunes {
				for i := range mod.askOptions {
					if msg.String() == fmt.Sprint(i+1) {
						m.closeModal(mod.askOptions[i])
						return nil
					}
				}
			}
			var cmd tea.Cmd
			*mod.input, cmd = mod.input.Update(msg)
			return cmd
		}
	}
	return nil
}

func (m *Model) filterModels() {
	if m.modal == nil || m.modal.kind != modalModels {
		return
	}
	filter := strings.ToLower(m.modal.input.Value())
	m.modal.filter = filter
	if filter == "" {
		m.modal.items = m.modal.allItems
		return
	}
	var filtered []string
	for _, id := range m.modal.allItems {
		if strings.Contains(strings.ToLower(id), filter) {
			filtered = append(filtered, id)
		}
	}
	m.modal.items = filtered
}

func (m *Model) closeModal(value any) {
	mod := m.modal
	m.modal = nil
	if mod == nil {
		return
	}
	switch mod.kind {
	case modalAsk:
		answer, _ := value.(string)
		if answer == "" {
			answer = "(cancelled)"
		}
		if mod.askChan != nil {
			mod.askChan <- answer
		}
	case modalSessions:
		if sid, ok := value.(string); ok {
			m.resumeSession(sid)
		}
	case modalModels:
		if id, ok := value.(string); ok {
			m.setModel(id)
		}
	case modalConnect:
		if key, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			_ = config.SaveUserAPIKey(strings.TrimSpace(key))
			m.appendNotice("api key saved")
		}
	case modalAgents:
		if name, ok := value.(string); ok {
			m.activateAgent(name)
		}
	case modalAgentForm:
		if agent, ok := value.(agents.Agent); ok {
			m.addAgent(agent, "agent: "+agent.Name+" added and active")
		}
	case modalAgentIntent:
		if intent, ok := value.(string); ok && strings.TrimSpace(intent) != "" {
			m.generateAgent(strings.TrimSpace(intent), AgentGeneratorSystemPrompt, "generated and active")
		}
	}
	m.input.Focus()
}

// -- session / model actions ---------------------------------------------------------------

func (m *Model) resumeSession(sid string) {
	m.sessionID = sid
	m.resumeNext = true
	m.blocks = nil
	m.traceLines = nil
	m.steps = 0
	m.renderConversation()
	m.appendNotice("resumed " + sid)
}

func (m *Model) setModel(modelID string) {
	m.modelID = modelID
	_ = config.SaveUserModel(modelID)
	m.appendNotice("model: " + modelID)
}

// -- view ----------------------------------------------------------------------------------

func (m *Model) View() string {
	if m.width == 0 {
		return "kaal — resize the terminal to begin\n"
	}
	topbar := ""
	if m.topbarVisible {
		model := m.modelID
		session := m.sessionID
		if len(session) > 15 {
			session = session[:15]
		}
		brand := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("79")).Render("KAAL")
		identity := m.dimStyle.Render("  " + model + " · " + session + "  ")
		actions := m.dimStyle.Render("[New chat] [Sessions] [Agents]")
		topbar = lipgloss.NewStyle().Width(m.width).BorderBottom(true).Render(
			brand+identity+actions,
		) + "\n"
	}
	if m.modal != nil {
		return topbar + m.modalView()
	}
	sidebar := ""
	if m.sidebarVisible {
		header := lipgloss.NewStyle().Bold(true).Render("Workspace")
		tabs := ""
		for i, name := range []string{"Trace", "Memory", "Sessions"} {
			if i == m.sidebarTab {
				tabs += lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("79")).Render(" " + name + " ")
			} else {
				tabs += m.dimStyle.Render(" " + name + " ")
			}
		}
		sidebar = lipgloss.NewStyle().Width(sidebarWidth).BorderLeft(true).Render(
			header + "\n" + tabs + "\n" + m.sidebarView(),
		)
	}

	state := "ready"
	if m.turnActive {
		state = "busy — ctrl+c cancels"
	}
	composer := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("Message kaal · ") +
		lipgloss.NewStyle().Foreground(lipgloss.Color("222")).Render(state) + "\n" +
		m.input.View()

	suggestions := ""
	if m.suggestionsVisible && len(m.suggestions) > 0 {
		var sb strings.Builder
		for i, cmd := range m.suggestions {
			if i == m.suggestIndex {
				sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("79")).Render("  " + cmd))
			} else {
				sb.WriteString(m.dimStyle.Render("  " + cmd))
			}
			sb.WriteString("\n")
		}
		suggestions = lipgloss.NewStyle().Width(m.width).BorderTop(true).Render(
			strings.TrimRight(sb.String(), "\n"),
		) + "\n"
	}

	status := m.statusBar()

	return topbar +
		lipgloss.JoinHorizontal(lipgloss.Top, m.viewport.View(), sidebar) + "\n" +
		suggestions +
		composer + "\n" +
		status
}

// modalView renders the active modal (sessions/models/agents lists, connect
// key entry, agent form, ask question) instead of the composer.
func (m *Model) modalView() string {
	mod := m.modal
	if mod == nil {
		return ""
	}
	title := lipgloss.NewStyle().Bold(true).Render(mod.title)
	var body strings.Builder
	switch mod.kind {
	case modalSessions, modalAgents:
		if len(mod.items) == 0 {
			body.WriteString(m.dimStyle.Render("(empty)"))
			break
		}
		for i, item := range mod.items {
			if i == mod.cursor {
				body.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("79")).Render("▸ " + item))
			} else {
				body.WriteString("  " + item)
			}
			body.WriteString("\n")
		}
		if mod.kind == modalAgents {
			body.WriteString(m.dimStyle.Render("enter activate · n new · d delete · esc close"))
		}
	case modalModels:
		if mod.input != nil {
			body.WriteString(m.dimStyle.Render("filter: ") + mod.input.View() + "\n\n")
		}
		for i, id := range mod.items {
			price := modelPriceLine(id)
			line := id
			if price != "" {
				line += m.dimStyle.Render("  " + price)
			}
			if i == mod.cursor {
				body.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("79")).Render("▸ " + line))
			} else {
				body.WriteString("  " + line)
			}
			body.WriteString("\n")
		}
		body.WriteString(m.dimStyle.Render("enter select · esc close"))
	case modalConnect:
		if mod.input != nil {
			body.WriteString(m.dimStyle.Render("paste the API key (never displayed):") + "\n")
			body.WriteString(mod.input.View() + "\n")
			body.WriteString(m.dimStyle.Render("enter save · esc cancel"))
		}
	case modalAgentForm:
		if mod.input != nil && mod.input2 != nil {
			body.WriteString(m.dimStyle.Render("name:") + "\n")
			body.WriteString(mod.input.View() + "\n")
			body.WriteString(m.dimStyle.Render("description:") + "\n")
			body.WriteString(mod.input2.View() + "\n")
			body.WriteString(m.dimStyle.Render("tab switch · enter save · esc cancel"))
		}
	case modalAgentIntent:
		if mod.input != nil {
			body.WriteString(m.dimStyle.Render("describe the agent you want — the model invents it:") + "\n")
			body.WriteString(mod.input.View() + "\n")
			body.WriteString(m.dimStyle.Render("enter generate · esc cancel"))
		}
	case modalAsk:
		if mod.input != nil {
			body.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("222")).Render(mod.title) + "\n")
			for i, opt := range mod.askOptions {
				body.WriteString(m.dimStyle.Render(fmt.Sprintf("  %d. %s", i+1, opt)) + "\n")
			}
			body.WriteString(mod.input.View() + "\n")
			body.WriteString(m.dimStyle.Render("enter answer · esc cancel"))
		}
	}
	box := lipgloss.NewStyle().Width(m.width-4).Border(lipgloss.RoundedBorder()).Padding(0, 1).Render(
		title + "\n\n" + strings.TrimRight(body.String(), "\n"),
	)
	return box + "\n"
}

// modelPriceLine renders '($in/$out per M)' for a catalog model.
func modelPriceLine(id string) string {
	for _, model := range config.Models {
		if model.ID == id {
			return fmt.Sprintf("($%.2f/$%.2f per M)", model.InputPerM, model.OutputPerM)
		}
	}
	return ""
}

func isShift(msg tea.KeyMsg) bool {
	// bubbletea v1 has no modifier bits; shift+enter arrives as its own key
	// string.
	return msg.String() == "shift+enter"
}

// -- Main ------------------------------------------------------------------------------------

// Main launches the workbench (the default `kaal` surface). The alt screen
// restores the terminal on exit, then the resume hint prints.
func Main() int {
	m, err := New("", 20, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	program := tea.NewProgram(m, tea.WithAltScreen())
	m.sendFn = program.Send
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "kaal: tui:", err)
		return 1
	}
	fmt.Printf(
		"Session %s — resume with: kaal run --resume %s  (or /resume %s in the TUI)\n",
		m.sessionID, m.sessionID, m.sessionID,
	)
	return 0
}

// ToolCall aliases the wire tool-call type for the block renderer.
type ToolCall = messages.ToolCall
