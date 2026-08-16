// Ported from tests/test_sessions.py (230 lines): ids, read_events,
// delete_session, prune_sessions.
package sessions_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/sessions"
)

// setup points KAAL_SESSIONS_DIR at a private temp dir per test.
func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KAAL_SESSIONS_DIR", dir)
	return dir
}

func TestIDsAreUniqueToTheMicrosecond(t *testing.T) {
	setup(t)
	first := sessions.NewSessionID()
	second := sessions.NewSessionID()
	if first == second {
		t.Fatal("ids must differ")
	}
	ids := map[string]bool{}
	for i := 0; i < 100; i++ {
		ids[sessions.NewSessionID()] = true
	}
	if len(ids) != 100 {
		t.Fatalf("collision in 100-id burst: %d unique", len(ids))
	}
}

func TestReadEventsRoundTrip(t *testing.T) {
	setup(t)
	sid := sessions.NewSessionID()
	_ = sessions.AppendEvent(sid, map[string]any{"type": "user", "data": map[string]any{"content": "hello"}})
	_ = sessions.AppendEvent(sid, map[string]any{"type": "assistant", "data": map[string]any{"content": "hi"}})
	_ = sessions.AppendEvent(sid, map[string]any{"type": "tool_result", "data": map[string]any{"tool_call_id": "call_1", "content": "ok"}})
	events := sessions.ReadEvents(sid)
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	for i, want := range []string{"user", "assistant", "tool_result"} {
		if events[i]["type"] != want {
			t.Fatalf("type %d: %v", i, events[i]["type"])
		}
	}
	if ts, _ := events[0]["ts"].(string); ts == "" {
		t.Fatal("ts missing")
	}
	if data := events[0]["data"].(map[string]any); data["content"] != "hello" {
		t.Fatalf("data: %v", data)
	}
}

func TestReadEventsMissingSessionYieldsEmpty(t *testing.T) {
	setup(t)
	if events := sessions.ReadEvents("20260802-000000-000000"); len(events) != 0 {
		t.Fatalf("want empty, got %v", events)
	}
}

func TestReadEventsSkipsBlankAndCorruptLines(t *testing.T) {
	dir := setup(t)
	sid := "20260802-000000-000000"
	path := filepath.Join(dir, sid+".jsonl")
	_ = os.WriteFile(path, []byte(
		"{\"ts\": \"t1\", \"type\": \"user\", \"data\": {\"content\": \"ok\"}}\n"+
			"\n"+
			"THIS IS NOT JSON\n"+
			"{\"ts\": \"t2\", \"type\": \"assistant\", \"data\": {\"content\": \"hi\"}}\n",
	), 0o644)
	events := sessions.ReadEvents(sid)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[0]["data"].(map[string]any)["content"] != "ok" || events[1]["data"].(map[string]any)["content"] != "hi" {
		t.Fatalf("events: %v", events)
	}
}

func TestBulkRoundTripInOrder(t *testing.T) {
	setup(t)
	sid := sessions.NewSessionID()
	_ = sessions.AppendEvents(sid, []map[string]any{
		{"type": "user", "data": map[string]any{"content": "hello"}},
		{"type": "assistant", "data": map[string]any{"content": "hi"}},
		{"type": "tool_call", "data": map[string]any{"id": "c1", "name": "read", "arguments": "{}"}},
		{"type": "tool_result", "data": map[string]any{"tool_call_id": "c1", "content": "ok"}},
	})
	events := sessions.ReadEvents(sid)
	wantTypes := []string{"user", "assistant", "tool_call", "tool_result"}
	if len(events) != 4 {
		t.Fatalf("want 4, got %d", len(events))
	}
	for i, want := range wantTypes {
		if events[i]["type"] != want {
			t.Fatalf("type %d: %v", i, events[i]["type"])
		}
	}
	if events[2]["data"].(map[string]any)["name"] != "read" {
		t.Fatalf("tool_call data: %v", events[2]["data"])
	}
	for _, e := range events {
		if ts, _ := e["ts"].(string); ts == "" {
			t.Fatal("ts missing")
		}
	}
}

func TestInvalidTypeRaisesAndWritesNothing(t *testing.T) {
	dir := setup(t)
	sid := sessions.NewSessionID()
	err := sessions.AppendEvents(sid, []map[string]any{
		{"type": "user", "data": map[string]any{"content": "ok"}},
		{"type": "bogus", "data": map[string]any{}},
	})
	if err == nil {
		t.Fatal("want error for invalid type")
	}
	// All events are validated before anything touches the store.
	if events := sessions.ReadEvents(sid); len(events) != 0 {
		t.Fatalf("store must be untouched: %v", events)
	}
	if _, err := os.Stat(filepath.Join(dir, sid+".jsonl")); err == nil {
		t.Fatal("file must not exist")
	}
}

func TestEmptyBatchIsNoop(t *testing.T) {
	setup(t)
	sid := sessions.NewSessionID()
	if err := sessions.AppendEvents(sid, nil); err != nil {
		t.Fatal(err)
	}
	if events := sessions.ReadEvents(sid); len(events) != 0 {
		t.Fatalf("want empty, got %v", events)
	}
}

func TestDeleteReturnsTrueAndRemovesFile(t *testing.T) {
	dir := setup(t)
	sid := "20260802-000000-000000"
	_ = sessions.AppendEvent(sid, map[string]any{"type": "user", "data": map[string]any{"content": "x"}})
	path := filepath.Join(dir, sid+".jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("file missing")
	}
	if !sessions.DeleteSession(sid) {
		t.Fatal("delete must return true")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("file still exists")
	}
}

func TestDeleteMissingReturnsFalse(t *testing.T) {
	setup(t)
	if sessions.DeleteSession("20260802-000000-000000") {
		t.Fatal("delete missing must return false")
	}
}

func TestDeleteLeavesOtherFilesUntouched(t *testing.T) {
	dir := setup(t)
	drop := "20260802-000000-000000"
	keep := "20260802-000000-000001"
	_ = sessions.AppendEvent(drop, map[string]any{"type": "user", "data": map[string]any{"content": "drop"}})
	_ = sessions.AppendEvent(keep, map[string]any{"type": "user", "data": map[string]any{"content": "keep"}})
	other := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(other, []byte("not a session file\n"), 0o644)
	if !sessions.DeleteSession(drop) {
		t.Fatal("delete failed")
	}
	if _, err := os.Stat(filepath.Join(dir, keep+".jsonl")); err != nil {
		t.Fatal("keep file deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, drop+".jsonl")); err == nil {
		t.Fatal("drop file still exists")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("non-session file deleted")
	}
}

func TestPruneKeepsNewestAndReturnsDeletedIDs(t *testing.T) {
	setup(t)
	ids := []string{"20260802-000000", "20260802-000001", "20260802-000002", "20260802-000003"}
	for _, sid := range ids {
		_ = sessions.AppendEvent(sid, map[string]any{"type": "user", "data": map[string]any{"content": sid}})
	}
	deleted := sessions.PruneSessions(2)
	if len(deleted) != 2 || deleted[0] != ids[0] || deleted[1] != ids[1] {
		t.Fatalf("deleted: %v", deleted)
	}
	remaining := sessions.ListSessions()
	if len(remaining) != 2 || remaining[0]["id"] != ids[2] || remaining[1]["id"] != ids[3] {
		t.Fatalf("remaining: %v", remaining)
	}
}

func TestPruneZeroDeletesEverything(t *testing.T) {
	setup(t)
	for i := 0; i < 3; i++ {
		_ = sessions.AppendEvent("20260802-00000"+itoa(i), map[string]any{"type": "user", "data": map[string]any{"content": "x"}})
	}
	deleted := sessions.PruneSessions(0)
	if len(deleted) != 3 {
		t.Fatalf("deleted: %v", deleted)
	}
	if list := sessions.ListSessions(); len(list) != 0 {
		t.Fatalf("list: %v", list)
	}
}

func TestPruneNegativeKeepDeletesEverything(t *testing.T) {
	setup(t)
	_ = sessions.AppendEvent("20260802-000000", map[string]any{"type": "user", "data": map[string]any{"content": "x"}})
	deleted := sessions.PruneSessions(-1)
	if len(deleted) != 1 || deleted[0] != "20260802-000000" {
		t.Fatalf("deleted: %v", deleted)
	}
}

func TestPruneKeepBeyondCountDeletesNothing(t *testing.T) {
	setup(t)
	for i := 0; i < 2; i++ {
		_ = sessions.AppendEvent("20260802-00000"+itoa(i), map[string]any{"type": "user", "data": map[string]any{"content": "x"}})
	}
	if deleted := sessions.PruneSessions(10); len(deleted) != 0 {
		t.Fatalf("deleted: %v", deleted)
	}
	if list := sessions.ListSessions(); len(list) != 2 {
		t.Fatalf("list: %v", list)
	}
}

func TestPruneEmptyStore(t *testing.T) {
	setup(t)
	if deleted := sessions.PruneSessions(20); len(deleted) != 0 {
		t.Fatalf("deleted: %v", deleted)
	}
}

func TestLoadMessagesAndListSessionsRoundTrip(t *testing.T) {
	setup(t)
	sid := "20260802-000000-000042"
	_ = sessions.AppendEvent(sid, map[string]any{"type": "user", "data": map[string]any{"content": "hello"}})
	_ = sessions.AppendEvent(sid, map[string]any{"type": "assistant", "data": map[string]any{"content": "hi"}})
	_ = sessions.AppendEvent(sid, map[string]any{"type": "tool_result", "data": map[string]any{"tool_call_id": "c1", "content": "ok"}})
	msgs := sessions.LoadMessages(sid)
	if len(msgs) != 3 {
		t.Fatalf("msgs: %v", msgs)
	}
	if msgs[0]["role"] != "user" || msgs[0]["content"] != "hello" {
		t.Fatalf("msg0: %v", msgs[0])
	}
	if msgs[1]["role"] != "assistant" || msgs[1]["content"] != "hi" {
		t.Fatalf("msg1: %v", msgs[1])
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "c1" {
		t.Fatalf("msg2: %v", msgs[2])
	}
	list := sessions.ListSessions()
	if len(list) != 1 || list[0]["id"] != sid {
		t.Fatalf("list: %v", list)
	}
	if ts, _ := list[0]["ts"].(string); ts == "" {
		t.Fatal("ts missing")
	}
	if prompt, _ := list[0]["prompt"].(string); prompt != "hello" {
		t.Fatalf("prompt: %v", list[0]["prompt"])
	}
}

func TestLoadMessagesSkipsCorruptViaSharedReader(t *testing.T) {
	dir := setup(t)
	sid := "20260802-000000-000043"
	_ = os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(
		"{\"ts\": \"t\", \"type\": \"user\", \"data\": {\"content\": \"ok\"}}\n"+
			"NOT JSON\n",
	), 0o644)
	msgs := sessions.LoadMessages(sid)
	if len(msgs) != 1 || msgs[0]["content"] != "ok" {
		t.Fatalf("msgs: %v", msgs)
	}
}

func TestAssistantWireWithToolCallsAndReasoning(t *testing.T) {
	setup(t)
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
	assistant := msgs[1]
	if assistant["role"] != "assistant" || assistant["content"] != "hi" {
		t.Fatalf("assistant: %v", assistant)
	}
	if assistant["reasoning_content"] != "thinking step" {
		t.Fatalf("reasoning: %v", assistant["reasoning_content"])
	}
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls: %v", assistant["tool_calls"])
	}
	if calls[0].(map[string]any)["function"].(map[string]any)["name"] != "memory_append" {
		t.Fatalf("call: %v", calls[0])
	}
}

func TestStoreDirOverride(t *testing.T) {
	dir := setup(t)
	if got := sessions.StoreDir(); got != dir {
		t.Fatalf("StoreDir: %q", got)
	}
	if list := sessions.ListSessions(); len(list) != 0 {
		t.Fatalf("list: %v", list)
	}
}

func TestInvalidTypeRejects(t *testing.T) {
	setup(t)
	err := sessions.AppendEvent(sessions.NewSessionID(), map[string]any{"type": "bogus", "data": map[string]any{}})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("message must name the invalid type: %v", err)
	}
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

func TestAsyncWriterAppendsAndFlushes(t *testing.T) {
	setup(t)
	w := sessions.NewAsyncWriter()
	defer w.Close()
	events := []map[string]any{
		{"type": "user", "data": map[string]any{"content": "hello"}},
		{"type": "assistant", "data": map[string]any{"content": "hi"}},
		{"type": "tool_result", "data": map[string]any{"tool_call_id": "c1", "content": "ok"}},
	}
	for range 3 {
		if err := w.AppendEvents("20260802-000000-000100", events); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	got := sessions.ReadEvents("20260802-000000-000100")
	if len(got) != 9 {
		t.Fatalf("want 9 events after flush, got %d", len(got))
	}
	// Flush is idempotent and safe to call again.
	w.Flush()
}

func TestAsyncWriterValidatesSynchronously(t *testing.T) {
	setup(t)
	w := sessions.NewAsyncWriter()
	defer w.Close()
	err := w.AppendEvents("20260802-000000-000101", []map[string]any{
		{"type": "user", "data": map[string]any{"content": "ok"}},
		{"type": "bogus", "data": map[string]any{}},
	})
	if err == nil {
		t.Fatal("invalid type must error at enqueue time")
	}
	w.Flush()
	if got := sessions.ReadEvents("20260802-000000-000101"); len(got) != 0 {
		t.Fatalf("invalid batch must write nothing: %v", got)
	}
}
