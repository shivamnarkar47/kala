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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
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
	appPadding          = 4 // the whole window floats inside a 4-cell frame
)

// The KAAL wordmark in block glyphs (ported from the Python camp's art.py)
// — the home-screen hero, crowned by Arjuna's chariot (theme.go).
var kaalArt = `▄▄▄   ▄▄▄             ▄▄                ▄▄▄
███ ▄███▀             ██                ███            ▄▄
███████   ▄█▀█▄ ▄█▀▀▀ ████▄  ▀▀█▄ ██ ██ ███      ▄███▄ ██ ▄█▀
███▀███▄  ██▄█▀ ▀███▄ ██ ██ ▄█▀██ ██▄██ ███      ██ ██ ████
███  ▀███ ▀█▄▄▄ ▄▄▄█▀ ██ ██ ▀█▄██  ▀█▀  ████████ ▀███▀ ██ ▀█▄`

const (
	homeTitle   = "KURUKSHETRA"
	homeTagline = "the field of dharma — where five Pandava agents take their stand, one mastermind at the reins"
	homeWelcome = "ask a task, or /help for commands · ctrl+g invents an agent · /sessions resumes the past"
)

// AgentGeneratorSystemPrompt is the AI agent designer's system prompt
// (verbatim from tui.py): the model answers with ONLY a JSON persona.
const AgentGeneratorSystemPrompt = `You are an agent designer for a coding harness. The user describes an agent
persona they want. Respond with ONLY a JSON object: {"name": "<a strong,
fitting name — prefer Sanskrit/epic-flavored names in the spirit of the
Pandavas>", "description": "<one or two sentences describing the persona's
strengths and style>"}.`

const agentGeneratorMaxTokens = 300

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
	blockMeta
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
	modalProviders
	modalCustomProvider
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

// Provider picker items (raw values; modalView decorates).
const (
	pickOpencode   = "opencode"    // zen/v1 — the keyless free tier
	pickOpencodeGo = "opencode-go" // zen/go/v1 — the paid opencode route
	pickCommand    = "commandcode"
	pickAddAnother = "@add"
)

// keyProviderFor resolves which credential chain a picker choice needs:
// both opencode tiers share the OPENCODE_API_KEY chain.
func keyProviderFor(pick string) string {
	if pick == pickCommand {
		return config.ProviderCommandCode
	}
	return config.ProviderOpencode
}

// providerDisplayName prettifies a picker choice for titles and notices.
func providerDisplayName(pick string) string {
	switch pick {
	case pickOpencode:
		return "opencode free"
	case pickOpencodeGo:
		return "opencode go"
	case pickCommand:
		return "command-code"
	}
	return pick
}

// customProviderForm carries the add-another-provider fields out of its
// modal into closeModal.
type customProviderForm struct{ baseURL, apiKey string }

// customModelsMsg lands the fetched model list of a BYOK endpoint.
type customModelsMsg struct {
	name, baseURL, apiKey string
	ids                   []string
	err                   error
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
	keyMissing     bool // no API key resolved at startup — /connect makes it live
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
	width, height  int  // the terminal's true size
	innerW, innerH int  // inside the app frame (terminal minus 2×appPadding)
	follow         bool // autoscroll pinned to the newest line; released on scroll-up
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

	// slash-command suggestions popup (the palette)
	suggestionsVisible bool
	suggestions        []*slashCommand
	suggestIndex       int
	suggestDismissed   bool
	lastSuggestInput   string

	// mermaid auto-render + AI agent generator
	diagramsEnabled bool
	generatingAgent bool
	homeMirrored    bool

	// provider flow: the provider awaiting its key (picker → connect →
	// models), and whether an open models modal finalizes a BYOK choice.
	pendingProvider  string
	pickingForCustom bool

	// turn state
	turnActive    bool
	thinking      bool
	connecting    bool // waiting for the stream's first byte
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
	freeKeyless   bool // running a zen free-tier model with no login at all

	// styles
	rightStyle     lipgloss.Style
	dimStyle       lipgloss.Style
	errorStyle     lipgloss.Style
	userLabelStyle lipgloss.Style
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
	key, keyErr := config.GetAPIKeyFor(config.ModelProvider(modelID))
	// Free-tier zen models run with no login at all; anything else needs a
	// resolvable key before sending.
	keyMissing := keyErr != nil && !config.FreeTierModel(modelID)
	freeKeyless := keyErr != nil && config.FreeTierModel(modelID)
	if keyErr != nil {
		key = ""
	}
	gw := &gateway.Gateway{BaseURL: config.ModelBaseURL(modelID), APIKey: key, Model: modelID}
	m, err := NewWithGateway(gw, projectDir, modelID, maxSteps, allowDangerous)
	if err != nil {
		return nil, err
	}
	m.freeKeyless = freeKeyless
	if keyMissing {
		m.keyMissing = true
		m.appendNotice("no API key — save one with /connect <key> to start chatting")
	}
	return m, nil
}

// applySavedKey pushes a key saved via /connect into the live gateway and
// clears the missing-key state so the next turn can run. The store is scoped
// to the active model's provider. No-op when no key is stored yet (the save
// failed or nothing was entered).
func (m *Model) applySavedKey() {
	m.applySavedKeyFor(config.ModelProvider(m.modelID))
}

// applySavedKeyFor loads the named provider's stored key into the live
// gateway and clears the missing-key state.
func (m *Model) applySavedKeyFor(provider string) {
	key := config.LoadUserAPIKeyFor(provider)
	if cp := config.LoadCustomProvider(); provider == cpName(cp) && cp != nil && key == "" {
		key = cp.APIKey
	}
	if key == "" {
		return
	}
	if g, ok := m.gateway.(*gateway.Gateway); ok {
		g.APIKey = key
	}
	m.keyMissing = false
}

func cpName(cp *config.CustomProvider) string {
	if cp == nil {
		return ""
	}
	return cp.Name
}

// providerFlow runs one picker selection: with a resolvable key it goes
// straight to that route's model list; otherwise the Diksha modal asks
// for the key first, then the models follow (the connect-close handler
// chains via pendingProvider).
func (m *Model) providerFlow(pick string) {
	if _, err := config.GetAPIKeyFor(keyProviderFor(pick)); err == nil {
		m.openProviderModels(pick)
		return
	}
	m.pendingProvider = pick
	m.openConnectModal()
}

// startCustomProvider probes a BYOK endpoint's live model list on a worker
// goroutine; the result lands as customModelsMsg and opens the picker.
func (m *Model) startCustomProvider(baseURL, apiKey string) {
	name := deriveProviderName(baseURL)
	m.appendNotice("probing " + strings.TrimRight(baseURL, "/") + "/models …")
	send := m.sendFn
	go func() {
		ids, err := fetchProviderModels(baseURL, apiKey)
		send(customModelsMsg{name: name, baseURL: baseURL, apiKey: apiKey, ids: ids, err: err})
	}()
}

// onCustomModels stores a working BYOK endpoint and offers its fetched
// model list; the pick finalizes the provider's model.
func (m *Model) onCustomModels(msg customModelsMsg) {
	if msg.err != nil {
		m.appendError("add provider: " + msg.err.Error())
		return
	}
	if len(msg.ids) == 0 {
		m.appendError("add provider: " + msg.baseURL + " listed no models")
		return
	}
	cp := config.CustomProvider{Name: msg.name, BaseURL: msg.baseURL, APIKey: msg.apiKey}
	if err := config.SaveCustomProvider(cp); err != nil {
		m.appendError("add provider: " + err.Error())
		return
	}
	m.pickingForCustom = true
	m.openListModal(modalModels, "Astras — "+cp.Name+" (pick one)", msg.ids)
}

// deriveProviderName names a BYOK provider after its host: api.together.xyz
// → "together"; hosts without a usable label fall back to "custom".
func deriveProviderName(baseURL string) string {
	u, err := url.Parse(baseURL)
	host := ""
	if err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "" || host == "localhost" || net.ParseIP(host) != nil {
		return "custom"
	}
	parts := strings.Split(host, ".")
	for len(parts) > 0 && (parts[0] == "api" || parts[0] == "www") {
		parts = parts[1:]
	}
	if len(parts) > 1 {
		parts = parts[:len(parts)-1] // drop the TLD
	}
	name := strings.Join(parts, "-")
	if name == "" {
		return "custom"
	}
	return name
}

// fetchProviderModels GETs an OpenAI-compatible endpoint's model list
// ({"data":[{"id":…}]} shape, with tolerant fallbacks). Never logs the key.
func fetchProviderModels(baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", doctorSafeUA)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("could not parse the model list from %s", url)
	}
	raw := doc.Data
	if len(raw) == 0 {
		raw = doc.Models
	}
	ids := make([]string, 0, len(raw))
	for _, entry := range raw {
		if entry.ID != "" {
			ids = append(ids, entry.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// doctorSafeUA is the proven User-Agent: default Go UAs are WAF-blocked by
// some gateways (see cli.go gatewayReachable).
const doctorSafeUA = "kaal/0.3 (+https://github.com/kaal/kaal)"

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
		follow:          true,
		agentsState:     agentState,
		sendFn:          func(tea.Msg) {}, // no-op until Main wires the program
		rightStyle:      lipgloss.NewStyle().Foreground(colorDim),
		dimStyle:        lipgloss.NewStyle().Foreground(colorDim),
		errorStyle:      lipgloss.NewStyle().Foreground(colorEmber),
		userLabelStyle:  lipgloss.NewStyle().Foreground(colorSaffron).Bold(true),
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
	if m.keyMissing {
		// No key: keep the typed text in the input and point at /connect.
		m.appendError("no API key — save one with /connect <key> before sending")
		return nil
	}
	m.appendUserBlock(task)
	m.promptHistory = append(m.promptHistory, task)
	m.historyIndex = -1
	m.input.Reset()
	m.steps = 0
	m.turnActive = true
	m.thinking = true
	m.connecting = true // until the stream's first event lands
	m.thinkFrame = 0
	m.mdPending = ""
	m.flushArmed = false
	m.streamChars = 0
	m.lastRateChars = 0
	m.rate = 0
	m.turnStart = time.Now()
	m.lastTickTime = m.turnStart
	// A new prompt re-pins the live edge, and the indicator must appear
	// the moment Enter lands — not when the server's first byte does.
	m.follow = true
	m.renderConversation()

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
	case tea.MouseMsg:
		switch msg.Type {
		case tea.MouseWheelUp:
			m.follow = false // reading history: release the pin
			m.viewport.ScrollUp(3)
		case tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
			if m.viewport.AtBottom() {
				m.follow = true // back at the live edge: re-pin
			}
		}
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
			// Animate even while nothing streams: the chakra must turn
			// during the silent connect/think window too.
			m.renderConversation()
			cmds = append(cmds, tea.Tick(thinkingTick, func(t time.Time) tea.Msg {
				return thinkTickMsg{frame: m.thinkFrame}
			}))
		}
	case openAskMsg:
		m.openAskModal(msg)
	case customModelsMsg:
		m.onCustomModels(msg)
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
		m.follow = true
		m.viewport.GotoBottom()
	case "pgup":
		m.follow = false
		m.viewport.HalfPageUp()
		return nil
	case "pgdown":
		m.viewport.HalfPageDown()
		if m.viewport.AtBottom() {
			m.follow = true
		}
		return nil
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
	case "up", "down":
		if m.suggestionsVisible && len(m.suggestions) > 0 {
			n := len(m.suggestions)
			if msg.String() == "up" {
				m.suggestIndex--
			} else {
				m.suggestIndex++
			}
			m.suggestIndex = ((m.suggestIndex % n) + n) % n
			return nil
		}
	case "esc":
		if m.suggestionsVisible {
			m.suggestDismissed = true
			m.updateSuggestions()
			return nil
		}
	case "tab":
		if m.suggestionsVisible && len(m.suggestions) > 0 {
			c := m.suggestions[m.suggestIndex]
			completed := c.name
			if c.args != "" {
				completed += " "
			}
			m.input.SetValue(completed)
			m.input.CursorEnd()
			m.updateSuggestions()
			return nil
		}
		m.input.InsertString("\t")
		return nil
	case "enter":
		if !isShift(msg) {
			value := strings.TrimSpace(m.input.Value())
			if strings.HasPrefix(value, "/") {
				head := value
				if i := strings.IndexAny(value, " \t"); i >= 0 {
					head = value[:i]
				}
				if slashFind(head) == nil && m.suggestionsVisible && len(m.suggestions) > 0 {
					// A partial command completes from the palette instead of
					// dying as "unknown" — the palette's cursor picks which.
					c := m.suggestions[m.suggestIndex]
					completed := c.name
					if c.args != "" {
						completed += " "
					}
					m.input.SetValue(completed)
					m.input.CursorEnd()
					m.updateSuggestions()
					return nil
				}
				m.input.Reset()
				m.updateSuggestions()
				m.runCommand(value)
				return nil
			}
			return m.Submit(value)
		}
		m.input.InsertString("\n")
	}
	// Everything else goes to the composer textarea.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.updateSuggestions()
	return cmd
}

// updateSuggestions drives the command palette: visible while the composer
// starts with "/", matching names first and descriptions second; esc
// dismisses until the input changes again.
func (m *Model) updateSuggestions() {
	value := m.input.Value()
	if value != m.lastSuggestInput {
		m.suggestDismissed = false
		m.lastSuggestInput = value
	}
	if !strings.HasPrefix(value, "/") {
		m.suggestionsVisible = false
		m.suggestions = nil
		m.suggestIndex = 0
		return
	}
	m.suggestions = slashMatches(value)
	m.suggestionsVisible = len(m.suggestions) > 0 && !m.suggestDismissed
	switch {
	case m.suggestIndex >= len(m.suggestions):
		m.suggestIndex = 0
	case m.suggestIndex < 0:
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
		m.appendNotice(m.renderHelpPanel())
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
		m.openListModal(modalSessions, "Itihasa — sessions", ids)
	case "/models":
		ids := make([]string, 0, len(config.Models))
		for _, model := range config.Models {
			ids = append(ids, model.ID)
		}
		m.openListModal(modalModels, "Astras — models", ids)
	case "/agents":
		state := agents.Load(m.projectDir)
		m.agentsState = state
		names := agents.AgentNames(state)
		m.openListModal(modalAgents, "Sabha — the assembly", names)
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
			if err := config.SaveUserAPIKeyFor(config.ModelProvider(m.modelID), arg); err != nil {
				m.appendError("could not save API key: " + err.Error())
			} else {
				m.applySavedKey() // inline key, no popup
				m.appendNotice("api key saved")
			}
		} else {
			m.openProviderModal()
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
		msg := "unknown command: " + cmd
		if near := slashNearest(cmd); near != "" && near != cmd {
			msg += " — did you mean " + near + "?"
		} else {
			msg += " (try /help)"
		}
		m.appendNotice(msg)
	}
}

// -- turn events -------------------------------------------------------------------

func (m *Model) onLoopEvent(ev loop.AgentEvent) tea.Cmd {
	if m.connecting && ev.Kind != loop.EventDone {
		// First sign of life from the gateway: the connection is open.
		m.connecting = false
	}
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
	m.connecting = false
	m.flushMD()
	m.closeUnclosedFence()
	if err == nil {
		// The receipt: wall time, steps, and effective tok/s for THIS turn.
		took := time.Since(m.turnStart)
		tps := 0.0
		if secs := took.Seconds(); secs > 0 {
			tps = float64(m.streamChars) / 3 / secs
		}
		m.appendMeta(fmt.Sprintf("⏱ %s · step %d · ~%.0f tok/s",
			humanDuration(took), m.steps, tps))
	}
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
	if m.follow {
		m.viewport.GotoBottom()
	}
	m.renderMermaidDiagrams()
}

// -- rendering helpers ----------------------------------------------------------------

func (m *Model) resize() {
	// Everything lives inside the app frame: terminal minus 4-cell padding.
	m.innerW = m.width - 2*appPadding
	m.innerH = m.height - 2*appPadding
	if m.innerW < 20 {
		m.innerW = 20
	}
	if m.innerH < 10 {
		m.innerH = 10
	}
	sidebar := 0
	if m.sidebarVisible {
		sidebar = sidebarWidth
	}
	convW := m.innerW - sidebar
	if convW < 20 {
		convW = 20
	}
	statusH := 1
	composerH := 4
	convH := m.innerH - statusH - composerH
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
	width := m.innerW - 2
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

// -- Kurukshetra regalia -----------------------------------------------------------

// activeAgentName resolves the persona speaking right now: the active
// Pandava, or plain "kaal" when none is set.
func (m *Model) activeAgentName() string {
	if m.agent != nil && m.agent.Name != "" {
		return m.agent.Name
	}
	return "kaal"
}

// agentBadge renders the compact banner badge (status bar): dark sigil +
// name on the Pandava's color.
func (m *Model) agentBadge(name string) string {
	id := identityFor(name)
	return lipgloss.NewStyle().
		Background(id.color).
		Foreground(lipgloss.Color("16")).
		Bold(true).
		Render(" " + id.glyph + " " + name + " ")
}

// agentChip is the badge with the epic byname beside it (home cast row,
// agents modal).
func (m *Model) agentChip(name string) string {
	id := identityFor(name)
	chip := m.agentBadge(name)
	if id.epithet != "" {
		chip += m.dimStyle.Render(" " + id.epithet)
	}
	return chip
}

// streamHost names where the current turn is dialing — the gateway host
// when available, the model id otherwise.
func (m *Model) streamHost() string {
	if g, ok := m.gateway.(*gateway.Gateway); ok {
		if u, err := url.Parse(g.BaseURL); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return m.modelID
}

// assistantStyle dresses the answering persona's label in its own color.
func assistantStyle(name string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(identityFor(name).color).Bold(true)
}

// renderHome renders the branded empty state: the wordmark over Arjuna's
// chariot, the KURUKSHETRA title and tagline, the Gita verse kaal works by,
// the first actions, and the Pandava cast in their own colors. Shown on a
// fresh session (and after /new) — mirrored to the transcript once.
func (m *Model) renderHome() {
	var sb strings.Builder
	sb.WriteString(lipgloss.NewStyle().Foreground(colorGold).Render(kaalArt))
	sb.WriteString("\n")
	sb.WriteString(m.dimStyle.Render(chariotArt))
	sb.WriteString("\n\n")
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorSaffron).Render(homeTitle))
	sb.WriteString("\n")
	sb.WriteString(m.dimStyle.Render(homeTagline))
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(colorIvory).Render(gitaVerse))
	sb.WriteString("\n")
	sb.WriteString(m.dimStyle.Render(gitaGloss))
	sb.WriteString("\n\n")
	// The first actions sit right after the tagline so the welcome + session
	// are in view even on short terminals (the cast below is decorative).
	sb.WriteString(m.dimStyle.Render(homeWelcome))
	sb.WriteString("\n")
	if m.keyMissing {
		sb.WriteString(m.errorStyle.Render("no API key — run /connect <key> to start chatting"))
		sb.WriteString("\n")
	}
	sb.WriteString(m.dimStyle.Render("kaal 0.3 · " + m.modelID + " · " + m.sessionID))
	sb.WriteString("\n")
	if m.freeKeyless {
		sb.WriteString(m.dimStyle.Render("zen free tier · no login needed"))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	// The cast: each Pandava in banner color, sigil and byname.
	for _, name := range agents.AgentNames(m.agentsState) {
		sb.WriteString(m.agentChip(name))
		sb.WriteString("  ")
	}
	m.viewport.SetContent(sb.String())
	m.viewport.GotoTop()
	if !m.homeMirrored {
		m.homeMirrored = true
		m.transcript = append(m.transcript, kaalArt, homeTitle, homeTagline)
	}
}

// renderConversation rebuilds the viewport content from the blocks.
func (m *Model) renderConversation() {
	if len(m.blocks) == 0 && !m.turnActive {
		m.renderHome()
		return
	}
	m.homeMirrored = false
	var sb strings.Builder
	// The answering persona speaks under its own banner: the active
	// Pandava's name in the Pandava's color.
	voice := m.activeAgentName()
	answerLabel := assistantStyle(voice).Render("▌ " + voice)
	for _, b := range m.blocks {
		switch b.kind {
		case blockUser:
			sb.WriteString(m.userLabelStyle.Render("▌ you"))
			sb.WriteString("\n")
			sb.WriteString(m.renderMarkdown(b.text))
		case blockAssistant:
			sb.WriteString(answerLabel)
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
		case blockMeta:
			sb.WriteString(lipgloss.NewStyle().Foreground(colorDim).
				Faint(true).Render(b.text))
			sb.WriteString("\n")
		}
	}
	if m.thinking && m.turnActive {
		st := lipgloss.NewStyle().Foreground(colorSaffron)
		if m.connecting {
			sb.WriteString(st.Render("◈ opening " + m.streamHost() + " …"))
		} else {
			sb.WriteString(st.Render(chakraFrames[m.thinkFrame%len(chakraFrames)] + " thinking"))
		}
		sb.WriteString("\n")
	}
	m.viewport.SetContent(sb.String())
	if m.follow {
		// Pinned to the live edge; a scroll-up releases this (see the
		// wheel/pgup handlers) so reading history never gets yanked.
		m.viewport.GotoBottom()
	}
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

// appendMeta appends a faint receipt line (timings, counters) under the
// answer — content for the eyes, never fed back to the model.
func (m *Model) appendMeta(text string) {
	m.transcript = append(m.transcript, text+"\n")
	m.blocks = append(m.blocks, block{kind: blockMeta, text: text})
	m.renderConversation()
}

// humanDuration renders a turn duration: milliseconds under a second,
// centisecond-precise seconds above.
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
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
	agentName := m.activeAgentName()
	clock := time.Now().Format("Mon 02 Jan 15:04")
	rate := m.tools.CacheHitRate()
	cache := "cache –"
	if rate >= 0 {
		cache = fmt.Sprintf("cache %.0f%%", rate*100)
	}
	sep := m.dimStyle.Render("│")
	var sb strings.Builder
	sb.WriteString(m.agentBadge(agentName))
	sb.WriteString(sep)
	sb.WriteString(fmt.Sprintf(" step %d/%d ", m.steps, m.maxSteps))
	sb.WriteString(sep)
	sb.WriteString(fmt.Sprintf(" %.0f tok/s ", m.rate))
	sb.WriteString(sep)
	sb.WriteString(" " + cache + " ")
	sb.WriteString(sep)
	sb.WriteString(fmt.Sprintf(" $%.4f ", m.totalCost))
	sb.WriteString(sep)
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
	title := "Diksha — connect"
	if m.pendingProvider != "" {
		title = "Diksha — " + providerDisplayName(m.pendingProvider) + " API key"
	}
	m.modal = &modal{kind: modalConnect, title: title, input: &in}
}

// openProviderModal opens the /connect chooser: the two built-in providers
// plus the BYOK escape hatch.
func (m *Model) openProviderModal() {
	m.pendingProvider = ""
	items := []string{pickOpencode, pickOpencodeGo, pickCommand, pickAddAnother}
	m.modal = &modal{kind: modalProviders, title: "Diksha — choose a provider", items: items, allItems: items}
}

// openProviderModels lists one picker choice's catalog slice. The opencode
// tiers split the shared provider catalog by route: free lists the keyless
// zen/v1 models, go lists the paid zen/go/v1 ones.
func (m *Model) openProviderModels(pick string) {
	var ids []string
	for _, model := range config.Models {
		p := config.ModelProvider(model.ID)
		ok := false
		switch pick {
		case pickOpencode:
			ok = p == config.ProviderOpencode && config.FreeTierModel(model.ID)
		case pickOpencodeGo:
			ok = p == config.ProviderOpencode && !config.FreeTierModel(model.ID)
		default:
			ok = p == pick
		}
		if ok {
			ids = append(ids, model.ID)
		}
	}
	m.openListModal(modalModels, "Astras — "+providerDisplayName(pick), ids)
}

func (m *Model) openAgentIntentModal() {
	in := textarea.New()
	in.Placeholder = "describe the agent you want…"
	in.ShowLineNumbers = false
	in.SetWidth(60)
	in.Focus()
	m.modal = &modal{kind: modalAgentIntent, title: "New agent (AI)", input: &in}
}

// openCustomProviderForm opens the BYOK form: any OpenAI-compatible
// endpoint plus its key. Enter probes <base>/models live.
func (m *Model) openCustomProviderForm() {
	url := textarea.New()
	url.Placeholder = "https://api.example.com/v1"
	url.ShowLineNumbers = false
	url.SetWidth(56)
	url.Focus()
	key := textarea.New()
	key.Placeholder = "paste API key"
	key.ShowLineNumbers = false
	key.SetWidth(56)
	m.modal = &modal{kind: modalCustomProvider, title: "Add another provider", input: &url, input2: &key, formFocus: 0}
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
	case modalProviders:
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
		}
	case modalCustomProvider:
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
				base := strings.TrimSpace(mod.input.Value())
				key := strings.TrimSpace(mod.input2.Value())
				if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
					m.appendNotice("add provider: base URL must start with http:// or https://")
					return nil
				}
				m.closeModal(customProviderForm{baseURL: base, apiKey: key})
			} else if mod.formFocus == 0 {
				mod.input.InsertString("\n")
			} else {
				mod.input2.InsertString("\n")
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
			if m.pickingForCustom {
				// Finalize the BYOK choice: store the model so the custom
				// provider owns this id for every future lookup.
				m.pickingForCustom = false
				if cp := config.LoadCustomProvider(); cp != nil {
					cp.Model = id
					_ = config.SaveCustomProvider(*cp)
					m.appendNotice("provider: " + cp.Name + " · model: " + id)
				}
			}
			m.setModel(id)
			m.applySavedKeyFor(config.ModelProvider(m.modelID))
		}
	case modalProviders:
		if pick, ok := value.(string); ok {
			switch pick {
			case pickOpencode, pickOpencodeGo, pickCommand:
				m.providerFlow(pick)
			case pickAddAnother:
				m.openCustomProviderForm()
			}
		}
	case modalCustomProvider:
		if form, ok := value.(customProviderForm); ok {
			m.startCustomProvider(form.baseURL, form.apiKey)
		}
	case modalConnect:
		flow := m.pendingProvider // picker flow: after the key lands, straight to its models
		target := config.ModelProvider(m.modelID)
		if flow != "" {
			target = keyProviderFor(flow)
		}
		m.pendingProvider = ""
		if key, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			key = strings.TrimSpace(key)
			if err := config.SaveUserAPIKeyFor(target, key); err != nil {
				m.appendError("could not save API key: " + err.Error())
			} else {
				m.applySavedKeyFor(target)
				m.appendNotice("api key saved (" + providerDisplayName(flow) + ")")
			}
		}
		if flow != "" {
			m.openProviderModels(flow)
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
	// Retarget the live gateway so the switch takes effect this session,
	// not just on the next launch.
	if g, ok := m.gateway.(*gateway.Gateway); ok {
		g.BaseURL = config.ModelBaseURL(modelID)
		g.Model = modelID
	}
	_, keyErr := config.GetAPIKeyFor(config.ModelProvider(modelID))
	m.freeKeyless = keyErr != nil && config.FreeTierModel(modelID)
	m.keyMissing = keyErr != nil && !config.FreeTierModel(modelID)
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
		brand := lipgloss.NewStyle().Bold(true).Foreground(colorSaffron).Render("KAAL")
		identity := m.dimStyle.Render("  " + model + " · " + session + "  ")
		actions := m.dimStyle.Render("[New chat] [Sessions] [Agents]")
		topbar = lipgloss.NewStyle().Width(m.innerW).BorderBottom(true).Render(
			brand+identity+actions,
		) + "\n"
	}
	var content string
	if m.modal != nil {
		content = topbar + m.modalView()
	} else {
		sidebar := ""
		if m.sidebarVisible {
			header := lipgloss.NewStyle().Bold(true).Render("Workspace")
			tabs := ""
			for i, name := range []string{"Trace", "Memory", "Sessions"} {
				if i == m.sidebarTab {
					tabs += lipgloss.NewStyle().Bold(true).Foreground(colorSaffron).Render(" " + name + " ")
				} else {
					tabs += m.dimStyle.Render(" " + name + " ")
				}
			}
			sidebar = lipgloss.NewStyle().Width(sidebarWidth).BorderLeft(true).Render(
				header + "\n" + tabs + "\n" + m.sidebarView(),
			)
		}

		conversation := m.viewport.View()
		if m.suggestionsVisible && len(m.suggestions) > 0 {
			// The palette floats over the conversation's lower rows — it must
			// never shove the layout around.
			w := m.viewport.Width
			if w > 64 {
				w = 64
			}
			conversation = overlayBottom(conversation, m.renderSuggestionPopup(w))
		}

		state := "ready"
		if m.turnActive {
			state = "busy — ctrl+c cancels"
		}
		composer := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("Message kaal · ") +
			lipgloss.NewStyle().Foreground(colorGold).Render(state) + "\n" +
			m.input.View()

		status := m.statusBar()

		content += topbar +
			lipgloss.JoinHorizontal(lipgloss.Top, conversation, sidebar) + "\n" +
			composer + "\n" +
			status
	}

	// The whole window floats inside a 4-cell frame.
	frame := lipgloss.NewStyle().
		Width(m.width).
		Height(m.height).
		MaxHeight(m.height).
		Padding(appPadding)
	return frame.Render(content)
}

// modalTopMargin is the breathing room above every dialog.
const modalTopMargin = 8

// modalPage splits a dialog into a fixed header, the scrollable rows, and a
// hint footer — the only part of a modal that ever overflows is rows.
type modalPage struct {
	header string
	rows   []string
	footer string
}

// buildModalPage renders each dialog's content as page sections. Rows map
// one-to-one onto mod.items for list dialogs so cursor scrolling stays
// trivially aligned.
func (m *Model) buildModalPage(mod *modal) modalPage {
	switch mod.kind {
	case modalSessions, modalAgents:
		var p modalPage
		p.footer = "enter select · esc close"
		if mod.kind == modalAgents {
			p.footer = "enter activate · n new · d delete · esc close"
		}
		if len(mod.items) == 0 {
			p.rows = []string{m.dimStyle.Render("(empty)")}
			return p
		}
		for i, item := range mod.items {
			line := item
			if mod.kind == modalAgents {
				// The Sabha lists each persona under its own sigil.
				id := identityFor(item)
				line = id.glyph + " " + item
				if id.epithet != "" {
					line += m.dimStyle.Render(" — " + id.epithet)
				}
			}
			if i == mod.cursor {
				line = lipgloss.NewStyle().Bold(true).Foreground(colorSaffron).Render("▸ " + line)
			} else {
				line = "  " + line
			}
			p.rows = append(p.rows, line)
		}
		return p
	case modalModels:
		var p modalPage
		if mod.input != nil {
			p.header = m.dimStyle.Render("filter: ") + mod.input.View()
		}
		p.footer = "enter select · esc close"
		for i, id := range mod.items {
			line := id
			if price := modelPriceLine(id); price != "" {
				line += m.dimStyle.Render("  " + price)
			}
			if config.ModelProvider(id) == config.ProviderCommandCode {
				line += m.dimStyle.Render("  · cmd")
			}
			if i == mod.cursor {
				line = lipgloss.NewStyle().Bold(true).Foreground(colorSaffron).Render("▸ " + line)
			} else {
				line = "  " + line
			}
			p.rows = append(p.rows, line)
		}
		return p
	case modalProviders:
		var p modalPage
		p.footer = "enter choose · esc cancel"
		if len(mod.items) == 0 {
			p.rows = []string{m.dimStyle.Render("(empty)")}
			return p
		}
		for i, item := range mod.items {
			line := m.providerItemLabel(item)
			if i == mod.cursor {
				line = lipgloss.NewStyle().Bold(true).Foreground(colorSaffron).Render("▸ " + line)
			} else {
				line = "  " + line
			}
			p.rows = append(p.rows, line)
		}
		return p
	case modalCustomProvider:
		return modalPage{rows: []string{
			m.dimStyle.Render("base URL (OpenAI-compatible):"),
			mod.input.View(),
			m.dimStyle.Render("API key:"),
			mod.input2.View(),
			m.dimStyle.Render("tab switch · enter fetch models · esc cancel"),
		}}
	case modalConnect:
		provider := keyProviderFor(m.pendingProvider)
		envName := "OPENCODE_API_KEY"
		if provider == config.ProviderCommandCode {
			envName = "CMD_API_KEY"
		}
		return modalPage{rows: []string{
			m.dimStyle.Render(fmt.Sprintf("paste the %s API key (never displayed):", providerDisplayName(m.pendingProvider))),
			mod.input.View(),
			m.dimStyle.Render("enter save · esc cancel · env: " + envName),
		}}
	case modalAgentForm:
		return modalPage{rows: []string{
			m.dimStyle.Render("name:"),
			mod.input.View(),
			m.dimStyle.Render("description:"),
			mod.input2.View(),
			m.dimStyle.Render("tab switch · enter save · esc cancel"),
		}}
	case modalAgentIntent:
		return modalPage{rows: []string{
			m.dimStyle.Render("describe the agent you want — the model invents it:"),
			mod.input.View(),
			m.dimStyle.Render("enter generate · esc cancel"),
		}}
	case modalAsk:
		rows := []string{}
		for i, opt := range mod.askOptions {
			rows = append(rows, m.dimStyle.Render(fmt.Sprintf("  %d. %s", i+1, opt)))
		}
		rows = append(rows, mod.input.View(), m.dimStyle.Render("enter answer · esc cancel"))
		return modalPage{rows: rows}
	}
	return modalPage{}
}

// modalView renders the active modal: a centered card with an 8-row top
// margin whose scrollable section never exceeds the screen.
func (m *Model) modalView() string {
	mod := m.modal
	if mod == nil {
		return ""
	}
	page := m.buildModalPage(mod)

	headerLines := 0
	if page.header != "" {
		headerLines = strings.Count(page.header, "\n") + 1
	}
	footerLines := 0
	if page.footer != "" {
		footerLines++
	}
	// The top margin shrinks before the scrollable section does: on short
	// screens the card keeps its rows and surrenders margin first.
	margin := modalTopMargin
	listMax := func() int {
		usable := m.innerH - 1 - margin // trailing newline + top margin
		if m.topbarVisible {
			usable -= 2
		}
		return usable - 4 - headerLines - footerLines // borders(2)+title(1)+blank(1)
	}
	for margin > 0 && listMax() < 2 {
		margin--
	}
	rowsMax := listMax()
	if rowsMax < 1 {
		rowsMax = 1
	}

	body := ""
	if page.header != "" {
		body += page.header + "\n"
	}
	if len(page.rows) > rowsMax {
		start, end := suggestionWindow(len(page.rows), mod.cursor, rowsMax)
		shown := page.rows[start:end]
		scrollNote := m.dimStyle.Render(fmt.Sprintf("  ↑↓ %d–%d of %d",
			start+1, end, len(page.rows)))
		if page.footer != "" {
			page.footer += scrollNote
		} else {
			shown = append(shown[:len(shown):len(shown)], scrollNote)
		}
		body += strings.Join(shown, "\n")
	} else {
		body += strings.Join(page.rows, "\n")
	}
	if page.footer != "" {
		body += "\n" + page.footer
	}

	title := lipgloss.NewStyle().Bold(true).Render(mod.title)
	// Dialogs read best as centered cards: capped width, saffron frame,
	// floated to the middle of the screen instead of stretching edge-to-edge.
	w := m.innerW - 8
	if w < 24 {
		w = 24
	}
	if w > 76 {
		w = 76
	}
	box := lipgloss.NewStyle().Width(w).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorSaffron).
		Padding(0, 1).
		Render(title + "\n\n" + strings.TrimRight(body, "\n"))
	centered := lipgloss.NewStyle().Width(m.innerW).Align(lipgloss.Center).Render(box)
	return strings.Repeat("\n", margin) + centered + "\n"
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

// providerItemLabel dresses one picker row: sigil, display name, and the
// key state so the user knows what choosing it will ask of them.
func (m *Model) providerItemLabel(item string) string {
	switch item {
	case pickOpencode:
		label := "☸ opencode · zen free"
		if _, err := config.GetAPIKeyFor(config.ProviderOpencode); err == nil {
			return label + m.dimStyle.Render("  — no login needed")
		}
		return label + m.dimStyle.Render("  — keyless")
	case pickOpencodeGo:
		label := "◈ opencode · go plan"
		if _, err := config.GetAPIKeyFor(config.ProviderOpencode); err == nil {
			return label + m.dimStyle.Render("  — key ready")
		}
		return label + m.dimStyle.Render("  — needs OPENCODE_API_KEY")
	case pickCommand:
		label := "⌘ command-code"
		if _, err := config.GetAPIKeyFor(config.ProviderCommandCode); err == nil {
			return label + m.dimStyle.Render("  — key ready")
		}
		return label + m.dimStyle.Render("  — needs CMD_API_KEY")
	default:
		return "✶ add another provider…" + m.dimStyle.Render("  — any OpenAI-compatible endpoint")
	}
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
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
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
