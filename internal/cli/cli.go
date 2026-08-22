// Package cli ports harness/cli.py: the kaal command surface.
//
// `kaal` with no subcommand launches the TUI (Go TUI lands in P5 — for now
// a placeholder message). `kaal run` is a one-shot, non-interactive agent
// run; `kaal sessions list|show|delete|prune` manage the session store;
// `kaal doctor` self-checks; `kaal update` self-updates; `kaal diagrams`
// renders mermaid via termaid.
//
// Exit codes mirror the Python CLI: 0 = answer produced, 1 = config/key/
// gateway error, 2 = argument or loop error.
package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/kaal/kaal/internal/agents"
	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/gateway"
	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/loop"
	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/prompts"
	"github.com/kaal/kaal/internal/sessions"
	"github.com/kaal/kaal/internal/toolcache"
	"github.com/kaal/kaal/internal/tools"
	"github.com/kaal/kaal/internal/tui"
)

// version is stamped at release build time via
// -ldflags "-X github.com/kaal/kaal/internal/cli.version=<tag>"; the
// default keeps local/dev builds (and the version probe) honest.
var version = "0.3"

// The same UA the gateway sends (proven to pass Cloudflare WAF error 1010).
const doctorUA = "python-requests/2.31.0"

// kaalRepoURL mirrors install.sh's KAAL_REPO_URL default.
const kaalRepoURL = "https://github.com/shivamnarkar47/kaal"

// kaalUpdateURL is the git-less tarball endpoint (a seam for tests).
var kaalUpdateURL = kaalRepoURL + "/archive/refs/heads/main.tar.gz"

// kaalReleaseAPI is the GitHub latest-release endpoint (a seam for tests).
var kaalReleaseAPI = "https://api.github.com/repos/shivamnarkar47/kaal/releases/latest"

// updateExecutable is the binary release-fetch replaces — the running
// executable by default, overridable in tests.
var updateExecutable = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// Main is the CLI entry point; returns the process exit code. stdin/stdout/
// stderr are injected so tests can capture everything.
func Main(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	root.SetIn(stdin)
	root.SetArgs(argv)
	if err := root.Execute(); err != nil {
		// exitError carries our own exit-code contract (1 = config/key/
		// gateway, 2 = loop); anything else is a flag/arg error — the
		// argparse exit-2 class.
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		return 2
	}
	return 0
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "kaal",
		Short:         "kaal — DeepSeek V4 Flash agent harness",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(newRunCmd(stdout, stderr))
	root.AddCommand(newSessionsCmd(stdout, stderr))
	root.AddCommand(newDoctorCmd(stdout, stderr))
	root.AddCommand(newUpdateCmd(stdout, stderr))
	root.AddCommand(newDiagramsCmd(stdout, stderr))
	// No subcommand: launch the bubbletea workbench (Python launches the
	// Textual TUI). The TUI needs a real terminal; without one, point the
	// user at the one-shot road.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if f, ok := stdout.(*os.File); ok && isTerminal(f) {
			return &exitError{code: tui.Main()}
		}
		fmt.Fprintln(stderr, "kaal: the TUI needs a terminal — use `kaal run \"PROMPT\"` for one-shot runs")
		return &exitError{code: 1}
	}
	return root
}

// exitError lets RunE return a specific exit code without cobra printing it
// as an error.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("exit %d", e.code)
}

// -- run ---------------------------------------------------------------------

type runOptions struct {
	batch          string
	workers        int
	dir            string
	model          string
	maxSteps       int
	memoryRoot     string
	allowDangerous bool
	resume         string
	verbose        bool
	jsonOut        bool
	noToolCache    bool
	noVerify       bool
	agent          string

	// P6: one async session writer per process, shared across batch
	// workers; batch workers also get their own gateway transport.
	sessionWriter   *sessions.AsyncWriter
	workerTransport bool
}

func newRunCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := &runOptions{}
	cmd := &cobra.Command{
		Use:   "run [prompt]",
		Short: "run a one-shot prompt",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := ""
			if len(args) == 1 {
				prompt = args[0]
			}
			return runCmd(cmd, opts, prompt, stdout, stderr)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.batch, "batch", "", "run prompts from FILE (one per line, or a JSON array), one session each")
	f.IntVar(&opts.workers, "workers", defaultWorkers(), "max concurrent --batch tasks (default: min(4, cpu count))")
	f.StringVar(&opts.dir, "dir", "", "project directory (default: cwd)")
	f.StringVar(&opts.model, "model", "", "model id (default: "+config.ModelID+")")
	f.IntVar(&opts.maxSteps, "max-steps", 20, "max agent turns (default: 20)")
	f.StringVar(&opts.memoryRoot, "memory-root", "", "memory root (default: <dir>/.agent-memory)")
	f.BoolVar(&opts.allowDangerous, "allow-dangerous", false, "permit destructive commands")
	f.StringVar(&opts.resume, "resume", "", "continue a session")
	f.BoolVar(&opts.verbose, "verbose", false, "print reasoning to stderr")
	f.BoolVar(&opts.jsonOut, "json", false, "final JSON line with session/answer")
	f.BoolVar(&opts.noToolCache, "no-tool-cache", false, "disable the read-only tool-result cache (.kaal/tool-cache.json)")
	f.BoolVar(&opts.noVerify, "no-verify", false, "disable verify hooks after mutation (.kaal/hooks.json)")
	f.StringVar(&opts.agent, "agent", "", "persona to operate as (name from .kaal/agents.json; the five Pandava defaults always exist)")
	return cmd
}

func defaultWorkers() int {
	n := runtime.NumCPU()
	if n > 4 {
		return 4
	}
	if n < 1 {
		return 1
	}
	return n
}

func runCmd(cmd *cobra.Command, opts *runOptions, prompt string, stdout, stderr io.Writer) error {
	opts.sessionWriter = sessions.NewAsyncWriter()
	defer opts.sessionWriter.Close()
	if prompt != "" && opts.batch != "" {
		return argError(cmd, stderr, "argument --batch: not allowed with argument prompt")
	}
	if opts.batch != "" {
		return runBatch(opts, stdout, stderr)
	}
	if prompt == "" {
		if opts.resume != "" {
			// `kaal run --resume <id>` alone is a valid continuation.
			prompt = "continue"
		} else {
			return argError(cmd, stderr, "the following arguments are required: prompt")
		}
	}
	if prompt == "-" {
		// Reading from a TTY blocks; that is the user's explicit choice.
		raw, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(raw))
	}
	sessionID := opts.resume
	if sessionID == "" {
		sessionID = sessions.NewSessionID()
	}
	record := runOne(prompt, opts, sessionID, stdout, stderr, nil)
	opts.sessionWriter.Flush() // durable before the answer prints
	if record.ErrorKind != "" {
		if record.ErrorKind == "config" {
			// GetAPIKey already printed its message; nothing to add.
			return &exitError{code: 1}
		}
		fmt.Fprintf(stderr, "kaal: %s\n", record.Error)
		if opts.jsonOut {
			line, _ := jsonpy.Marshal(publicRecord(record))
			fmt.Fprintln(stdout, string(line))
		}
		if record.ErrorKind == "loop" {
			return &exitError{code: 2}
		}
		return &exitError{code: 1}
	}
	if opts.jsonOut {
		line, _ := jsonpy.Marshal(publicRecord(record))
		fmt.Fprintln(stdout, string(line))
	}
	return nil
}

// argError prints an argument error with the command's usage (the argparse
// contract: usage + message, exit 2) and returns the exit code.
func argError(cmd *cobra.Command, stderr io.Writer, msg string) error {
	fmt.Fprintln(stderr, "Error:", msg)
	fmt.Fprintln(stderr, cmd.UsageString())
	return &exitError{code: 2}
}

// runRecord is one run's outcome: success {session_id, answer, model, steps,
// tool_calls, usage, cost} or error {session_id, error} (+ internal kind).
type runRecord struct {
	SessionID string     `json:"session_id"`
	Answer    string     `json:"answer,omitempty"`
	Model     string     `json:"model,omitempty"`
	Steps     int        `json:"steps,omitempty"`
	ToolCalls int        `json:"tool_calls,omitempty"`
	Usage     loop.Usage `json:"usage,omitempty"`
	Cost      *float64   `json:"cost,omitempty"`
	Error     string     `json:"error,omitempty"`
	ErrorKind string     `json:"-"`
}

// publicRecord strips the internal error_kind; success records gain cost
// (estimated dollars from the run's usage).
func publicRecord(record runRecord) runRecord {
	record.ErrorKind = ""
	if record.Error == "" && record.Usage != (loop.Usage{}) {
		cost := config.EstimateCost(record.Usage.InputTokens, record.Usage.OutputTokens, 0, record.Model)
		record.Cost = &cost
	}
	return record
}

// batchAsk is the ask_user handler for --batch workers: refuse, never block
// on stdin.
func batchAsk(question string, options []string) string {
	return "ask_user: not available in batch mode"
}

// runOne runs one prompt through the full per-run machinery. askHandler nil
// falls back to the loop's stdin-reading default; batch workers pass
// batchAsk.
// runGatewayBaseURL overrides the gateway endpoint for the run path (test
// seam — tests point it at a local httptest SSE server).
var runGatewayBaseURL = ""

func runOne(prompt string, opts *runOptions, sessionID string, stdout, stderr io.Writer, askHandler func(string, []string) string) runRecord {
	// Model first: the provider decides which key chain resolves. Free-tier
	// zen models are the exception — they need no key at all.
	modelID := config.ResolveModelID(opts.model)
	key, err := config.GetAPIKeyFor(config.ModelProvider(modelID))
	if err != nil && config.FreeTierModel(modelID) {
		key, err = "", nil
	}
	if err != nil {
		// Missing/invalid key: config already printed the instructions.
		fmt.Fprintln(stderr, err)
		return runRecord{SessionID: sessionID, Error: "no API key", ErrorKind: "config"}
	}
	projectDir := opts.dir
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}
	var agent *prompts.Agent
	if opts.agent != "" {
		state := agents.Load(projectDir)
		found := false
		for i := range state.Agents {
			if state.Agents[i].Name == opts.agent {
				agent = &prompts.Agent{Name: state.Agents[i].Name, Description: state.Agents[i].Description}
				found = true
				break
			}
		}
		if !found {
			return runRecord{SessionID: sessionID, Error: "no such agent: " + opts.agent, ErrorKind: "agent"}
		}
	}
	memoryRoot := opts.memoryRoot
	if memoryRoot == "" {
		memoryRoot = filepath.Join(projectDir, ".agent-memory")
	}
	mem := memory.NewMemory(memoryRoot)
	var cache *toolcache.ToolCache
	if !opts.noToolCache {
		cache = toolcache.NewToolCache(filepath.Join(projectDir, ".kaal", "tool-cache.json"))
	}
	toolRegistry := tools.NewRegistry(projectDir, opts.allowDangerous, cache, mem)
	baseURL := config.ModelBaseURL(modelID)
	if runGatewayBaseURL != "" {
		baseURL = runGatewayBaseURL
	}
	gw := &gateway.Gateway{BaseURL: baseURL, APIKey: key, Model: modelID}
	if opts.workerTransport {
		// P6 §4.3: batch workers get a transport per worker goroutine.
		gw.Opener = gateway.NewClientOpener()
	}
	agentLoop := loop.NewAgentLoop(gw, toolRegistry, mem, sessionID,
		loop.WithMaxSteps(opts.maxSteps),
		loop.WithAllowDangerous(opts.allowDangerous),
		loop.WithResume(opts.resume != ""),
		loop.WithEnableVerify(!opts.noVerify),
		loop.WithAgent(agent),
		loop.WithAskHandler(askHandler),
		loop.WithSessionWriter(opts.sessionWriter),
	)
	toolCalls := 0
	connectHost := baseURL
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		connectHost = u.Host
	}
	fmt.Fprintf(stderr, "◌ opening %s · %s…\r", connectHost, modelID)
	noted := false // flips on the stream's first sign of life
	emit := func(ev loop.AgentEvent) {
		if !noted {
			noted = true
			fmt.Fprint(stderr, "\r\033[2K") // erase the connecting line in place
		}
		switch ev.Kind {
		case loop.EventContent:
			fmt.Fprint(stdout, ev.Text)
		case loop.EventReasoning:
			if opts.verbose {
				fmt.Fprintf(stderr, "[think] %s\n", ev.Text)
			}
		case loop.EventVerify:
			fmt.Fprintf(stderr, "[verify] %s\n", ev.Text)
		case loop.EventToolStart:
			toolCalls++
		}
	}

	stop := startProgress(opts, stderr)
	answer, runErr := agentLoop.Run(prompt, emit)
	if !noted {
		fmt.Fprint(stderr, "\r\033[2K") // nothing ever streamed: clear the line
	}
	if stop != nil {
		stop()
		clearProgressLine(stderr)
	}
	if runErr != nil {
		var le *loop.LoopError
		if errors.As(runErr, &le) {
			return runRecord{SessionID: sessionID, Error: runErr.Error(), ErrorKind: "loop"}
		}
		return runRecord{SessionID: sessionID, Error: runErr.Error(), ErrorKind: "gateway"}
	}
	return runRecord{
		SessionID: sessionID,
		Answer:    answer,
		Model:     modelID,
		Steps:     agentLoop.Steps,
		ToolCalls: toolCalls,
		Usage:     agentLoop.Usage,
	}
}

// startProgress runs a live elapsed-time ticker on stderr (perceived
// responsiveness). Off for --verbose (reasoning already streams live),
// --batch (workers would interleave), and non-TTY stderr (pipes stay clean).
func startProgress(opts *runOptions, stderr io.Writer) func() {
	if opts.verbose || opts.batch != "" {
		return nil
	}
	f, ok := stderr.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	done := make(chan struct{})
	go func() {
		start := time.Now()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprintf(f, "\r💭 working %4.1fs", time.Since(start).Seconds())
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func clearProgressLine(stderr io.Writer) {
	if f, ok := stderr.(*os.File); ok && isTerminal(f) {
		fmt.Fprint(f, "\r"+strings.Repeat(" ", 32)+"\r")
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// -- batch ---------------------------------------------------------------------

// readBatchPrompts reads a --batch file: a JSON array of strings, else one
// prompt per line. Blank lines and empty strings are dropped in both modes.
// A file that is valid JSON but not a string array falls back to the
// line-per-prompt interpretation.
func readBatchPrompts(path string) ([]string, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var prompts []string
	stripped := strings.TrimSpace(string(text))
	if stripped != "" {
		var parsed any
		if json.Unmarshal([]byte(stripped), &parsed) == nil {
			if list, ok := parsed.([]any); ok {
				allStrings := true
				for _, item := range list {
					if _, ok := item.(string); !ok {
						allStrings = false
						break
					}
				}
				if allStrings {
					for _, item := range list {
						if s := strings.TrimSpace(item.(string)); s != "" {
							prompts = append(prompts, s)
						}
					}
				}
			}
		}
	}
	if len(prompts) == 0 {
		for _, line := range strings.Split(string(text), "\n") {
			if s := strings.TrimSpace(line); s != "" {
				prompts = append(prompts, s)
			}
		}
	}
	return prompts, nil
}

// runBatch runs every prompt in the --batch file concurrently; one session
// each. Records land in file order whatever the worker count. Exit: 0 if all
// tasks produced answers, 1 if any config/key/gateway error, 2 if any loop
// error; per-kind failure counts go to stderr.
func runBatch(opts *runOptions, stdout, stderr io.Writer) error {
	if opts.workers < 1 {
		fmt.Fprintln(stderr, "Error: --workers must be at least 1")
		return &exitError{code: 2}
	}
	opts.workerTransport = true
	prompts, err := readBatchPrompts(opts.batch)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: cannot read batch file: %v\n", err)
		return &exitError{code: 1}
	}
	if len(prompts) == 0 {
		fmt.Fprintln(stderr, "kaal: batch file contains no prompts")
		return &exitError{code: 1}
	}
	sessionIDs := make([]string, len(prompts))
	for i := range prompts {
		sessionIDs[i] = sessions.NewSessionID()
	}
	records := make([]runRecord, len(prompts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.workers)
	for i := range prompts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if !opts.jsonOut {
				fmt.Fprintf(stdout, "--- %s ---\n", sessionIDs[i])
			}
			records[i] = runOne(prompts[i], opts, sessionIDs[i], stdout, stderr, batchAsk)
		}(i)
	}
	wg.Wait()
	opts.sessionWriter.Flush() // flush-on-turn-end across all tasks

	counts := map[string]int{}
	for _, record := range records {
		if record.ErrorKind != "" {
			counts[record.ErrorKind]++
		}
	}
	failed := 0
	for _, c := range counts {
		failed += c
	}
	if failed > 0 {
		var detail []string
		for _, kind := range []string{"config", "gateway", "loop", "agent"} {
			if counts[kind] > 0 {
				detail = append(detail, fmt.Sprintf("%d %s", counts[kind], kind))
			}
		}
		fmt.Fprintf(stderr, "kaal: batch: %d of %d task(s) failed (%s)\n", failed, len(prompts), strings.Join(detail, ", "))
	}
	if opts.jsonOut {
		public := make([]runRecord, len(records))
		for i, record := range records {
			public[i] = publicRecord(record)
		}
		line, _ := jsonpy.Marshal(public)
		fmt.Fprintln(stdout, string(line))
	}
	if counts["config"] > 0 || counts["gateway"] > 0 || counts["agent"] > 0 {
		return &exitError{code: 1}
	}
	if counts["loop"] > 0 {
		return &exitError{code: 2}
	}
	return nil
}

// -- sessions -------------------------------------------------------------------

func newSessionsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "session store commands",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("kaal: unknown sessions subcommand")
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "list sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, entry := range sessions.ListSessions() {
				id, _ := entry["id"].(string)
				ts, _ := entry["ts"].(string)
				prompt, _ := entry["prompt"].(string)
				if ts == "" {
					ts = "-"
				}
				if len(prompt) > 80 {
					prompt = prompt[:80]
				}
				fmt.Fprintf(stdout, "%s  %s  %s\n", id, ts, prompt)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show <id>",
		Short: "print a session's events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return sessionsShow(args[0], stdout, stderr)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "delete <id>",
		Short: "delete a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if sessions.DeleteSession(args[0]) {
				fmt.Fprintf(stdout, "deleted %s\n", args[0])
				return nil
			}
			fmt.Fprintf(stdout, "no such session: %s\n", args[0])
			return &exitError{code: 1}
		},
	})
	prune := &cobra.Command{
		Use:   "prune",
		Short: "delete old sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			keep, _ := cmd.Flags().GetInt("keep")
			deleted := sessions.PruneSessions(keep)
			if len(deleted) == 0 {
				fmt.Fprintln(stdout, "nothing to prune")
				return nil
			}
			for _, sessionID := range deleted {
				fmt.Fprintf(stdout, "deleted %s\n", sessionID)
			}
			return nil
		},
	}
	prune.Flags().Int("keep", 20, "keep the newest N sessions (default: 20)")
	cmd.AddCommand(prune)
	return cmd
}

func sessionsShow(sessionID string, stdout, stderr io.Writer) error {
	if _, err := os.Stat(filepath.Join(sessions.StoreDir(), sessionID+".jsonl")); err != nil {
		fmt.Fprintf(stderr, "kaal: no such session: %s\n", sessionID)
		return &exitError{code: 1}
	}
	for _, record := range sessions.ReadEvents(sessionID) {
		ts, _ := record["ts"].(string)
		etype, _ := record["type"].(string)
		data, _ := record["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}
		compact, _ := json.Marshal(data)
		fmt.Fprintf(stdout, "%s | %s | %s\n", ts, etype, compact)
	}
	return nil
}

// -- doctor ---------------------------------------------------------------------

// doctorGatewayURL is the reachability probe target (a seam for tests; the
// real target is the gateway base URL).
var doctorGatewayURL = config.BaseURL

func newDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "self-check the environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(stdout)
		},
	}
}

func runDoctor(stdout io.Writer) error {
	fmt.Fprintf(stdout, "go: %s\n", runtime.Version())

	terminal := os.Getenv("TERM_PROGRAM")
	if terminal == "" {
		terminal = os.Getenv("TERM")
	}
	if terminal == "" {
		terminal = "unknown"
	}
	fmt.Fprintf(stdout, "terminal: %s (font: see docs/terminal-setup.md)\n", terminal)

	keySource := apiKeySource()
	freeDefault := config.FreeTierModel(config.ResolveModelID(""))
	if keySource == "MISSING" && freeDefault {
		fmt.Fprintln(stdout, "api key: none (zen free tier is keyless)")
	} else {
		fmt.Fprintf(stdout, "api key: %s\n", keySource)
	}

	gatewayOK := gatewayReachable()
	fmt.Fprintf(stdout, "gateway: %s\n", map[bool]string{true: "reachable", false: "unreachable"}[gatewayOK])

	structurePath := filepath.Join(".kaal", "STRUCTURE.md")
	if raw, err := os.ReadFile(structurePath); err == nil {
		fmt.Fprintf(stdout, "structure cache: exists · %d entries\n", structureEntryCount(raw))
	} else {
		fmt.Fprintln(stdout, "structure cache: missing")
	}

	store := sessions.StoreDir()
	fileCount := 0
	if entries, err := os.ReadDir(store); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				fileCount++
			}
		}
	}
	fmt.Fprintf(stdout, "sessions dir: %s · %d files\n", store, fileCount)

	ok := (keySource != "MISSING" || freeDefault) && gatewayOK
	if !ok {
		fmt.Fprintln(stdout, "doctor: FAILED")
		return &exitError{code: 1}
	}
	return nil
}

// apiKeySource reports which API-key source resolves for the default model's
// provider, without ever printing the key itself.
func apiKeySource() string {
	provider := config.ModelProvider(config.ResolveModelID(""))
	if provider == config.ProviderCommandCode {
		if os.Getenv("CMD_API_KEY") != "" || os.Getenv("COMMANDCODE_API_KEY") != "" {
			return "env"
		}
		if config.LoadUserAPIKeyFor(provider) != "" {
			return "user store"
		}
		return "MISSING"
	}
	if os.Getenv("OPENCODE_API_KEY") != "" {
		return "env"
	}
	if config.LoadUserAPIKey() != "" {
		return "user store"
	}
	if _, err := config.GetAPIKey(); err == nil {
		return "omp store"
	}
	return "MISSING"
}

// gatewayReachable GETs the gateway base URL; any HTTP status counts as
// reachable. Never sends the API key.
func gatewayReachable() bool {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, doctorGatewayURL, nil)
	if err != nil {
		return false
	}
	// The proven UA — Cloudflare WAF (error 1010) blocks default UAs.
	req.Header.Set("User-Agent", doctorUA)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}

// structureEntryCount parses `Files: N · Dirs: M` from the STRUCTURE.md
// header.
func structureEntryCount(doc []byte) int {
	for _, line := range strings.Split(string(doc), "\n") {
		if !strings.HasPrefix(line, "Files:") {
			continue
		}
		files, dirs := 0, 0
		for _, part := range strings.Split(line, "·") {
			part = strings.TrimSpace(part)
			switch {
			case strings.HasPrefix(part, "Files:"):
				fmt.Sscanf(part[len("Files:"):], "%d", &files)
			case strings.HasPrefix(part, "Dirs:"):
				fmt.Sscanf(part[len("Dirs:"):], "%d", &dirs)
			}
		}
		return files + dirs
	}
	return 0
}

// -- update ----------------------------------------------------------------------

// resolveCheckout finds the kaal source checkout to update: the installer
// dir first (KAAL_INSTALL_DIR, else ~/.local/share/kaal), then the repo the
// running binary was launched from. A checkout counts with a .git dir OR a
// go.mod (the tarball install path has no git history).
func resolveCheckout() string {
	envDir := os.Getenv("KAAL_INSTALL_DIR")
	candidates := []string{}
	if envDir != "" {
		candidates = append(candidates, envDir)
	} else {
		home, err := os.UserHomeDir()
		if err == nil {
			candidates = append(candidates, filepath.Join(home, ".local", "share", "kaal"))
		}
	}
	for _, cand := range candidates {
		if hasGitOrGoMod(cand) {
			return cand
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

func hasGitOrGoMod(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func runCmdOut(cmd string, args []string, cwd string) (string, error) {
	proc := exec.Command(cmd, args...)
	proc.Dir = cwd
	out, err := proc.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = fmt.Sprintf("exit %v", proc.ProcessState.ExitCode())
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func newUpdateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "pull and reinstall the latest kaal",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(stdout, stderr)
		},
	}
}

func runUpdate(stdout, stderr io.Writer) error {
	checkout := resolveCheckout()
	_, gitErr := exec.LookPath("git")
	hasGit := gitErr == nil
	_, goErr := exec.LookPath("go")
	hasGo := goErr == nil
	switch {
	case checkout != "" && hasGit && hasGo:
		// Source install with both tools: pull and rebuild in place.
		return updateFromGit(checkout, stdout, stderr)
	case checkout != "" && hasGo:
		// Source install but no git: overlay the main-branch tarball and rebuild.
		return updateTarball(checkout, stdout, stderr)
	default:
		// Binary install (no rebuildable checkout) or no Go toolchain:
		// fetch the prebuilt release binary.
		return updateFromRelease(stdout, stderr)
	}
}

func updateFromGit(checkout string, stdout, stderr io.Writer) error {
	before, err := runCmdOut("git", []string{"rev-parse", "--short", "HEAD"}, checkout)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	if _, err := runCmdOut("git", []string{"pull", "--ff-only"}, checkout); err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	after, err := runCmdOut("git", []string{"rev-parse", "--short", "HEAD"}, checkout)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	subject, err := runCmdOut("git", []string{"log", "-1", "--format=%s"}, checkout)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	if before == after {
		fmt.Fprintf(stdout, "kaal is up to date (%s).\n", after)
		return nil
	}
	// New commit pulled: rebuild the program into the checkout's venv.
	if !rebuildCheckout(checkout, stderr) {
		return &exitError{code: 1}
	}
	fmt.Fprintf(stdout, "kaal updated: %s -> %s (%s)\n", before, after, subject)
	fmt.Fprintf(stdout, "kaal rebuilt at %s — restart kaal to use the new build.\n", filepath.Join(checkout, "kaal"))
	return nil
}

// rebuildCheckout rebuilds the kaal binary from the checkout's Go source,
// landing at <checkout>/kaal (the README build command, CGO off for static).
func rebuildCheckout(checkout string, stderr io.Writer) bool {
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Fprintln(stderr,
			"kaal: pulled, but `go` is not on PATH — see README.md for build instructions")
		return false
	}
	proc := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", "kaal", "./cmd/kaal")
	proc.Dir = checkout
	proc.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := proc.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		fmt.Fprintf(stderr, "kaal: pulled, but rebuild failed: %s\n", msg)
		return false
	}
	return true
}

// updateTarball is the git-less update: fetch the main-branch tarball and
// overlay it on the checkout (the running install survives; stale code dirs
// are cleared first so upstream deletions do not linger).
func updateTarball(checkout string, stdout, stderr io.Writer) error {
	url := kaalUpdateURL
	fmt.Fprintf(stdout, "kaal: fetching %s\n", url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	// The proven UA — Cloudflare WAF (error 1010) blocks default UAs.
	req.Header.Set("User-Agent", doctorUA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	tr := tar.NewReader(gz)
	src := filepath.Join(os.TempDir(), fmt.Sprintf("kaal-update-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(src, 0o755); err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	defer os.RemoveAll(src)
	top := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
			return &exitError{code: 1}
		}
		if hdr.Name == "" {
			continue
		}
		if top == "" {
			top = strings.Split(hdr.Name, "/")[0]
		}
		name := strings.TrimPrefix(hdr.Name, top+"/")
		if name == "" {
			continue
		}
		target := filepath.Join(src, filepath.FromSlash(name))
		if hdr.Typeflag == tar.TypeDir {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		f, err := os.Create(target)
		if err != nil {
			continue
		}
		_, _ = io.Copy(f, tr)
		f.Close()
	}
	// Only the code dirs are fully owned by the tarball; live files
	// (AGENTS.md, docs/, .githooks, .gitignore, README.md) must never be
	// deleted — upstream deletions there are non-fatal and linger safely.
	for _, stale := range []string{"cmd", "internal"} {
		path := filepath.Join(checkout, stale)
		_ = os.RemoveAll(path)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	for _, child := range entries {
		dest := filepath.Join(checkout, child.Name())
		if child.IsDir() {
			_ = copyTree(filepath.Join(src, child.Name()), dest)
		} else {
			raw, err := os.ReadFile(filepath.Join(src, child.Name()))
			if err == nil {
				_ = os.WriteFile(dest, raw, 0o644)
			}
		}
	}
	if !rebuildCheckout(checkout, stderr) {
		return &exitError{code: 1}
	}
	fmt.Fprintln(stdout, "kaal updated from the main tarball.")
	fmt.Fprintf(stdout, "kaal rebuilt at %s — restart kaal to use the new build.\n", filepath.Join(checkout, "kaal"))
	return nil
}

func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		return os.WriteFile(target, raw, 0o644)
	})
}

// -- release-fetch update -----------------------------------------------------------

// releaseAssetName is the conventional release asset for this platform:
// kaal-<goos>-<goarch> (with .exe on Windows).
func releaseAssetName() string {
	name := "kaal-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// compareVersions orders dotted numeric versions ("0.3", "1.4.2"): -1, 0,
// +1 when a is older, equal, or newer than b.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// updateFromRelease is the binary-install update: fetch the prebuilt
// release binary for this platform from GitHub Releases, probe its version,
// and atomically replace the running executable. It needs no git checkout
// and no Go toolchain.
func updateFromRelease(stdout, stderr io.Writer) error {
	target := updateExecutable()
	if target == "" {
		fmt.Fprintln(stderr, "kaal: update failed: cannot locate the running kaal binary")
		return &exitError{code: 1}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, kaalReleaseAPI, nil)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	req.Header.Set("User-Agent", doctorUA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintln(stderr,
			"kaal: no release published yet — install from source (see README.md), "+
				"or run `kaal update` with git and go on PATH")
		return &exitError{code: 1}
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "kaal: update failed: release endpoint returned HTTP %d\n", resp.StatusCode)
		return &exitError{code: 1}
	}
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(payload, &rel); err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: bad release metadata: %v\n", err)
		return &exitError{code: 1}
	}
	asset := releaseAssetName()
	url := ""
	for _, a := range rel.Assets {
		if a.Name == asset {
			url = a.BrowserDownloadURL
			break
		}
	}
	if url == "" {
		fmt.Fprintf(stderr, "kaal: release %s has no %s asset\n", rel.TagName, asset)
		return &exitError{code: 1}
	}
	// Download into the target's directory so the final rename stays on one
	// filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".kaal-update-*")
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	dlReq.Header.Set("User-Agent", doctorUA)
	dlResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "kaal: update failed: asset download returned HTTP %d\n", dlResp.StatusCode)
		return &exitError{code: 1}
	}
	if _, err := io.Copy(tmp, dlResp.Body); err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: %v\n", err)
		return &exitError{code: 1}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmpPath, 0o755)
	}
	// The downloaded binary must answer --version like a kaal before we swap
	// it over the running executable.
	fetched, err := probeVersion(tmpPath)
	if err != nil {
		fmt.Fprintf(stderr, "kaal: update failed: downloaded binary is not a valid kaal: %v\n", err)
		return &exitError{code: 1}
	}
	if compareVersions(fetched, version) <= 0 {
		fmt.Fprintf(stdout, "kaal is up to date (%s).\n", version)
		return nil
	}
	if err := os.Rename(tmpPath, target); err != nil {
		// Windows may refuse to rename over a running executable; fall back
		// to remove-then-rename.
		_ = os.Remove(target)
		if err2 := os.Rename(tmpPath, target); err2 != nil {
			fmt.Fprintf(stderr, "kaal: update failed: cannot replace %s: %v\n", target, err2)
			return &exitError{code: 1}
		}
	}
	fmt.Fprintf(stdout, "kaal updated: %s -> %s (%s).\n", version, fetched, rel.TagName)
	fmt.Fprintln(stdout, "restart kaal to use the new build.")
	return nil
}

// probeVersion runs <path> --version and returns the dotted version.
func probeVersion(path string) (string, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", err
	}
	var v string
	if _, err := fmt.Sscanf(string(out), "kaal %s", &v); err != nil || v == "" {
		return "", fmt.Errorf("unexpected --version output %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// -- diagrams ---------------------------------------------------------------------

func newDiagramsCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "diagrams <file>",
		Short: "render a mermaid .mmd file via termaid",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiagrams(args[0], stdout, stderr)
		},
	}
}

// runDiagrams renders a mermaid .mmd file as terminal Unicode art via
// termaid (a dev tool, not the agent surface — shelling out per §9.5).
func runDiagrams(file string, stdout, stderr io.Writer) error {
	termaid, err := exec.LookPath("termaid")
	if err != nil {
		fmt.Fprintln(stderr,
			"kaal: termaid not found — install it with: uv tool install termaid "+
				"(or pip install kaal[diagrams])")
		return &exitError{code: 1}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	proc := exec.CommandContext(ctx, termaid, file)
	out, runErr := proc.CombinedOutput()
	if runErr != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = runErr.Error()
		}
		fmt.Fprintf(stderr, "kaal: termaid failed: %s\n", msg)
		return &exitError{code: 1}
	}
	fmt.Fprint(stdout, string(out))
	return nil
}
