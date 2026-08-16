// Package parity is the P7 gate's one-time harness: both armies run the
// same corpus and their outputs are diffed — final answers, session shape,
// tool-call sequences, and reasoning replay on turn 2+.
//
// The Python side runs through `uv run python` against the frozen harness/
// tree; the Go side drives internal/loop directly with the same scripted
// gateway. The test self-skips when the Python toolchain or harness/ is
// absent (after the burn it is a historical instrument).
package parity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/kaal/kaal/internal/gateway"
	"github.com/kaal/kaal/internal/loop"
	"github.com/kaal/kaal/internal/memory"
	"github.com/kaal/kaal/internal/messages"
	"github.com/kaal/kaal/internal/sessions"
	"github.com/kaal/kaal/internal/tools"
)

const (
	fw = "\uff5c" // fullwidth pipe ｜
	b  = "\u2581" // block glyph ▁
)

var (
	sOpen  = "<" + fw + "DSML" + fw + "tool_calls>"
	sClose = "</" + fw + "DSML" + fw + "tool_calls>"
	iOpen  = "<" + fw + "DSML" + fw + "invoke"
	iClose = "</" + fw + "DSML" + fw + "invoke>"
	pOpen  = "<" + fw + "DSML" + fw + "parameter"
	pClose = "</" + fw + "DSML" + fw + "parameter>"
)

func dsmlCall(name string, params map[string]string) string {
	var sb strings.Builder
	sb.WriteString(sOpen)
	sb.WriteString(iOpen + ` name="` + name + `">`)
	for k, v := range params {
		sb.WriteString(pOpen + ` name="` + k + `" string="true">` + v + pClose)
	}
	sb.WriteString(iClose)
	sb.WriteString(sClose)
	return sb.String()
}

// corpusCase is one gate case. Turns are lists of events; an event is
// ["content"|"reasoning"|"done", text] or ["tool_call", {id,name,
// arguments}].
type corpusCase struct {
	ID       string            `json:"id"`
	Prompt   string            `json:"prompt"`
	Files    map[string]string `json:"files,omitempty"`
	Hooks    []string          `json:"hooks,omitempty"`
	MaxSteps int               `json:"max_steps,omitempty"`
	Turns    [][][]any         `json:"turns"`
}

// turn builds one stream turn from events + a closing done event.
func turn(events [][]any, doneReason string) [][]any {
	out := append([][]any{}, events...)
	out = append(out, []any{"done", doneReason})
	return out
}

func contentTurn(text, reason string) [][]any {
	return turn([][]any{{"content", text}}, reason)
}

func toolCallTurn(calls ...messages.ToolCall) [][]any {
	events := [][]any{}
	for _, c := range calls {
		events = append(events, []any{"tool_call", map[string]any{"id": c.ID, "name": c.Name, "arguments": c.Arguments}})
	}
	return turn(events, "tool_calls")
}

var corpus = []corpusCase{
	{
		ID: "01-plain-answer", Prompt: "Say hello",
		Turns: [][][]any{contentTurn("Hello from the model.\n", "stop")},
	},
	{
		ID: "02-think-span", Prompt: "Think",
		Turns: [][][]any{contentTurn("<think>hmm</think>answer\n", "stop")},
	},
	{
		ID: "03-reasoning", Prompt: "Reason",
		Turns: [][][]any{turn([][]any{{"reasoning", "Let me check"}, {"content", "done\n"}}, "stop")},
	},
	{
		ID: "04-dsml-write-replay", Prompt: "Write the file",
		// Turn 1 streams reasoning + a generation-leading DSML envelope +
		// visible text; turn 2's wire must replay the reasoning verbatim.
		Turns: [][][]any{
			turn([][]any{{"reasoning", "Let me check the directory"}, {"content", dsmlCall("write", map[string]string{"path": "hello.txt", "content": "hi"})}, {"content", "I will write the file. "}}, "tool_calls"),
			contentTurn("Wrote hello.txt.\n", "stop"),
		},
	},
	{
		ID: "05-dsml-read", Prompt: "Read the file",
		Files: map[string]string{"a.txt": "hello\n"},
		Turns: [][][]any{
			contentTurn(dsmlCall("read", map[string]string{"path": "a.txt"}), "tool_calls"),
			contentTurn("got it\n", "stop"),
		},
	},
	{
		ID: "06-chained-invokes", Prompt: "Write then read",
		Files: map[string]string{"notes.txt": "old\n"},
		Turns: [][][]any{
			contentTurn(dsmlCall("write", map[string]string{"path": "notes.txt", "content": "new"})+"\n"+dsmlCall("read", map[string]string{"path": "notes.txt"}), "tool_calls"),
			contentTurn("both done\n", "stop"),
		},
	},
	{
		ID: "07-parallel-reads", Prompt: "Read three files",
		Files: map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"},
		Turns: [][][]any{
			toolCallTurn(
				messages.ToolCall{ID: "c1", Name: "read", Arguments: `{"path": "a.txt"}`},
				messages.ToolCall{ID: "c2", Name: "read", Arguments: `{"path": "b.txt"}`},
				messages.ToolCall{ID: "c3", Name: "read", Arguments: `{"path": "c.txt"}`},
			),
			contentTurn("all read\n", "stop"),
		},
	},
	{
		ID: "08-prose-quote", Prompt: "Explain the wire format",
		Turns: [][][]any{contentTurn("The envelope `<|DSML|tool_calls>` is what the model emits. The rest of this answer must be preserved completely, with every single word intact.\n", "stop")},
	},
	{
		ID: "09-complete-prose-envelope", Prompt: "Show the envelope",
		Turns: [][][]any{contentTurn("The write tool's envelope looks like this: "+dsmlCall("write", map[string]string{"path": "hello.txt", "content": "boom"})+" — but that is just an example, I will not actually call it.\n", "stop")},
	},
	{
		ID: "10-unclosed-section", Prompt: "Explain the format",
		Turns: [][][]any{contentTurn("The envelope starts with "+sOpen+iOpen+` name="x">`+" and then the answer continues here.\n", "stop")},
	},
	{
		ID: "11-tool-loop-abort", Prompt: "Loop",
		Turns: [][][]any{
			contentTurn(dsmlCall("write", map[string]string{"path": "hello.txt", "content": "hi"}), "tool_calls"),
			contentTurn(dsmlCall("write", map[string]string{"path": "hello.txt", "content": "hi"}), "tool_calls"),
			contentTurn(dsmlCall("write", map[string]string{"path": "hello.txt", "content": "hi"}), "tool_calls"),
		},
	},
	{
		// With the REAL registry on both sides, "read: no such file" is a
		// result string, not a failure — so the batch executes and the
		// max-steps abort is what both loops hit (a real parity path).
		ID: "12-max-steps-abort", Prompt: "Fail", MaxSteps: 1,
		Turns: [][][]any{toolCallTurn(
			messages.ToolCall{ID: "f0", Name: "read", Arguments: `{"path": "missing0.txt"}`},
			messages.ToolCall{ID: "f1", Name: "read", Arguments: `{"path": "missing1.txt"}`},
			messages.ToolCall{ID: "f2", Name: "read", Arguments: `{"path": "missing2.txt"}`},
			messages.ToolCall{ID: "f3", Name: "read", Arguments: `{"path": "missing3.txt"}`},
			messages.ToolCall{ID: "f4", Name: "read", Arguments: `{"path": "missing4.txt"}`},
		)},
	},
	{
		ID: "13-overflow-retry", Prompt: "Overflow",
		Turns: [][][]any{turn(nil, "length"), contentTurn("ok\n", "stop")},
	},
	{
		ID: "14-ask-user", Prompt: "Ask",
		Turns: [][][]any{
			toolCallTurn(messages.ToolCall{ID: "a1", Name: "ask_user", Arguments: `{"question": "Proceed?", "options": ["yes", "no"]}`}),
			contentTurn("Proceeding with your answer.\n", "stop"),
		},
	},
	{
		ID: "15-grep-tool", Prompt: "Find the needle",
		Files: map[string]string{"src/x.txt": "needle here\nplain line\n", "node_modules/skip.txt": "needle skipped\n"},
		Turns: [][][]any{
			contentTurn(dsmlCall("grep", map[string]string{"pattern": "needle"}), "tool_calls"),
			contentTurn("found\n", "stop"),
		},
	},
	{
		ID: "16-glob-tool", Prompt: "List python files",
		Files: map[string]string{"src/a.py": "x\n", "src/nested/b.py": "y\n"},
		Turns: [][][]any{
			toolCallTurn(messages.ToolCall{ID: "g1", Name: "glob", Arguments: `{"pattern": "src/**/*.py"}`}),
			contentTurn("listed\n", "stop"),
		},
	},
	{
		ID: "17-edit-tool", Prompt: "Edit the file",
		Files: map[string]string{"d.txt": "a b a\n"},
		Turns: [][][]any{
			contentTurn(dsmlCall("edit", map[string]string{"path": "d.txt", "old_text": "a", "new_text": "x"}), "tool_calls"),
			contentTurn("edited\n", "stop"),
		},
	},
	{
		ID: "18-memory-append", Prompt: "Remember this",
		Turns: [][][]any{
			contentTurn(dsmlCall("memory_append", map[string]string{"section": "decisions", "text": "use boring code"}), "tool_calls"),
			contentTurn("recorded\n", "stop"),
		},
	},
	{
		ID: "19-verify-hook", Prompt: "Write the file",
		Hooks: []string{"python", "-c", "print('verify-ok')"},
		Turns: [][][]any{
			contentTurn(dsmlCall("write", map[string]string{"path": "hello.txt", "content": "hi"}), "tool_calls"),
			contentTurn("Wrote hello.txt.\n", "stop"),
		},
	},
	{
		ID: "20-spawn-agent", Prompt: "Run spawn",
		Turns: [][][]any{
			toolCallTurn(messages.ToolCall{ID: "sp1", Name: "spawn_agent", Arguments: `{"task": "nested task"}`}),
			contentTurn("nested answer\n", "stop"),
			contentTurn("parent final\n", "stop"),
		},
	},
	{
		ID: "21-spawn-parallel", Prompt: "Parallel spawn",
		Turns: [][][]any{
			// Nested scripts answer identically: the fake gateway pops
			// scripts by stream arrival order, so WHICH task lands on
			// WHICH answer is racy on both sides — the parallel structure
			// is what's under test, not per-task fidelity.
			toolCallTurn(messages.ToolCall{ID: "p1", Name: "spawn_parallel_task", Arguments: `{"tasks": [{"task": "task one"}, {"task": "task two"}]}`}),
			contentTurn("nested done\n", "stop"),
			contentTurn("nested done\n", "stop"),
			contentTurn("parent done\n", "stop"),
		},
	},
	{
		ID: "22-multi-turn-tools", Prompt: "Write then read",
		Files: map[string]string{"a.txt": "alpha\n"},
		Turns: [][][]any{
			contentTurn(dsmlCall("write", map[string]string{"path": "out.txt", "content": "x"}), "tool_calls"),
			toolCallTurn(messages.ToolCall{ID: "r1", Name: "read", Arguments: `{"path": "a.txt"}`}),
			contentTurn("finally\n", "stop"),
		},
	},
}

// -- normalization (identical on both sides) -------------------------------------

var (
	sidRe = regexp.MustCompile(`\d{8}-\d{6}-\d{6}`)
	tokRe = regexp.MustCompile(`"(input|output)_tokens": \d+`)
)

// scrub walks the parsed result, scrubbing environmental values from every
// string: session ids, token counts, and the run's scratch directory
// (paths/dates live only in elided system content). Walking the tree (not
// the serialized text) matters: JSON-escaped quotes inside content strings
// defeat text-level regexes.
func scrub(v any, scratch string) any {
	switch x := v.(type) {
	case string:
		// Content strings that are themselves JSON (spawn summaries, tool
		// results) are parsed and scrubbed semantically — key order and
		// int/float spelling must not decide parity.
		var parsed any
		if json.Unmarshal([]byte(x), &parsed) == nil {
			switch parsed.(type) {
			case map[string]any, []any:
				// spawn summaries and parallel-task arrays are JSON too —
				// compare them semantically, not as ordered-key strings.
				return scrub(parsed, scratch)
			}
		}
		x = sidRe.ReplaceAllString(x, "<sid>")
		x = tokRe.ReplaceAllString(x, `"${1}_tokens": <n>`)
		if scratch != "" {
			x = strings.ReplaceAll(x, scratch, "<scratch>")
		}
		return x
	case map[string]any:
		for k, val := range x {
			// Token counts are environmental (system-prompt length); the
			// regex only catches the string form, so numbers get scrubbed
			// here too.
			if k == "input_tokens" || k == "output_tokens" {
				x[k] = "<n>"
				continue
			}
			x[k] = scrub(val, scratch)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = scrub(val, scratch)
		}
		return x
	}
	return v
}

// normalizeWire reduces a stream call's wire messages to the comparable
// shape: system content elided (environmental), tool calls compacted.
func normalizeWire(msgs []any) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		entry := map[string]any{"role": "?"}
		switch mm := m.(type) {
		case messages.WireSystem:
			entry["role"] = "system" // content elided
		case messages.WireUser:
			entry["role"] = "user"
			entry["content"] = mm.Content
		case messages.WireAssistant:
			entry["role"] = "assistant"
			if mm.Content != "" {
				entry["content"] = mm.Content
			}
			if mm.Reasoning != "" {
				entry["reasoning_content"] = mm.Reasoning
			}
			if len(mm.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(mm.ToolCalls))
				for _, c := range mm.ToolCalls {
					calls = append(calls, map[string]any{"id": c.ID, "name": c.Function.Name, "arguments": c.Function.Arguments})
				}
				entry["tool_calls"] = calls
			}
		case messages.WireToolResult:
			entry["role"] = "tool"
			entry["tool_call_id"] = mm.ToolCallID
			entry["content"] = mm.Content
		}
		out = append(out, entry)
	}
	return out
}

func normalizeSession(sid string) []map[string]any {
	var out []map[string]any
	for _, rec := range sessions.ReadEvents(sid) {
		data, _ := rec["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}
		out = append(out, map[string]any{"type": rec["type"], "data": data})
	}
	return out
}

// -- fake gateway (Go side) --------------------------------------------------------

type fakeGateway struct {
	mu      sync.Mutex
	scripts [][][]any // each turn's events, encoded like the corpus
	calls   [][]any
}

func newFakeGateway(turns [][][]any) *fakeGateway {
	return &fakeGateway{scripts: turns}
}

func (f *fakeGateway) Stream(ctx context.Context, msgs []any, toolsList []any, maxTokens int) <-chan gateway.StreamEvent {
	f.mu.Lock()
	script := f.scripts[0]
	f.scripts = f.scripts[1:]
	f.calls = append(f.calls, msgs)
	f.mu.Unlock()
	ch := make(chan gateway.StreamEvent, len(script)+1)
	go func() {
		defer close(ch)
		for _, ev := range script {
			kind, _ := ev[0].(string)
			switch kind {
			case "content":
				text, _ := ev[1].(string)
				ch <- gateway.StreamEvent{Kind: gateway.EventContent, Text: text}
			case "reasoning":
				text, _ := ev[1].(string)
				ch <- gateway.StreamEvent{Kind: gateway.EventReasoning, Text: text}
			case "tool_call":
				payload, _ := ev[1].(map[string]any)
				id, _ := payload["id"].(string)
				name, _ := payload["name"].(string)
				arguments, _ := payload["arguments"].(string)
				ch <- gateway.StreamEvent{Kind: gateway.EventToolCall, ToolCall: messages.ToolCall{ID: id, Name: name, Arguments: arguments}}
			case "done":
				reason, _ := ev[1].(string)
				ch <- gateway.StreamEvent{Kind: gateway.EventDone, FinishReason: &reason}
			}
		}
	}()
	return ch
}

func (f *fakeGateway) ModelID() string { return "deepseek-v4-flash" }

// -- Go driver ----------------------------------------------------------------------

func runCaseGo(t *testing.T, c corpusCase, scratch, sid string) map[string]any {
	t.Helper()
	for rel, content := range c.Files {
		p := filepath.Join(scratch, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.Hooks) > 0 {
		hooksPath := filepath.Join(scratch, ".kaal", "hooks.json")
		if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{"verify": c.Hooks})
		if err := os.WriteFile(hooksPath, payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gw := newFakeGateway(c.Turns)
	mem := memory.NewMemory(filepath.Join(scratch, ".agent-memory"))
	reg := tools.NewRegistry(scratch, false, nil, mem)
	maxSteps := c.MaxSteps
	if maxSteps == 0 {
		maxSteps = 20
	}
	l := loop.NewAgentLoop(gw, reg, mem, sid,
		loop.WithEnableVerify(len(c.Hooks) > 0),
		loop.WithAskHandler(func(question string, options []string) string { return "yes" }),
		loop.WithMaxSteps(maxSteps),
	)
	answer, err := l.Run(c.Prompt, nil)
	var calls [][]map[string]any
	for _, c := range gw.calls {
		calls = append(calls, normalizeWire(c))
	}
	result := map[string]any{
		"answer":  nil,
		"error":   nil,
		"calls":   calls,
		"session": normalizeSession(sid),
		"scratch": scratch,
	}
	if err != nil {
		result["error"] = err.Error()
	} else {
		result["answer"] = answer
	}
	return result
}

// -- Python driver --------------------------------------------------------------------

const pythonDriver = `import json, os, re, sys, tempfile
from pathlib import Path

sys.path.insert(0, os.environ["KAAL_REPO_ROOT"])
from harness.loop import AgentLoop
from harness.memory import Memory
from harness.messages import ToolCall
from harness.tools import ToolRegistry
from harness import sessions

class FakeGateway:
    def __init__(self, scripts):
        self.scripts = scripts
        self.calls = []
        self.model_id = "deepseek-v4-flash"
    def stream(self, messages, tools):
        self.calls.append(json.loads(json.dumps(messages)))
        script = self.scripts.pop(0)
        for event in script:
            yield event

def build_scripts(case):
    scripts = []
    for turn in case["turns"]:
        s = []
        for ev in turn:
            kind = ev[0]
            if kind == "content":
                s.append(("content", ev[1]))
            elif kind == "reasoning":
                s.append(("reasoning", ev[1]))
            elif kind == "tool_call":
                tc = ev[1]
                s.append(("tool_call", ToolCall(tc["id"], tc["name"], tc["arguments"])))
            elif kind == "done":
                s.append(("done", ev[1]))
        scripts.append(s)
    return scripts

def normalize_wire(messages):
    out = []
    for m in messages:
        role = m.get("role")
        entry = {"role": role}
        if role == "system":
            out.append(entry)
            continue
        if m.get("content"):
            entry["content"] = m["content"]
        if m.get("reasoning_content"):
            entry["reasoning_content"] = m["reasoning_content"]
        calls = m.get("tool_calls") or []
        if calls:
            entry["tool_calls"] = [
                {"id": c.get("id", ""), "name": c.get("function", {}).get("name", c.get("name", "")),
                 "arguments": c.get("function", {}).get("arguments", c.get("arguments", ""))}
                for c in calls
            ]
        if role == "tool":
            entry["tool_call_id"] = m.get("tool_call_id", "")
        out.append(entry)
    return out

def run_case(case, scratch, sid):
    for rel, content in (case.get("files") or {}).items():
        p = Path(scratch) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content, encoding="utf-8")
    hooks = case.get("hooks")
    if hooks:
        hp = Path(scratch) / ".kaal" / "hooks.json"
        hp.parent.mkdir(parents=True, exist_ok=True)
        hp.write_text(json.dumps({"verify": hooks}), encoding="utf-8")
    gw = FakeGateway(build_scripts(case))
    mem = Memory(Path(scratch) / ".agent-memory")
    tools = ToolRegistry(memory=mem, project_dir=scratch)
    loop = AgentLoop(
        gw, tools, mem, sid,
        max_steps=case.get("max_steps") or 20,
        enable_verify=bool(hooks),
        ask_handler=lambda question, options=None: "yes",
    )
    try:
        answer = loop.run(case["prompt"])
        error = None
    except Exception as exc:
        answer = None
        error = str(exc)
    calls = [normalize_wire(c) for c in gw.calls]
    session = []
    for rec in sessions.read_events(sid):
        session.append({"type": rec.get("type"), "data": rec.get("data", {})})
    return {"answer": answer, "error": error, "calls": calls, "session": session, "scratch": scratch}

def main():
    corpus_path, out_path = sys.argv[1], sys.argv[2]
    corpus = json.loads(Path(corpus_path).read_text(encoding="utf-8"))
    results = []
    for case in corpus:
        scratch = tempfile.mkdtemp(prefix="parity-py-")
        results.append(run_case(case, scratch, "parity-" + case["id"] + "-py"))
    Path(out_path).write_text(json.dumps(results, ensure_ascii=False), encoding="utf-8")

main()
`

// -- the gate --------------------------------------------------------------------------

func repoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func TestParityGate(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("repo root not found")
	}
	if _, err := os.Stat(filepath.Join(root, "harness", "loop.py")); err != nil {
		t.Skip("python harness/ absent — the parity gate is a pre-burn instrument")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH — the python side of the gate needs it")
	}

	// Run the Python side. The sessions dir is shared by both sides and
	// MUST be isolated — the Go driver reads the same store.
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	t.Setenv("KAAL_SESSIONS_DIR", sessionsDir)
	corpusPath := filepath.Join(tmp, "corpus.json")
	outPath := filepath.Join(tmp, "results.json")
	driverPath := filepath.Join(tmp, "driver.py")
	corpusBytes, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corpusPath, corpusBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driverPath, []byte(pythonDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "python", driverPath, corpusPath, outPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"KAAL_REPO_ROOT="+root,
		"KAAL_SESSIONS_DIR="+sessionsDir,
		"OPENCODE_API_KEY=sk-test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python driver failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var pyResults []map[string]any
	if err := json.Unmarshal(raw, &pyResults); err != nil {
		t.Fatal(err)
	}
	if len(pyResults) != len(corpus) {
		t.Fatalf("python results: %d, corpus: %d", len(pyResults), len(corpus))
	}

	// Run the Go side and diff case by case.
	failures := 0
	for i, c := range corpus {
		scratch := t.TempDir()
		goResult := runCaseGo(t, c, scratch, "parity-"+c.ID+"-go")
		pyResult := pyResults[i]

		goScratch, _ := goResult["scratch"].(string)
		pyScratch, _ := pyResult["scratch"].(string)
		// toGeneric FIRST: scrub descends only generic JSON types, and the
		// Go side arrives with concrete types ([][]map[string]any etc.);
		// the Python side is already generic from json.Unmarshal.
		goNorm := scrub(toGeneric(goResult), goScratch)
		pyNorm := scrub(toGeneric(pyResult), pyScratch)
		delete(goNorm.(map[string]any), "scratch")
		delete(pyNorm.(map[string]any), "scratch")
		equal := reflect.DeepEqual(goNorm, pyNorm)
		if !equal {
			// Parallel nested spawns stream in a racy order on BOTH sides;
			// the wire-call sequence is exact except the racy middle, which
			// is compared as a multiset.
			equal = resultsEquivalent(goNorm, pyNorm)
		}
		if !equal {
			failures++
			_ = os.WriteFile(filepath.Join(os.TempDir(), "parity-"+c.ID+"-py.json"), []byte(pretty(pyNorm)), 0o644)
			_ = os.WriteFile(filepath.Join(os.TempDir(), "parity-"+c.ID+"-go.json"), []byte(pretty(goNorm)), 0o644)
			t.Errorf("case %s: PARITY VIOLATION\n  first diff: %s", c.ID, firstDiff(pyNorm, goNorm))
		} else {
			t.Logf("case %s: parity ok", c.ID)
		}
	}
	if failures > 0 {
		t.Fatalf("parity gate: %d of %d cases diverged", failures, len(corpus))
	}
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "  ", "  ")
	return string(b)
}

// resultsEquivalent compares two results, treating the wire-calls list with
// the racy-middle rule: the longest common prefix and suffix must match
// exactly; the remaining middle is compared as a multiset (parallel nested
// spawns stream in a nondeterministic order on both sides).
func resultsEquivalent(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		return false
	}
	ac, aok := am["calls"].([]any)
	bc, bok := bm["calls"].([]any)
	if !aok || !bok {
		return false
	}
	restA := map[string]any{}
	restB := map[string]any{}
	for k, v := range am {
		if k != "calls" {
			restA[k] = v
		}
	}
	for k, v := range bm {
		if k != "calls" {
			restB[k] = v
		}
	}
	if !reflect.DeepEqual(restA, restB) {
		if os.Getenv("KAAL_PARITY_DEBUG") != "" {
			fmt.Printf("DEBUG rest mismatch:\npython: %s\ngo: %s\n", pretty(restA), pretty(restB))
		}
		return false
	}
	i := 0
	for i < len(ac) && i < len(bc) && reflect.DeepEqual(ac[i], bc[i]) {
		i++
	}
	j, k := len(ac), len(bc)
	for j > i && k > i && reflect.DeepEqual(ac[j-1], bc[k-1]) {
		j--
		k--
	}
	if j != k {
		return false
	}
	midA := map[string]bool{}
	for _, call := range ac[i:j] {
		b, _ := json.Marshal(call)
		midA[string(b)] = true
	}
	midB := map[string]bool{}
	for _, call := range bc[i:k] {
		b, _ := json.Marshal(call)
		midB[string(b)] = true
	}
	if os.Getenv("KAAL_PARITY_DEBUG") != "" && !reflect.DeepEqual(midA, midB) {
		fmt.Printf("DEBUG midA=%v\nmidB=%v\n", midA, midB)
	}
	return reflect.DeepEqual(midA, midB)
}

// toGeneric re-marshals a result so both sides compare as plain JSON types.
func toGeneric(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// firstDiff reports the first path where two normalized results differ.
func firstDiff(a, b any) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("$: map vs %T", b)
		}
		for k := range x {
			if _, ok := y[k]; !ok {
				return "$." + k + ": only in python"
			}
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				return "$." + k + ": only in go"
			}
		}
		for k := range x {
			if d := firstDiff(x[k], y[k]); d != "" {
				return "$." + k + d
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok {
			return fmt.Sprintf("$: list vs %T", b)
		}
		if len(x) != len(y) {
			return fmt.Sprintf("$: len %d vs %d", len(x), len(y))
		}
		for i := range x {
			if d := firstDiff(x[i], y[i]); d != "" {
				return fmt.Sprintf("$[%d]%s", i, d)
			}
		}
	default:
		if a != b {
			return fmt.Sprintf("$: %v vs %v", a, b)
		}
	}
	return ""
}
