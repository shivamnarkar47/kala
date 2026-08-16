// Ported from tests/test_memory.py (289 lines): memory digest/append/prune
// rules plus the prompt assembly tests.
package memory_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaal/kaal/internal/context"
	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/prompts"
	"github.com/kaal/kaal/internal/sessions"
)

var sections = []string{"project-state", "decisions", "patterns", "lessons-learned"}

func TestDigestCapsContentLines(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	_, _ = mem.Append("project-state", strings.Join(lines("detail line ", 80), "\n"))
	digest := mem.LoadDigest()
	// The single seeded file: path present, at most 60 content lines.
	if !strings.Contains(digest, filepath.Join(root, "project-state.md")) {
		t.Fatal("path missing from digest")
	}
	block := strings.Split(strings.Split(digest, "### Project State")[1], "### Decisions")[0]
	blockLines := strings.Split(block, "\n")
	if len(blockLines) < 2 {
		t.Fatalf("block too short: %q", block)
	}
	contentLines := 0
	for _, ln := range blockLines[1:] {
		if strings.TrimSpace(ln) != "" {
			contentLines++
		}
	}
	if contentLines > 60 {
		t.Fatalf("content lines: %d", contentLines)
	}
	// The other three sections appear with their paths too.
	for _, section := range sections[1:] {
		if !strings.Contains(digest, filepath.Join(root, section+".md")) {
			t.Fatalf("section %s missing from digest", section)
		}
	}
}

func TestDigestTokenCapAllSections(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	for _, section := range sections {
		_, _ = mem.Append(section, strings.Join(lines(section+" data line ", 70), "\n"))
	}
	digest := mem.LoadDigest()
	if context.EstimateTokens(digest) > 4000 {
		t.Fatalf("digest %d tokens over cap", context.EstimateTokens(digest))
	}
	for _, section := range sections {
		if !strings.Contains(digest, filepath.Join(root, section+".md")) {
			t.Fatalf("section %s path missing", section)
		}
	}
}

func TestAppendDedupes(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	path, err := mem.Append("decisions", "Use JSONL for sessions.")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "decisions.md") {
		t.Fatalf("path: %q", path)
	}
	again, err := mem.Append("decisions", "Use JSONL for sessions.")
	if err != nil {
		t.Fatal(err)
	}
	if again != "already recorded" {
		t.Fatalf("dedupe: %q", again)
	}
	content, _ := os.ReadFile(filepath.Join(root, "decisions.md"))
	if strings.Count(string(content), "## ") != 1 {
		t.Fatalf("sections: %q", content)
	}
	if !strings.Contains(string(content), "Use JSONL for sessions.") {
		t.Fatal("text missing")
	}
}

func TestAppendPrunesOldestSection(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	for i := 0; i < 5; i++ {
		text := "run " + itoa(i) + "\n" + strings.Join(lines("body line ", 149), "\n")
		_, _ = mem.Append("patterns", text)
	}
	content, _ := os.ReadFile(filepath.Join(root, "patterns.md"))
	fileLines := strings.Split(string(content), "\n")
	if len(fileLines) > 200 {
		t.Fatalf("lines: %d", len(fileLines))
	}
	// Oldest sections dropped, newest kept.
	if strings.Count(string(content), "## ") != 1 {
		t.Fatalf("sections: %q", content)
	}
	if strings.Contains(string(content), "run 0") {
		t.Fatal("oldest section survived")
	}
	if !strings.Contains(string(content), "run 4") {
		t.Fatal("newest section lost")
	}
}

func TestConcurrentAppendIsLossless(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	section := "lessons-learned"
	texts := []string{}
	for i := 0; i < 8; i++ {
		texts = append(texts, "note-"+itoa(i))
	}
	var wg sync.WaitGroup
	for _, text := range texts {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			_, _ = mem.Append(section, text)
		}(text)
	}
	wg.Wait()
	content, _ := os.ReadFile(filepath.Join(root, section+".md"))
	for _, text := range texts {
		if !strings.Contains(string(content), text) {
			t.Fatalf("lost note %s", text)
		}
	}
}

func TestInvalidSectionRaises(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	if _, err := mem.Append("bogus", "x"); err == nil {
		t.Fatal("append bogus must error")
	}
	if _, err := mem.FilePath("bogus"); err == nil {
		t.Fatal("file_path bogus must error")
	}
	// Valid sections keep working after the failure.
	if _, err := mem.FilePath("decisions"); err != nil {
		t.Fatalf("valid section failed: %v", err)
	}
}

func TestRecordSessionSummary(t *testing.T) {
	root := t.TempDir()
	mem := memory.NewMemory(root)
	mem.RecordSessionSummary("write the file", "ok")
	content, _ := os.ReadFile(filepath.Join(root, "project-state.md"))
	if !strings.Contains(string(content), "session: write the file → ok") {
		t.Fatalf("summary missing: %q", content)
	}
}

func TestSessionRoundTripThroughStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	sid := sessions.NewSessionID()
	toolCalls := []any{
		map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "memory_append", "arguments": "{}"}},
	}
	_ = sessions.AppendEvent(sid, map[string]any{"type": "user", "data": map[string]any{"content": "hello"}})
	_ = sessions.AppendEvent(sid, map[string]any{
		"type": "assistant",
		"data": map[string]any{
			"content":           "hi",
			"reasoning_content": "thinking step",
			"tool_calls":        toolCalls,
		},
	})
	_ = sessions.AppendEvent(sid, map[string]any{"type": "tool_result", "data": map[string]any{"tool_call_id": "call_1", "content": "ok"}})
	msgs := sessions.LoadMessages(sid)
	if len(msgs) != 3 {
		t.Fatalf("msgs: %v", msgs)
	}
	if msgs[1]["reasoning_content"] != "thinking step" {
		t.Fatalf("reasoning: %v", msgs[1])
	}
	if msgs[1]["tool_calls"] == nil {
		t.Fatal("tool_calls missing")
	}
	list := sessions.ListSessions()
	if len(list) != 1 || list[0]["id"] != sid || list[0]["prompt"] != "hello" {
		t.Fatalf("list: %v", list)
	}
}

func TestLoadMessagesMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	if msgs := sessions.LoadMessages("does-not-exist"); len(msgs) != 0 {
		t.Fatalf("msgs: %v", msgs)
	}
	sid := sessions.NewSessionID()
	_ = os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sessions", sid+".jsonl"), []byte(
		"{\"ts\": \"t\", \"type\": \"user\", \"data\": {\"content\": \"ok\"}}\n"+
			"THIS IS NOT JSON\n",
	), 0o644)
	if msgs := sessions.LoadMessages(sid); len(msgs) != 1 || msgs[0]["content"] != "ok" {
		t.Fatalf("msgs: %v", msgs)
	}
}

func TestAppendEventInvalidType(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", filepath.Join(dir, "sessions"))
	if err := sessions.AppendEvent(sessions.NewSessionID(), map[string]any{"type": "bogus", "data": map[string]any{}}); err == nil {
		t.Fatal("want error")
	}
}

func TestStoreDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", dir)
	if sessions.StoreDir() != dir {
		t.Fatalf("StoreDir: %q", sessions.StoreDir())
	}
	if list := sessions.ListSessions(); len(list) != 0 {
		t.Fatalf("list: %v", list)
	}
}

// -- prompts -------------------------------------------------------------------

func TestBuildProjectContextWithAgents(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj")
	_ = os.Mkdir(cwd, 0o755)
	_ = os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("# Agents\n\nBuild with stdlib only.\n"), 0o644)
	ctx := prompts.BuildProjectContext(cwd)
	if !strings.Contains(ctx, time.Now().Format("2006-01-02")) {
		t.Fatal("date missing")
	}
	if !strings.Contains(ctx, cwd) {
		t.Fatal("cwd missing")
	}
	if !strings.Contains(ctx, "## AGENTS.md (first 200 lines)") || !strings.Contains(ctx, "# Agents") || !strings.Contains(ctx, "Build with stdlib only.") {
		t.Fatal("AGENTS.md block missing")
	}
}

func TestBuildProjectContextWithoutAgents(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj2")
	_ = os.Mkdir(cwd, 0o755)
	ctx := prompts.BuildProjectContext(cwd)
	if !strings.Contains(ctx, "No AGENTS.md") {
		t.Fatal("absence note missing")
	}
	if strings.Contains(ctx, "## AGENTS.md (first 200 lines)") {
		t.Fatal("block present without AGENTS.md")
	}
}

func TestProjectContextIncludesStructureCache(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj3")
	_ = os.MkdirAll(filepath.Join(cwd, ".kaal"), 0o755)
	_ = os.WriteFile(filepath.Join(cwd, ".kaal", "STRUCTURE.md"), []byte("# Project Structure\nRoot: x\n## Tree\n└── README.md (5 B)\n"), 0o644)
	ctx := prompts.BuildProjectContext(cwd)
	if !strings.Contains(ctx, "## Project structure") || !strings.Contains(ctx, "└── README.md (5 B)") {
		t.Fatal("structure block missing")
	}
	if !strings.Contains(ctx, "re-read it if the files change") {
		t.Fatal("re-read note missing")
	}
}

func TestProjectContextMissingStructureCache(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj4")
	_ = os.Mkdir(cwd, 0o755)
	ctx := prompts.BuildProjectContext(cwd)
	if !strings.Contains(ctx, "No structure cache yet") {
		t.Fatal("absence note missing")
	}
}

func TestBuildSystemPromptWithoutAgent(t *testing.T) {
	prompt := prompts.BuildSystemPrompt("### Project State\n- nothing yet", "Date: 2026-08-02\nCWD: /tmp/x", nil)
	if !strings.Contains(prompt, "## Memory Guidance") || !strings.Contains(prompt, "## Project") {
		t.Fatal("blocks missing")
	}
	if strings.Contains(prompt, "## Agent") {
		t.Fatal("agent block without agent")
	}
}

func TestBuildSystemPromptWithAgent(t *testing.T) {
	prompt := prompts.BuildSystemPrompt("### Project State\n- nothing yet", "Date: 2026-08-02\nCWD: /tmp/x",
		&prompts.Agent{Name: "Arjuna", Description: "the Precise Marksman"})
	for _, want := range []string{"## Agent", "Arjuna", "the Precise Marksman"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q", want)
		}
	}
	// The persona block comes after the project block.
	if strings.Index(prompt, "## Project") > strings.Index(prompt, "## Agent") {
		t.Fatal("agent block must follow project block")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	digest := "### Project State\n" +
		"path: /tmp/x/project-state.md\n" +
		"# Project State\n" +
		"- session: work → done"
	prompt := prompts.BuildSystemPrompt(digest, "Date: 2026-08-02\nCWD: /tmp/x", nil)
	for _, want := range []string{
		"kaal — DeepSeek V4 Flash harness agent",
		"When you need a fact or a file operation, call a tool. You may batch " +
			"independent tool calls. The harness parses your DSML tool calls " +
			"automatically.",
		"Final answers are plain text. Never emit tool markup, " +
			"`reasoning_content`, or `<think>` blocks in your visible answer.",
		".agent-memory/",
		"## Memory Guidance",
		"## Project",
		"Mahabharata",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if !strings.Contains(prompt, digest) || !strings.Contains(prompt, "Date: 2026-08-02") {
		t.Fatal("dynamic blocks missing")
	}
	if !strings.Contains(prompt, prompts.FixedPrefix) {
		t.Fatal("fixed prefix missing")
	}
	if context.EstimateTokens(prompts.FixedPrefix) > 8000 {
		t.Fatalf("fixed prefix too long: %d tokens", context.EstimateTokens(prompts.FixedPrefix))
	}
}

func lines(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, prefix+itoa(i))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
