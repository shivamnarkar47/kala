// Ported from tests/test_tools.py (556 lines) — schemas, execution, path
// safety, DENY list, caps. One known divergence: the Python
// test_grep_backreference_falls_back_to_python case is not ported — Go's
// RE2 (like rg) rejects backreferences, so the pure-Go fallback cannot match
// Python's `re` there; the fallback still triggers, but returns the invalid-
// regex error string instead of matches.
package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/toolcache"
	"github.com/kaal/kaal/internal/tools"
)

type env struct {
	dir string
	reg *tools.Registry
}

func setup(t *testing.T, opts ...func(*tools.Registry)) *env {
	t.Helper()
	dir := t.TempDir()
	reg := tools.NewRegistry(dir, false, nil, nil)
	for _, opt := range opts {
		opt(reg)
	}
	return &env{dir: dir, reg: reg}
}

func (e *env) make(rel, content string) string {
	path := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
	return path
}

func (e *env) readFile(rel string) string {
	raw, err := os.ReadFile(filepath.Join(e.dir, rel))
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func exec(t *testing.T, reg *tools.Registry, name string, args map[string]any) string {
	t.Helper()
	result, err := reg.Execute(context.Background(), name, args)
	if err != nil {
		t.Fatalf("execute(%s): %v", name, err)
	}
	return result
}

func execErr(t *testing.T, reg *tools.Registry, name string, args map[string]any) error {
	t.Helper()
	_, err := reg.Execute(context.Background(), name, args)
	return err
}

func TestEditOldTextNotFound(t *testing.T) {
	e := setup(t)
	e.make("a.txt", "hello world")
	if got := exec(t, e.reg, "edit", map[string]any{"path": "a.txt", "old_text": "missing", "new_text": "x"}); got != "old_text not found" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteOutsideCwdRejected(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "write", map[string]any{"path": "../evil.txt", "content": "x"})
	if !strings.HasPrefix(result, "blocked: ") {
		t.Fatalf("got %q", result)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.dir), "evil.txt")); err == nil {
		t.Fatal("escaped write landed")
	}
}

func TestBashDenyList(t *testing.T) {
	e := setup(t)
	if got := exec(t, e.reg, "bash", map[string]any{"command": "rm -rf /tmp/x"}); got != tools.DenyMessage {
		t.Fatalf("got %q", got)
	}
}

func TestBashAllowDangerousDisablesDenyCheck(t *testing.T) {
	e := setup(t, func(r *tools.Registry) { r.AllowDangerous() })
	_ = e
	reg := tools.NewRegistry(e.dir, true, nil, nil)
	if got := exec(t, reg, "bash", map[string]any{"command": `echo "git push"`}); !strings.Contains(got, "git push") {
		t.Fatalf("got %q", got)
	}
}

func TestDenyPatternsMatch(t *testing.T) {
	reg := tools.NewRegistry(t.TempDir(), false, nil, nil)
	cases := []struct {
		cmd  string
		want bool
	}{
		{"rm -rf /", true},
		{"git reset --hard HEAD", true},
		{"echo hello", false},
	}
	for _, c := range cases {
		result, err := reg.Execute(context.Background(), "bash", map[string]any{"command": c.cmd})
		if err != nil {
			t.Fatalf("%q: %v", c.cmd, err)
		}
		got := result == tools.DenyMessage
		if got != c.want {
			t.Fatalf("%q: want blocked=%v, got %v", c.cmd, c.want, got)
		}
	}
}

func TestGrepSkipsDenyDirs(t *testing.T) {
	e := setup(t)
	e.make("node_modules/x.txt", "needle\n")
	e.make("src/y.txt", "needle\n")
	result := exec(t, e.reg, "grep", map[string]any{"pattern": "needle"})
	if !strings.Contains(result, "src/y.txt:1: needle") {
		t.Fatalf("missing match: %q", result)
	}
	if strings.Contains(result, "node_modules") {
		t.Fatalf("skip dir leaked: %q", result)
	}
}

func TestBashOutputCapped(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "bash", map[string]any{"command": "python3 -c \"print('x' * 20000)\""})
	if len(result) > tools.MaxResultChars+len(tools.TruncatedSuffix) {
		t.Fatalf("result too long: %d", len(result))
	}
	if !strings.HasSuffix(result, tools.TruncatedSuffix) {
		t.Fatalf("no truncation suffix: %q", result)
	}
}

func TestReadOutputCapped(t *testing.T) {
	e := setup(t)
	e.make("big.txt", strings.Repeat("x", 20_000))
	result := exec(t, e.reg, "read", map[string]any{"path": "big.txt"})
	if len(result) > tools.MaxResultChars+len(tools.TruncatedSuffix) {
		t.Fatalf("result too long: %d", len(result))
	}
	if !strings.HasSuffix(result, tools.TruncatedSuffix) {
		t.Fatalf("no truncation suffix")
	}
}

func TestUnknownToolRaises(t *testing.T) {
	e := setup(t)
	if err := execErr(t, e.reg, "no_such_tool", map[string]any{}); err == nil {
		t.Fatal("want error")
	}
}

func TestNonDictArgsRaise(t *testing.T) {
	e := setup(t)
	// The registry interface takes a map; a nil map is the non-dict stand-in.
	if err := execErr(t, e.reg, "bash", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestWriteInsideCwdRoundTrip(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "write", map[string]any{"path": "notes.txt", "content": "hello\nworld"})
	if !strings.HasPrefix(result, "wrote notes.txt") || !strings.Contains(result, "bytes") {
		t.Fatalf("got %q", result)
	}
	if got := exec(t, e.reg, "read", map[string]any{"path": "notes.txt"}); got != "hello\nworld" {
		t.Fatalf("read back: %q", got)
	}
	if got := e.readFile("notes.txt"); got != "hello\nworld" {
		t.Fatalf("file: %q", got)
	}
}

func TestReadMissingFile(t *testing.T) {
	e := setup(t)
	if got := exec(t, e.reg, "read", map[string]any{"path": "nope.txt"}); !strings.HasPrefix(got, "read: ") {
		t.Fatalf("got %q", got)
	}
}

func TestReadDirectoryListing(t *testing.T) {
	e := setup(t)
	e.make("src/a.py", "x")
	e.make("src/nested/b.py", "x")
	e.make("top.txt", "x")
	result := exec(t, e.reg, "read", map[string]any{"path": "."})
	if !strings.Contains(result, "src/") || !strings.Contains(result, "src/nested/") || !strings.Contains(result, "top.txt") {
		t.Fatalf("listing: %q", result)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	e := setup(t)
	e.make("lines.txt", "one\ntwo\nthree\nfour\n")
	if got := exec(t, e.reg, "read", map[string]any{"path": "lines.txt", "offset": 2, "limit": 2}); got != "two\nthree\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditMultipleMatchesRequiresAll(t *testing.T) {
	e := setup(t)
	e.make("b.txt", "a b a")
	result := exec(t, e.reg, "edit", map[string]any{"path": "b.txt", "old_text": "a", "new_text": "x"})
	if result != "old_text matches 2 times; pass all=true to replace all" {
		t.Fatalf("got %q", result)
	}
	if got := e.readFile("b.txt"); got != "a b a" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestEditSingleReplacement(t *testing.T) {
	e := setup(t)
	e.make("d.txt", "a b")
	result := exec(t, e.reg, "edit", map[string]any{"path": "d.txt", "old_text": "a", "new_text": "x"})
	if !strings.HasPrefix(result, "edited d.txt") {
		t.Fatalf("got %q", result)
	}
	if got := e.readFile("d.txt"); got != "x b" {
		t.Fatalf("file: %q", got)
	}
}

func TestEditAllReplacesEverywhere(t *testing.T) {
	e := setup(t)
	e.make("c.txt", "a b a a")
	exec(t, e.reg, "edit", map[string]any{"path": "c.txt", "old_text": "a", "new_text": "x", "all": true})
	if got := e.readFile("c.txt"); got != "x b x x" {
		t.Fatalf("file: %q", got)
	}
}

func TestMemoryAppendWithoutStore(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "memory_append", map[string]any{"section": "decisions", "text": "note"})
	if result != "memory_append: no memory store configured" {
		t.Fatalf("got %q", result)
	}
}

func TestMemoryAppendInvalidSection(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "memory_append", map[string]any{"section": "bogus", "text": "note"})
	if !strings.HasPrefix(result, "memory_append: invalid section") {
		t.Fatalf("got %q", result)
	}
}

func TestSchemasCoverAllTools(t *testing.T) {
	e := setup(t)
	schemas := e.reg.Schemas()
	names := map[string]bool{}
	for _, s := range schemas {
		schema := s.(tools.SchemaFunction)
		names[schema.Function.Name] = true
		if schema.Type != "function" {
			t.Fatalf("type: %s", schema.Type)
		}
		if schema.Function.Description == "" {
			t.Fatalf("no description for %s", schema.Function.Name)
		}
		if schema.Function.Parameters["type"] != "object" {
			t.Fatalf("parameters type for %s", schema.Function.Name)
		}
	}
	for _, want := range []string{"read", "grep", "glob", "write", "edit", "bash", "memory_append", "spawn_agent", "ask_user", "spawn_parallel_task"} {
		if !names[want] {
			t.Fatalf("missing schema %s", want)
		}
	}
}

func TestSpawnAgentSchema(t *testing.T) {
	e := setup(t)
	schema := findSchema(t, e.reg, "spawn_agent")
	params := schema.Function.Parameters
	if required, _ := params["required"].([]string); len(required) != 1 || required[0] != "task" {
		t.Fatalf("required: %v", params["required"])
	}
	props := params["properties"].(map[string]any)
	if maxSteps := asFloat(props["max_steps"].(map[string]any)["maximum"]); maxSteps != 5 {
		t.Fatalf("max_steps maximum: %v", maxSteps)
	}
	if timeout := asFloat(props["timeout"].(map[string]any)["maximum"]); timeout != 300 {
		t.Fatalf("timeout maximum: %v", timeout)
	}
	if _, ok := props["dir"]; !ok {
		t.Fatal("missing dir prop")
	}
	if !strings.Contains(schema.Function.Description, "session_id") {
		t.Fatal("description must mention session_id")
	}
}

func TestSpawnAgentWithoutHandler(t *testing.T) {
	e := setup(t)
	if got := exec(t, e.reg, "spawn_agent", map[string]any{"task": "do the thing"}); got != "spawn_agent: not available in this context" {
		t.Fatalf("got %q", got)
	}
}

func TestSpawnAgentWithStubHandler(t *testing.T) {
	e := setup(t)
	var calls [][4]any
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnHandler(func(task string, dir *string, maxSteps, timeout int) string {
		var d any
		if dir != nil {
			d = *dir
		}
		calls = append(calls, [4]any{task, d, maxSteps, timeout})
		return `{"answer": "done", "steps": 1, "usage": {}, "session_id": "n-1"}`
	})
	result := exec(t, reg, "spawn_agent", map[string]any{"task": "do it", "dir": "sub", "max_steps": 3, "timeout": 42})
	var summary map[string]any
	if err := json.Unmarshal([]byte(result), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["answer"] != "done" {
		t.Fatalf("summary: %v", summary)
	}
	if len(calls) != 1 || calls[0][0] != "do it" || calls[0][1] != "sub" || calls[0][2] != 3 || calls[0][3] != 42 {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestSpawnAgentHandlerDefaults(t *testing.T) {
	e := setup(t)
	var calls [][4]any
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnHandler(func(task string, dir *string, maxSteps, timeout int) string {
		var d any
		if dir != nil {
			d = *dir
		}
		calls = append(calls, [4]any{task, d, maxSteps, timeout})
		return "ok"
	})
	exec(t, reg, "spawn_agent", map[string]any{"task": "t"})
	if len(calls) != 1 || calls[0][0] != "t" || calls[0][1] != nil || calls[0][2] != 5 || calls[0][3] != 120 {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestSpawnAgentClampsOutOfRange(t *testing.T) {
	e := setup(t)
	var calls [][4]any
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnHandler(func(task string, dir *string, maxSteps, timeout int) string {
		var d any
		if dir != nil {
			d = *dir
		}
		calls = append(calls, [4]any{task, d, maxSteps, timeout})
		return "ok"
	})
	exec(t, reg, "spawn_agent", map[string]any{"task": "t", "max_steps": 99, "timeout": 9999})
	if calls[0][2] != 5 || calls[0][3] != 300 {
		t.Fatalf("clamp high: %+v", calls[0])
	}
	exec(t, reg, "spawn_agent", map[string]any{"task": "t", "max_steps": 0, "timeout": 0})
	if calls[1][2] != 1 || calls[1][3] != 1 {
		t.Fatalf("clamp low: %+v", calls[1])
	}
}

func TestAskUserWithoutHandler(t *testing.T) {
	e := setup(t)
	if got := exec(t, e.reg, "ask_user", map[string]any{"question": "Continue?"}); got != "ask_user: not available in this context" {
		t.Fatalf("got %q", got)
	}
}

func TestAskUserWithStubHandler(t *testing.T) {
	e := setup(t)
	var calls [][2]any
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetAskHandler(func(question string, options []string) string {
		calls = append(calls, [2]any{question, options})
		return "yes"
	})
	result := exec(t, reg, "ask_user", map[string]any{"question": "Continue?", "options": []any{"yes", "no"}})
	if result != "yes" {
		t.Fatalf("got %q", result)
	}
	if len(calls) != 1 || calls[0][0] != "Continue?" {
		t.Fatalf("calls: %+v", calls)
	}
	if opts, ok := calls[0][1].([]string); !ok || len(opts) != 2 || opts[0] != "yes" {
		t.Fatalf("options: %+v", calls[0][1])
	}
}

func TestAskUserOptionsDefaultNone(t *testing.T) {
	e := setup(t)
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetAskHandler(func(question string, options []string) string {
		return question + " / " + strOrNil(options)
	})
	if got := exec(t, reg, "ask_user", map[string]any{"question": "Continue?"}); got != "Continue? / None" {
		t.Fatalf("got %q", got)
	}
}

func strOrNil(options []string) string {
	if options == nil {
		return "None"
	}
	return "[" + strings.Join(options, " ") + "]"
}

func TestAskUserOptionsMustBeStringArray(t *testing.T) {
	e := setup(t)
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetAskHandler(func(question string, options []string) string { return "ok" })
	result := exec(t, reg, "ask_user", map[string]any{"question": "q", "options": []any{1, 2}})
	if !strings.HasPrefix(result, "ask_user: options must be an array of strings") {
		t.Fatalf("got %q", result)
	}
}

func TestSpawnParallelTaskWithoutHandler(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "spawn_parallel_task", map[string]any{"tasks": []any{map[string]any{"task": "do the thing"}}})
	if result != "spawn_parallel_task: not available in this context" {
		t.Fatalf("got %q", result)
	}
}

func TestSpawnParallelTaskWithStubManyHandler(t *testing.T) {
	e := setup(t)
	var calls []any
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnManyHandler(func(tasks []map[string]any, timeout int) string {
		calls = append(calls, tasks)
		return `[{"index": 0, "answer": "done", "steps": 1, "usage": {}, "session_id": "m-1"}]`
	})
	result := exec(t, reg, "spawn_parallel_task", map[string]any{
		"tasks":   []any{map[string]any{"task": "a", "max_steps": 3, "timeout": 42}, map[string]any{"task": "b"}},
		"timeout": 99,
	})
	var records []any
	if err := json.Unmarshal([]byte(result), &records); err != nil {
		t.Fatal(err)
	}
	if records[0].(map[string]any)["answer"] != "done" {
		t.Fatalf("records: %v", records)
	}
	tasks := calls[0].([]map[string]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks: %+v", tasks)
	}
	if tasks[0]["task"] != "a" || tasks[0]["max_steps"] != 3 || tasks[0]["timeout"] != 42 {
		t.Fatalf("task 0: %+v", tasks[0])
	}
	if tasks[1]["task"] != "b" || tasks[1]["max_steps"] != 5 || tasks[1]["timeout"] != 99 {
		t.Fatalf("task 1: %+v", tasks[1])
	}
}

func TestSpawnParallelTaskClampsPerTask(t *testing.T) {
	e := setup(t)
	var calls []any
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnManyHandler(func(tasks []map[string]any, timeout int) string {
		calls = append(calls, tasks)
		return "[]"
	})
	exec(t, reg, "spawn_parallel_task", map[string]any{
		"tasks": []any{map[string]any{"task": "a", "max_steps": 99, "timeout": 9999}},
	})
	tasks := calls[0].([]map[string]any)
	if tasks[0]["max_steps"] != 5 || tasks[0]["timeout"] != 300 {
		t.Fatalf("clamps: %+v", tasks[0])
	}
}

func TestSpawnParallelTaskRequiresNonEmptyArray(t *testing.T) {
	e := setup(t)
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnManyHandler(func(tasks []map[string]any, timeout int) string { return "[]" })
	if got := exec(t, reg, "spawn_parallel_task", map[string]any{"tasks": []any{}}); !strings.HasPrefix(got, "spawn_parallel_task: tasks must be a non-empty array") {
		t.Fatalf("got %q", got)
	}
	if got := exec(t, reg, "spawn_parallel_task", map[string]any{"tasks": "nope"}); !strings.HasPrefix(got, "spawn_parallel_task: tasks must be a non-empty array") {
		t.Fatalf("got %q", got)
	}
}

func TestSpawnParallelTaskRequiresTaskInEach(t *testing.T) {
	e := setup(t)
	reg := tools.NewRegistry(e.dir, false, nil, nil)
	reg.SetSpawnManyHandler(func(tasks []map[string]any, timeout int) string { return "[]" })
	got := exec(t, reg, "spawn_parallel_task", map[string]any{"tasks": []any{map[string]any{"max_steps": 2}}})
	if !strings.HasPrefix(got, "spawn_parallel_task: tasks[0]: missing required argument: task") {
		t.Fatalf("got %q", got)
	}
}

func TestAskUserSchema(t *testing.T) {
	e := setup(t)
	schema := findSchema(t, e.reg, "ask_user")
	params := schema.Function.Parameters
	if required, _ := params["required"].([]string); len(required) != 1 || required[0] != "question" {
		t.Fatalf("required: %v", params["required"])
	}
	props := params["properties"].(map[string]any)
	options := props["options"].(map[string]any)
	if options["type"] != "array" {
		t.Fatalf("options type: %v", options["type"])
	}
	if items := options["items"].(map[string]any); items["type"] != "string" {
		t.Fatalf("options items: %v", items["type"])
	}
}

func TestReadRangeOnHugeFile(t *testing.T) {
	e := setup(t)
	var sb strings.Builder
	for i := 1; i <= 100_000; i++ {
		sb.WriteString("line " + itoa(i) + "\n")
	}
	e.make("huge.txt", sb.String())
	if got := exec(t, e.reg, "read", map[string]any{"path": "huge.txt", "offset": 5, "limit": 3}); got != "line 5\nline 6\nline 7\n" {
		t.Fatalf("got %q", got)
	}
	if got := exec(t, e.reg, "read", map[string]any{"path": "huge.txt", "limit": 2}); got != "line 1\nline 2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditRangeScopedReplacement(t *testing.T) {
	e := setup(t)
	e.make("r.txt", "line1\nneedle\nline3\nline4\nline5\nline6\nline7\nline8\nneedle\nline10\n")
	exec(t, e.reg, "edit", map[string]any{"path": "r.txt", "old_text": "needle", "new_text": "NINE", "offset": 9, "limit": 2})
	if got := e.readFile("r.txt"); got != "line1\nneedle\nline3\nline4\nline5\nline6\nline7\nline8\nNINE\nline10\n" {
		t.Fatalf("file: %q", got)
	}
}

func TestGrepStopsScanningAtCap(t *testing.T) {
	e := setup(t)
	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		sb.WriteString("needle " + itoa(i) + "\n")
	}
	e.make("many.txt", sb.String())
	e.make("sub/sentinel.txt", "needle sentinel\n")
	result := exec(t, e.reg, "grep", map[string]any{"pattern": "needle"})
	if len(result) > tools.MaxResultChars+len(tools.TruncatedSuffix) {
		t.Fatalf("result too long: %d", len(result))
	}
	if !strings.HasSuffix(result, tools.TruncatedSuffix) {
		t.Fatal("no truncation suffix")
	}
	if !strings.HasPrefix(result, "many.txt:1: needle 1") {
		t.Fatalf("prefix: %q", result[:40])
	}
	if strings.Contains(result, "sentinel") {
		t.Fatal("post-cap file scanned")
	}
}

func TestReadNoLimitCapsAt10k(t *testing.T) {
	e := setup(t)
	var sb strings.Builder
	for i := 1; i <= 2_000_000; i++ {
		sb.WriteString("line " + itoa(i) + "\n")
	}
	e.make("huge2.txt", sb.String())
	result := exec(t, e.reg, "read", map[string]any{"path": "huge2.txt"})
	if len(result) != tools.MaxResultChars+len(tools.TruncatedSuffix) {
		t.Fatalf("length: %d", len(result))
	}
	if !strings.HasSuffix(result, tools.TruncatedSuffix) {
		t.Fatal("no truncation suffix")
	}
	if !strings.HasPrefix(result, "line 1\n") {
		t.Fatalf("prefix: %q", result[:16])
	}
	// offset without limit still honors the start line on the capped path.
	tail := exec(t, e.reg, "read", map[string]any{"path": "huge2.txt", "offset": 1_999_999})
	if tail != "line 1999999\nline 2000000\n" {
		t.Fatalf("tail: %q", tail)
	}
}

func TestEditRangeBeyondEOF(t *testing.T) {
	e := setup(t)
	e.make("s.txt", "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n")
	if got := exec(t, e.reg, "edit", map[string]any{"path": "s.txt", "old_text": "line9", "new_text": "NINE", "offset": 90, "limit": 5}); got != "old_text not found" {
		t.Fatalf("got %q", got)
	}
}

func TestBashSanitizedPath(t *testing.T) {
	e := setup(t)
	result := exec(t, e.reg, "bash", map[string]any{"command": "echo $PATH"})
	parts := strings.Split(strings.TrimSpace(result), ":")
	want := []string{"/usr/local/bin", "/usr/bin", "/bin"}
	if len(parts) != len(want) {
		t.Fatalf("PATH: %q", result)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("PATH: %q", result)
		}
	}
}

func TestBashSanitizedPathPrependsProjectVenv(t *testing.T) {
	e := setup(t)
	if err := os.MkdirAll(filepath.Join(e.dir, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := exec(t, e.reg, "bash", map[string]any{"command": "echo $PATH"})
	parts := strings.Split(strings.TrimSpace(result), ":")
	want := []string{filepath.Join(e.dir, ".venv", "bin"), "/usr/local/bin", "/usr/bin", "/bin"}
	if len(parts) != len(want) {
		t.Fatalf("PATH: %q", result)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("PATH: %q", result)
		}
	}
}

func TestGrepSkipsKaalDir(t *testing.T) {
	e := setup(t)
	e.make(".kaal/tool-cache.json", `{"needle": "cached secret"}`)
	e.make("visible.txt", "needle visible\n")
	result := exec(t, e.reg, "grep", map[string]any{"pattern": "needle"})
	if !strings.Contains(result, "visible.txt:1: needle visible") {
		t.Fatalf("missing match: %q", result)
	}
	if strings.Contains(result, "tool-cache") {
		t.Fatalf(".kaal scanned: %q", result)
	}
}

func TestGrepInvalidPatternReportsErrorViaFallback(t *testing.T) {
	e := setup(t)
	e.make("x.txt", "aa\n")
	result := exec(t, e.reg, "grep", map[string]any{"pattern": "[z-a]"})
	if !strings.Contains(result, "invalid regex") {
		t.Fatalf("got %q", result)
	}
}

func TestSchemasMemoized(t *testing.T) {
	e := setup(t)
	first := e.reg.Schemas()
	second := e.reg.Schemas()
	if len(first) != 10 {
		t.Fatalf("schema count: %d", len(first))
	}
	// Fresh top-level list, shared inner structs.
	if &first[0] == &second[0] {
		t.Fatal("top-level lists must be distinct")
	}
	for i := range first {
		f, ok1 := first[i].(tools.SchemaFunction)
		s, ok2 := second[i].(tools.SchemaFunction)
		if !ok1 || !ok2 {
			t.Fatal("bad schema type")
		}
		if f.Function.Name != s.Function.Name || f.Function.Description != s.Function.Description {
			t.Fatalf("schema %d drifted", i)
		}
	}
}

// -- cache counters ----------------------------------------------------------

func cachedRegistry(t *testing.T, dir string) *tools.Registry {
	t.Helper()
	return tools.NewRegistry(dir, false, toolcache.NewToolCache(filepath.Join(dir, ".kaal", "tool-cache.json")), nil)
}

func TestFirstReadMissesSecondHits(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644)
	reg := cachedRegistry(t, dir)
	reg.BeginBatch([]string{"read"}, "sig-1")
	_, _ = reg.Execute(context.Background(), "read", map[string]any{"path": "a.txt"})
	_, _ = reg.Execute(context.Background(), "read", map[string]any{"path": "a.txt"})
	if reg.CacheMisses() != 1 || reg.CacheHits() != 1 {
		t.Fatalf("hits %d misses %d", reg.CacheHits(), reg.CacheMisses())
	}
	if rate := reg.CacheHitRate(); rate != 0.5 {
		t.Fatalf("rate: %v", rate)
	}
}

func TestNoLookupsRateIsNone(t *testing.T) {
	dir := t.TempDir()
	reg := cachedRegistry(t, dir)
	if reg.CacheHits() != 0 || reg.CacheMisses() != 0 {
		t.Fatal("counters not zero")
	}
	if rate := reg.CacheHitRate(); rate != -1 {
		t.Fatalf("rate: %v", rate)
	}
}

func TestMutatorBatchBypassCountsNeither(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644)
	reg := cachedRegistry(t, dir)
	reg.BeginBatch([]string{"write", "read"}, "sig-1")
	_, _ = reg.Execute(context.Background(), "read", map[string]any{"path": "a.txt"})
	if reg.CacheHits() != 0 || reg.CacheMisses() != 0 {
		t.Fatalf("hits %d misses %d", reg.CacheHits(), reg.CacheMisses())
	}
	if rate := reg.CacheHitRate(); rate != -1 {
		t.Fatalf("rate: %v", rate)
	}
}

func TestUncachedRegistryNeverLooksUp(t *testing.T) {
	dir := t.TempDir()
	reg := tools.NewRegistry(dir, false, nil, nil)
	reg.BeginBatch([]string{"read"}, "sig-1")
	_, _ = reg.Execute(context.Background(), "read", map[string]any{"path": "a.txt"})
	if reg.CacheHits() != 0 || reg.CacheMisses() != 0 {
		t.Fatal("uncached registry must not look up")
	}
}

func findSchema(t *testing.T, reg *tools.Registry, name string) tools.SchemaFunction {
	t.Helper()
	for _, s := range reg.Schemas() {
		schema := s.(tools.SchemaFunction)
		if schema.Function.Name == name {
			return schema
		}
	}
	t.Fatalf("schema %s not found", name)
	return tools.SchemaFunction{}
}

func itoa(n int) string { return strconv.Itoa(n) }

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func TestGrepRGPythonEquivalence(t *testing.T) {
	// rg (when available) and the pure-Go fallback return the same matches
	// (the Python test_grep_rg_python_equivalence).
	e := setup(t)
	e.make("alpha.txt", "needle alpha\nplain line\n")
	e.make("beta.txt", "NEEDLE upper\n")
	e.make("sub/gamma.txt", "nothing here\nneedle gamma\n")
	if _, err := osexec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	rgResult := exec(t, e.reg, "grep", map[string]any{"pattern": "needle"})
	rgCase := exec(t, e.reg, "grep", map[string]any{"pattern": "NEEDLE", "case": true})
	old := tools.RgLookup
	tools.RgLookup = func(string) (string, error) { return "", errors.New("no rg") }
	defer func() { tools.RgLookup = old }()
	pyResult := exec(t, e.reg, "grep", map[string]any{"pattern": "needle"})
	pyCase := exec(t, e.reg, "grep", map[string]any{"pattern": "NEEDLE", "case": true})

	if sortedLines(rgResult) != sortedLines(pyResult) {
		t.Fatalf("rg vs fallback:\\nrg: %q\\npy: %q", rgResult, pyResult)
	}
	if sortedLines(rgCase) != sortedLines(pyCase) {
		t.Fatalf("case rg vs fallback:\\nrg: %q\\npy: %q", rgCase, pyCase)
	}
	for _, want := range []string{"alpha.txt:1: needle alpha", "sub/gamma.txt:2: needle gamma", "beta.txt:1: NEEDLE upper"} {
		if !strings.Contains(rgResult, want) {
			t.Fatalf("missing %q in %q", want, rgResult)
		}
	}
	if strings.Contains(rgCase, "needle alpha") {
		t.Fatalf("case-sensitive must exclude: %q", rgCase)
	}
}

func sortedLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestGrepCapSentinelBothEngines(t *testing.T) {
	// The cap-sentinel guarantee holds for rg AND the pure-Go fallback.
	e := setup(t)
	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		sb.WriteString("needle " + itoa(i) + "\n")
	}
	e.make("many.txt", sb.String())
	e.make("sub/sentinel.txt", "needle sentinel\n")
	results := []string{exec(t, e.reg, "grep", map[string]any{"pattern": "needle"})}
	old := tools.RgLookup
	tools.RgLookup = func(string) (string, error) { return "", errors.New("no rg") }
	defer func() { tools.RgLookup = old }()
	results = append(results, exec(t, e.reg, "grep", map[string]any{"pattern": "needle"}))
	for _, result := range results {
		if len(result) > tools.MaxResultChars+len(tools.TruncatedSuffix) {
			t.Fatalf("result too long: %d", len(result))
		}
		if !strings.HasSuffix(result, tools.TruncatedSuffix) {
			t.Fatal("no truncation suffix")
		}
		if !strings.HasPrefix(result, "many.txt:1: needle 1") {
			t.Fatalf("prefix: %q", result[:40])
		}
		if strings.Contains(result, "sentinel") {
			t.Fatal("post-cap file appeared")
		}
	}
}
