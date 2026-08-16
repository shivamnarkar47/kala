// Package tools ports harness/tools.py: the tool registry — OpenAI function
// schemas plus safe local execution. File paths are constrained to the
// project directory (no `..` escapes) and destructive shell commands hit a
// DENY list. Execute always returns a result string; only unknown tools and
// internal errors surface as errors.
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/toolcache"
)

const (
	MaxResultChars  = 10_000
	TruncatedSuffix = "…[truncated]"
)

// CacheableTools: results are pure reads of the project tree — safe to cache.
var CacheableTools = map[string]bool{"read": true, "grep": true, "glob": true}

// MutatorTools: tools that mutate state (files or memory) — a batch
// containing any of these bypasses cache lookups entirely and invalidates
// the cache after the batch.
var MutatorTools = map[string]bool{"write": true, "edit": true, "bash": true, "memory_append": true}

// DenyMessage is the DENY-list block notice.
const DenyMessage = "blocked by harness policy (destructive command)"

// denyPatterns are matched case-insensitively against the shell command
// before execution (skipped when the registry is allow_dangerous).
var denyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rm\s+-rf`),
	regexp.MustCompile(`(?i)git\s+push`),
	regexp.MustCompile(`(?i)git\s+reset\s+--hard`),
	regexp.MustCompile(`(?i)git\s+clean\s+-f`),
	regexp.MustCompile(`(?i)mkfs`),
	regexp.MustCompile(`(?i)dd\s+if=`),
	regexp.MustCompile(`(?i):\{\(\)`),
	regexp.MustCompile(`(?i)>\s*/dev/sd`),
}

var skipDirs = map[string]bool{".git": true, "__pycache__": true, "node_modules": true, ".kaal": true}

var memorySections = []string{"project-state", "decisions", "patterns", "lessons-learned"}

// ToolError is a hard failure: unknown tool or an internal error.
type ToolError struct{ msg string }

func (e *ToolError) Error() string { return e.msg }

func toolErr(format string, args ...any) *ToolError {
	return &ToolError{msg: fmt.Sprintf(format, args...)}
}

// SpawnHandler runs a nested agent (injected by the loop).
type SpawnHandler func(task string, dir *string, maxSteps, timeout int) string

// SpawnManyHandler runs several nested agents (injected by the loop).
type SpawnManyHandler func(tasks []map[string]any, timeout int) string

// AskHandler answers an ask_user question (injected by the loop).
type AskHandler func(question string, options []string) string

// Registry is the agent's tool registry: schemas for the API, safe
// execution.
type Registry struct {
	projectDir     string
	allowDangerous bool
	cache          *toolcache.ToolCache
	memory         *memory.Memory

	// Per-batch cache state (set by the loop via BeginBatch/EndBatch).
	cacheSignature string
	batchMutator   bool
	cacheHits      int
	cacheMisses    int

	// Nested-agent runners and the ask handler (injected by the loop).
	spawnHandler     SpawnHandler
	spawnManyHandler SpawnManyHandler
	askHandler       AskHandler

	// Handler-call visibility for tests (the CountingToolRegistry seam).
	readCalls int
}

// NewRegistry builds a registry. memory and cache may be nil.
func NewRegistry(projectDir string, allowDangerous bool, cache *toolcache.ToolCache, mem *memory.Memory) *Registry {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		abs = projectDir
	}
	return &Registry{projectDir: abs, allowDangerous: allowDangerous, cache: cache, memory: mem}
}

// ProjectDir returns the registry's project directory (absolute).
func (r *Registry) ProjectDir() string { return r.projectDir }

// AllowDangerous reports whether the DENY list is disabled.
func (r *Registry) AllowDangerous() bool { return r.allowDangerous }

// ReadCalls counts actual read-handler invocations (cache hits don't count).
func (r *Registry) ReadCalls() int { return r.readCalls }

// SetSpawnHandler injects the nested-agent runner used by spawn_agent.
func (r *Registry) SetSpawnHandler(h SpawnHandler) { r.spawnHandler = h }

// SetSpawnManyHandler injects the runner used by spawn_parallel_task.
func (r *Registry) SetSpawnManyHandler(h SpawnManyHandler) { r.spawnManyHandler = h }

// SetAskHandler injects the ask_user answer provider.
func (r *Registry) SetAskHandler(h AskHandler) { r.askHandler = h }

// BeginBatch opens a tool batch: pins the structure signature and detects
// mutators. A batch containing any mutator disables cache lookups for the
// entire step (the same-step write-then-read staleness hole).
func (r *Registry) BeginBatch(names []string, sig string) {
	if r.cache == nil {
		return
	}
	r.cacheSignature = sig
	r.batchMutator = false
	for _, n := range names {
		if MutatorTools[n] {
			r.batchMutator = true
			break
		}
	}
}

// EndBatch closes a tool batch: drops the cache after a mutating batch.
func (r *Registry) EndBatch(mutated bool) {
	if r.cache == nil {
		return
	}
	if mutated {
		r.cache.Drop()
	}
	r.batchMutator = false
	r.cacheSignature = ""
}

// CacheHits / CacheMisses expose cache-lookup visibility.
func (r *Registry) CacheHits() int   { return r.cacheHits }
func (r *Registry) CacheMisses() int { return r.cacheMisses }

// CacheHitRate returns the fraction of lookups that hit, or -1 when no
// lookups occurred (Python returns None).
func (r *Registry) CacheHitRate() float64 {
	lookups := r.cacheHits + r.cacheMisses
	if lookups == 0 {
		return -1
	}
	return float64(r.cacheHits) / float64(lookups)
}

// Execute runs tool name with args; returns a result string, or an error for
// unknown tools. Tool-level failures (blocked paths, missing args) are
// surfaced as result strings, exactly like Python.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		return "", toolErr("tool %s: arguments must be a dict", name)
	}
	handler, ok := r.handlers()[name]
	if !ok {
		return "", toolErr("unknown tool: %s", name)
	}
	// Read-only tools consult the tool-result cache when a structure
	// signature is available AND the batch contains no mutator.
	if r.cache != nil && r.cacheSignature != "" && !r.batchMutator && CacheableTools[name] {
		argsJSON, _ := json.Marshal(args)
		if cached, ok := r.cache.Get(name, string(argsJSON), r.cacheSignature); ok {
			r.cacheHits++
			return cached, nil
		}
		r.cacheMisses++
		result := handler(ctx, args)
		r.cache.Put(name, string(argsJSON), r.cacheSignature, result)
		return result, nil
	}
	return handler(ctx, args), nil
}

type handlerFunc func(ctx context.Context, args map[string]any) string

func (r *Registry) handlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"read":                r.toolRead,
		"grep":                r.toolGrep,
		"glob":                r.toolGlob,
		"write":               r.toolWrite,
		"edit":                r.toolEdit,
		"bash":                r.toolBash,
		"memory_append":       r.toolMemoryAppend,
		"spawn_agent":         r.toolSpawnAgent,
		"ask_user":            r.toolAskUser,
		"spawn_parallel_task": r.toolSpawnParallelTask,
	}
}

// -- argument plumbing -------------------------------------------------------

func requireString(args map[string]any, name, tool string) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return "", toolErr("%s: missing required argument: %s", tool, name)
	}
	return fmt.Sprint(v), nil
}

func intArg(args map[string]any, name, tool string, def int) (int, bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return def, false, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), true, nil
	case int:
		return n, true, nil
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true, nil
		}
	}
	return 0, true, toolErr("%s: invalid %s: %v", tool, name, v)
}

func boolArg(args map[string]any, name string) bool {
	v, _ := args[name].(bool)
	return v
}

// ResolveRelative resolves a user-supplied path against cwd, rejecting any
// escape. Absolute paths inside cwd are allowed; absolute paths outside it
// and `..` escapes return an error with a "blocked: ..." message.
func ResolveRelative(p, cwd string) (string, error) {
	base, err := filepath.Abs(cwd)
	if err != nil {
		base = cwd
	}
	joined := filepath.Join(base, filepath.FromSlash(p))
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	// Resolve symlinks when the path exists (Python's Path.resolve follows
	// them; a symlink escape must not bypass confinement).
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", toolErr("blocked: path escapes project directory: %s", p)
	}
	return abs, nil
}

// -- tools -------------------------------------------------------------------

func (r *Registry) toolRead(ctx context.Context, args map[string]any) string {
	r.readCalls++
	tool := "read"
	p, err := requireString(args, "path", tool)
	if err != nil {
		return err.Error()
	}
	offset, hasOffset, err := intArg(args, "offset", tool, 1)
	if err != nil {
		return err.Error()
	}
	limit, hasLimit, err := intArg(args, "limit", tool, 0)
	if err != nil {
		return err.Error()
	}
	if hasLimit && limit < 0 {
		return toolErr("read: limit must be >= 0").Error()
	}
	target, err := ResolveRelative(p, r.projectDir)
	if err != nil {
		return err.Error()
	}
	info, statErr := os.Stat(target)
	if statErr == nil && info.IsDir() {
		return capText(directoryListing(target))
	}
	if statErr != nil || !info.IsDir() {
		if os.IsNotExist(statErr) || (statErr == nil && !info.IsDir()) {
			if statErr == nil && !info.IsDir() {
				// regular file: fall through to read
			} else {
				return fmt.Sprintf("read: no such file: %s", p)
			}
		} else {
			return fmt.Sprintf("read: %s", statErr)
		}
	}
	text, err := readLines(target, offset, hasOffset, limit, hasLimit)
	if err != nil {
		return fmt.Sprintf("read: %s", err)
	}
	return capText(text)
}

// readLines mirrors _read_lines: offset is 1-based; a nil limit reads at most
// cap+slack characters (runes) after skipping the start lines.
func readLines(file string, offset int, hasOffset bool, limit int, hasLimit bool) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	start := 0
	if !hasOffset {
		start = 0 // offset defaults to 1 -> line 0
	} else {
		start = max(0, offset-1)
	}
	if !hasLimit {
		// Whole-file read: read at most cap + suffix slack (runes); the
		// caller truncates to the first 10k chars anyway. Skip `start`
		// lines first (lazily) to honor offset.
		for i := 0; i < start; i++ {
			if _, err := reader.ReadString('\n'); err != nil {
				break
			}
		}
		var sb strings.Builder
		for sb.Len() <= MaxResultChars+len(TruncatedSuffix) {
			chunk, err := reader.ReadString('\n')
			if chunk == "" && err != nil {
				break
			}
			sb.WriteString(chunk)
			if err != nil {
				break
			}
			if utf8.RuneCountInString(sb.String()) > MaxResultChars+len(TruncatedSuffix) {
				break
			}
		}
		return sb.String(), nil
	}
	// Stream only the requested window — never materialize the whole file.
	var sb strings.Builder
	for i := 0; i < start; i++ {
		if _, err := reader.ReadString('\n'); err != nil {
			return sb.String(), nil
		}
	}
	for i := 0; i < limit; i++ {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			break
		}
		sb.WriteString(line)
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

// directoryListing renders a depth-limited listing: immediate children, then
// one more level. Dirs and files are sorted separately, dirs first — exactly
// the Python _directory_listing order (parity for the gate).
func directoryListing(root string) string {
	var dirs, files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(rel, string(filepath.Separator))
		depth := len(parts)
		if depth == 1 {
			if d.IsDir() {
				dirs = append(dirs, d.Name()+"/")
			} else {
				files = append(files, d.Name())
			}
			return nil
		}
		if depth == 2 {
			parent := parts[0]
			if d.IsDir() {
				dirs = append(dirs, parent+"/"+d.Name()+"/")
			} else {
				files = append(files, parent+"/"+d.Name())
			}
			if d.IsDir() {
				return filepath.SkipDir // never descend past level 2
			}
			return nil
		}
		return filepath.SkipDir
	})
	sort.Strings(dirs)
	sort.Strings(files)
	return strings.Join(append(dirs, files...), "\n")
}

func (r *Registry) toolGrep(ctx context.Context, args map[string]any) string {
	tool := "grep"
	pattern, err := requireString(args, "pattern", tool)
	if err != nil {
		return err.Error()
	}
	root := r.projectDir
	if p, ok := args["path"]; ok && p != nil {
		resolved, err := ResolveRelative(fmt.Sprint(p), r.projectDir)
		if err != nil {
			return err.Error()
		}
		root = resolved
	}
	if skipDirs[filepath.Base(root)] {
		return "no matches for " + pattern
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Sprintf("grep: no such directory: %s", args["path"])
	}
	caseSensitive := boolArg(args, "case")
	if pattern != "" {
		// rg first when available; result == "" + fallback flag means "fall
		// back" (missing binary, rg error such as an unsupported regex
		// feature, or OSError).
		if result, ok := grepRG(pattern, root, caseSensitive, r.projectDir); ok {
			return result
		}
	}
	return grepPython(pattern, root, caseSensitive, r.projectDir)
}

// formatRGLine normalizes an rg output line to the harness
// 'path:lineno: text' format: strip the './' prefix and insert the separator
// space so the result matches the pure-Go scan byte for byte.
func formatRGLine(line string) string {
	if strings.HasPrefix(line, "./") {
		line = line[2:]
	}
	pathPart, rest, _ := strings.Cut(line, ":")
	lineno, text, _ := strings.Cut(rest, ":")
	return pathPart + ":" + lineno + ": " + text
}

// RgLookup is the rg-detection seam (tests force the pure-Go fallback).
var RgLookup = exec.LookPath

// grepRG is the rg-backed grep; ok=false means "fall back". Streams rg's
// stdout so scanning stops as soon as the result reaches the cap.
func grepRG(pattern, root string, caseSensitive bool, projectDir string) (string, bool) {
	if _, err := RgLookup("rg"); err != nil {
		return "", false
	}
	relRoot, err := filepath.Rel(projectDir, root)
	if err != nil {
		relRoot = "."
	}
	cmdArgs := []string{"--line-number", "--no-heading", "--color", "never", "--sort-files"}
	if !caseSensitive {
		cmdArgs = append(cmdArgs, "-i")
	}
	cmdArgs = append(cmdArgs,
		"--glob", "!.git/**",
		"--glob", "!node_modules/**",
		"--glob", "!__pycache__/**",
		"--glob", "!.kaal/**",
		"--", pattern, relRoot,
	)
	cmd := exec.Command("rg", cmdArgs...)
	cmd.Dir = projectDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", false
	}
	var matches []string
	totalChars := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		entry := formatRGLine(strings.TrimRight(scanner.Text(), "\n"))
		if len(matches) > 0 {
			totalChars++ // the "\n" separator
		}
		totalChars += utf8.RuneCountInString(entry)
		matches = append(matches, entry)
		if totalChars > MaxResultChars {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return capText(strings.Join(matches, "\n")), true
		}
	}
	_ = stdout.Close()
	err = cmd.Wait()
	if len(matches) > 0 {
		// Best effort: rg may have exited non-zero mid-stream; keep whatever
		// matched before the failure.
		return capText(strings.Join(matches, "\n")), true
	}
	if err == nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1) {
		return "no matches for " + pattern, true
	}
	return "", false // rg error (exit 2) or other non-zero -> Python fallback
}

// grepPython is the reference pure-Go grep scan; also the rg fallback.
//
// P6: files are scanned in parallel (≤4 workers) while the result stays
// deterministic — each file's entries are collected in walk order, a single
// huge file stops mid-scan at its own local cap, and the final join
// truncates in walk order. The observable contract is unchanged: post-cap
// files never appear in the result.
func grepPython(pattern, root string, caseSensitive bool, projectDir string) string {
	re, err := regexp.Compile(pattern)
	if !caseSensitive {
		re, err = regexp.Compile("(?i)" + pattern)
	}
	if err != nil {
		return fmt.Sprintf("grep: invalid regex %q: %s", pattern, err)
	}
	// Walk first, collecting files in deterministic order (noise dirs
	// skipped; the entry cap is not applied — grep scans everything below
	// the root, like the Python fallback).
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, p)
		return nil
	})

	results := make([][]string, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, file := range files {
		wg.Add(1)
		go func(i int, file string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = scanFileForGrep(file, re, projectDir)
		}(i, file)
	}
	wg.Wait()

	// Join in walk order; truncate at the cap (the authoritative cut). The
	// entry that crosses the cap IS included, then capText truncates — the
	// exact semantics of the pre-parallel scan.
	var matches []string
	totalChars := 0
	for _, entries := range results {
		for _, entry := range entries {
			if len(matches) > 0 {
				totalChars++ // the "\n" separator
			}
			totalChars += utf8.RuneCountInString(entry)
			matches = append(matches, entry)
			if totalChars > MaxResultChars {
				return capText(strings.Join(matches, "\n"))
			}
		}
	}
	if len(matches) == 0 {
		return "no matches for " + pattern
	}
	return capText(strings.Join(matches, "\n"))
}

// scanFileForGrep scans one file, collecting matching lines as
// 'relpath:lineno: text'. A single huge file stops mid-scan at its own
// local cap (the join truncates authoritatively in walk order).
func scanFileForGrep(file string, re *regexp.Regexp, projectDir string) []string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	rel, _ := filepath.Rel(projectDir, file)
	var entries []string
	localChars := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineno := 0
	for scanner.Scan() {
		lineno++
		line := scanner.Text()
		if !re.MatchString(line) {
			continue
		}
		entry := filepath.ToSlash(rel) + ":" + fmt.Sprint(lineno) + ": " + strings.TrimRight(line, "\r\n")
		entries = append(entries, entry)
		localChars += utf8.RuneCountInString(entry) + 1
		if localChars > MaxResultChars {
			break
		}
	}
	return entries
}

var errCap = errors.New("cap reached")

func (r *Registry) toolGlob(ctx context.Context, args map[string]any) string {
	tool := "glob"
	pattern, err := requireString(args, "pattern", tool)
	if err != nil {
		return err.Error()
	}
	root := r.projectDir
	if containsDotDot(pattern) {
		return fmt.Sprintf("blocked: path escapes project directory: %s", pattern)
	}
	matches, err := globMatch(root, pattern)
	if err != nil {
		return fmt.Sprintf("glob: invalid pattern %q: %s", pattern, err)
	}
	var lines []string
	for _, m := range matches {
		abs := filepath.Join(root, filepath.FromSlash(m))
		if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
			abs = resolved
		}
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		lines = append(lines, filepath.ToSlash(rel))
	}
	return capText(strings.Join(lines, "\n"))
}

func containsDotDot(pattern string) bool {
	for _, part := range strings.Split(filepath.ToSlash(pattern), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// globMatch matches a glob pattern relative to root, supporting `**` (zero
// or more directories) like Python's pathlib.glob.
func globMatch(root, pattern string) ([]string, error) {
	pattern = filepath.ToSlash(pattern)
	segs := strings.Split(pattern, "/")
	var out []string
	var walk func(dir string, i int, rel string)
	walk = func(dir string, i int, rel string) {
		if i == len(segs) {
			out = append(out, rel)
			return
		}
		seg := segs[i]
		if seg == "**" {
			walk(dir, i+1, rel) // zero segments
			entries, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range entries {
				if e.IsDir() {
					sub := e.Name()
					if rel != "" {
						sub = rel + "/" + sub
					}
					walk(filepath.Join(dir, e.Name()), i, sub)
				}
			}
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			ok, err := path.Match(seg, e.Name())
			if err != nil {
				return
			}
			if !ok {
				continue
			}
			sub := e.Name()
			if rel != "" {
				sub = rel + "/" + sub
			}
			if i == len(segs)-1 {
				out = append(out, sub)
			} else if e.IsDir() {
				walk(filepath.Join(dir, e.Name()), i+1, sub)
			}
		}
	}
	walk(root, 0, "")
	sort.Strings(out)
	return out, nil
}

func (r *Registry) toolWrite(ctx context.Context, args map[string]any) string {
	tool := "write"
	p, err := requireString(args, "path", tool)
	if err != nil {
		return err.Error()
	}
	content, err := requireString(args, "content", tool)
	if err != nil {
		return err.Error()
	}
	target, err := ResolveRelative(p, r.projectDir)
	if err != nil {
		return err.Error()
	}
	data := []byte(content)
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return fmt.Sprintf("write: %s", err)
	}
	return fmt.Sprintf("wrote %s (%d bytes)", p, len(data))
}

func (r *Registry) toolEdit(ctx context.Context, args map[string]any) string {
	tool := "edit"
	p, err := requireString(args, "path", tool)
	if err != nil {
		return err.Error()
	}
	oldText, err := requireString(args, "old_text", tool)
	if err != nil {
		return err.Error()
	}
	newText, err := requireString(args, "new_text", tool)
	if err != nil {
		return err.Error()
	}
	all := boolArg(args, "all")
	offset, hasOffset, err := intArg(args, "offset", tool, 1)
	if err != nil {
		return err.Error()
	}
	limit, hasLimit, err := intArg(args, "limit", tool, 0)
	if err != nil {
		return err.Error()
	}
	if hasLimit && limit < 0 {
		return toolErr("edit: limit must be >= 0").Error()
	}
	target, err := ResolveRelative(p, r.projectDir)
	if err != nil {
		return err.Error()
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("edit: no such file: %s", p)
		}
		return fmt.Sprintf("edit: %s", err)
	}
	if !utf8.Valid(raw) {
		return "edit: not valid UTF-8 text: invalid byte sequence"
	}
	text := string(raw)
	var updated string
	count := 0
	if hasOffset || hasLimit {
		// Scope the exact-substring replace to the 1-based line range
		// [offset, offset+limit): split, replace within the range only,
		// splice the result back into the line list.
		lines := splitKeepEnds(text)
		lo := min(max(0, offset-1), len(lines)) // Python slicing clamps; Go panics
		hi := len(lines)
		if hasLimit {
			hi = min(lo+limit, len(lines))
		}
		rangeText := strings.Join(lines[lo:hi], "")
		count = strings.Count(rangeText, oldText)
		if count == 0 {
			return "old_text not found"
		}
		if count > 1 && !all {
			return fmt.Sprintf("old_text matches %d times; pass all=true to replace all", count)
		}
		updatedRange := strings.ReplaceAll(rangeText, oldText, newText)
		if !all {
			updatedRange = strings.Replace(rangeText, oldText, newText, 1)
		}
		spliced := make([]string, 0, len(lines)-len(lines[lo:hi])+1)
		spliced = append(spliced, lines[:lo]...)
		spliced = append(spliced, updatedRange)
		spliced = append(spliced, lines[hi:]...)
		updated = strings.Join(spliced, "")
	} else {
		count = strings.Count(text, oldText)
		if count == 0 {
			return "old_text not found"
		}
		if count > 1 && !all {
			return fmt.Sprintf("old_text matches %d times; pass all=true to replace all", count)
		}
		if all {
			updated = strings.ReplaceAll(text, oldText, newText)
		} else {
			updated = strings.Replace(text, oldText, newText, 1)
		}
	}
	if err := os.WriteFile(target, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("edit: %s", err)
	}
	if all {
		return fmt.Sprintf("edited %s (%d replacements)", p, count)
	}
	return fmt.Sprintf("edited %s", p)
}

// splitKeepEnds splits into lines keeping the terminators (Python's
// splitlines(keepends=True)).
func splitKeepEnds(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func (r *Registry) toolBash(ctx context.Context, args map[string]any) string {
	tool := "bash"
	command, err := requireString(args, "command", tool)
	if err != nil {
		return err.Error()
	}
	timeout, _, err := intArg(args, "timeout", tool, 30)
	if err != nil {
		return err.Error()
	}
	if timeout == 0 {
		timeout = 30
	}
	if timeout > 300 {
		return fmt.Sprintf("bash: timeout must be at most 300 seconds (got %d)", timeout)
	}
	if !r.allowDangerous {
		for _, pattern := range denyPatterns {
			if pattern.MatchString(command) {
				return DenyMessage
			}
		}
	}
	// Sanitized environment: keep the ambient env but restrict PATH to the
	// project venv (when present) plus the platform's standard search dirs.
	env := os.Environ()
	env = append(env, "PATH="+bashPathEnv(r.projectDir))
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	shell, shellArg := bashShell()
	cmd := exec.CommandContext(cmdCtx, shell, shellArg, command)
	cmd.Dir = r.projectDir
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("bash: timed out after %ds", timeout)
		}
		// Command exited non-zero: its output is still the result.
	}
	return capText(string(out))
}

// bashShell returns the shell invocation for the platform: cmd.exe /C on
// Windows (there is no /bin/sh), /bin/sh -c everywhere else.
func bashShell() (shell string, arg string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", "/C"
	}
	return "/bin/sh", "-c"
}

// bashPathEnv builds the sanitized PATH for the bash tool: the project venv
// (Scripts on Windows, bin elsewhere) when present, plus the platform's
// standard search dirs (System32 on Windows, the POSIX dirs elsewhere).
func bashPathEnv(projectDir string) string {
	pathDirs := []string{}
	for _, v := range []string{
		filepath.Join(projectDir, ".venv", "Scripts"),
		filepath.Join(projectDir, ".venv", "bin"),
	} {
		if info, err := os.Stat(v); err == nil && info.IsDir() {
			pathDirs = append(pathDirs, v)
		}
	}
	if runtime.GOOS == "windows" {
		if sr := os.Getenv("SystemRoot"); sr != "" {
			pathDirs = append(pathDirs, filepath.Join(sr, "System32"), sr)
		}
	} else {
		pathDirs = append(pathDirs, "/usr/local/bin", "/usr/bin", "/bin")
	}
	return strings.Join(pathDirs, string(os.PathListSeparator))
}

func (r *Registry) toolMemoryAppend(ctx context.Context, args map[string]any) string {
	tool := "memory_append"
	section, err := requireString(args, "section", tool)
	if err != nil {
		return err.Error()
	}
	text, err := requireString(args, "text", tool)
	if err != nil {
		return err.Error()
	}
	valid := false
	for _, s := range memorySections {
		if s == section {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Sprintf("memory_append: invalid section: %s (expected one of %s)", section, strings.Join(memorySections, ", "))
	}
	if r.memory == nil {
		return "memory_append: no memory store configured"
	}
	result, err := r.memory.Append(section, text)
	if err != nil {
		return fmt.Sprintf("memory_append: memory store failed: %s", err)
	}
	return result
}

func (r *Registry) toolSpawnAgent(ctx context.Context, args map[string]any) string {
	tool := "spawn_agent"
	task, err := requireString(args, "task", tool)
	if err != nil {
		return err.Error()
	}
	if r.spawnHandler == nil {
		return "spawn_agent: not available in this context"
	}
	maxSteps, _, _ := intArg(args, "max_steps", tool, 5)
	timeout, _, _ := intArg(args, "timeout", tool, 120)
	maxSteps = clamp(maxSteps, 1, 5)
	timeout = clamp(timeout, 1, 300)
	var dir *string
	if d, ok := args["dir"]; ok && d != nil {
		s := fmt.Sprint(d)
		dir = &s
	}
	return r.spawnHandler(task, dir, maxSteps, timeout)
}

func (r *Registry) toolAskUser(ctx context.Context, args map[string]any) string {
	tool := "ask_user"
	question, err := requireString(args, "question", tool)
	if err != nil {
		return err.Error()
	}
	if r.askHandler == nil {
		return "ask_user: not available in this context"
	}
	var options []string
	if v, ok := args["options"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return "ask_user: options must be an array of strings"
		}
		options = make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return "ask_user: options must be an array of strings"
			}
			options = append(options, s)
		}
	}
	return r.askHandler(question, options)
}

func (r *Registry) toolSpawnParallelTask(ctx context.Context, args map[string]any) string {
	tool := "spawn_parallel_task"
	tasksVal, ok := args["tasks"]
	if !ok || tasksVal == nil {
		return "spawn_parallel_task: tasks must be a non-empty array"
	}
	tasks, ok := tasksVal.([]any)
	if !ok || len(tasks) == 0 {
		return "spawn_parallel_task: tasks must be a non-empty array"
	}
	if r.spawnManyHandler == nil {
		return "spawn_parallel_task: not available in this context"
	}
	timeout, _, _ := intArg(args, "timeout", tool, 120)
	if timeout == 0 {
		timeout = 120
	}
	timeout = clamp(timeout, 1, 300)
	clean := make([]map[string]any, 0, len(tasks))
	for index, taskVal := range tasks {
		task, ok := taskVal.(map[string]any)
		if !ok {
			return fmt.Sprintf("spawn_parallel_task: tasks[%d] must be an object", index)
		}
		taskText, ok := task["task"].(string)
		if !ok || taskText == "" {
			return fmt.Sprintf("spawn_parallel_task: tasks[%d]: missing required argument: task", index)
		}
		maxSteps, err := clampedInt(task["max_steps"], 5, 1, 5, index, "max_steps")
		if err != "" {
			return err
		}
		taskTimeout, err := clampedInt(task["timeout"], timeout, 1, 300, index, "timeout")
		if err != "" {
			return err
		}
		var dir any
		if d, ok := task["dir"]; ok {
			dir = d
		}
		clean = append(clean, map[string]any{
			"task": taskText, "dir": dir, "max_steps": maxSteps, "timeout": taskTimeout,
		})
	}
	return r.spawnManyHandler(clean, timeout)
}

func clampedInt(v any, def, lo, hi, index int, name string) (int, string) {
	if v == nil {
		return def, ""
	}
	var n int
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, fmt.Sprintf("spawn_parallel_task: tasks[%d]: invalid %s: %v", index, name, x)
		}
		n = int(i)
	default:
		return 0, fmt.Sprintf("spawn_parallel_task: tasks[%d]: invalid %s: %v", index, name, v)
	}
	return clamp(n, lo, hi), ""
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// capText truncates result strings longer than MaxResultChars (runes, like
// Python's len()).
func capText(text string) string {
	if utf8.RuneCountInString(text) > MaxResultChars {
		return truncateToRunes(text, MaxResultChars) + TruncatedSuffix
	}
	return text
}

func truncateToRunes(s string, n int) string {
	i := 0
	for count := 0; count < n && i < len(s); count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}
