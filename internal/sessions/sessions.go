// Package sessions ports harness/sessions.py: the JSONL session store for
// interactive and resumed conversations.
package sessions

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var validTypes = map[string]bool{
	"user": true, "assistant": true, "tool_call": true,
	"tool_result": true, "error": true, "meta": true,
}

// StoreDir returns the session store directory: $KAAL_SESSIONS_DIR or the
// default XDG-ish path.
func StoreDir() string {
	if override := os.Getenv("KAAL_SESSIONS_DIR"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".local/share/kaal/sessions"
	}
	return filepath.Join(home, ".local", "share", "kaal", "sessions")
}

var (
	idMu      sync.Mutex
	lastMicro string
)

// NewSessionID returns a session id unique to the microsecond:
// %Y%m%d-%H%M%S-%f (Python's strftime has no period; Go's "000000" layout
// element only works after a '.', so the microseconds are appended by hand).
// Go's time.Now() is fast enough that two calls can land in the same
// microsecond tick (Python's datetime.now() call itself spans microseconds,
// hiding the race there) — so the id spins until the clock advances,
// keeping the format exact and the ids collision-free.
func NewSessionID() string {
	idMu.Lock()
	defer idMu.Unlock()
	for {
		now := time.Now()
		micro := now.Format("20060102-150405") + "-" + fmt.Sprintf("%06d", now.Nanosecond()/1000)
		if micro != lastMicro {
			lastMicro = micro
			return micro
		}
	}
}

func sessionPath(sessionID string) string {
	return filepath.Join(StoreDir(), sessionID+".jsonl")
}

// Event is one JSONL record: {"ts", "type", "data"}.
type Event struct {
	Ts   string         `json:"ts"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// ValidateEvents checks every event's type without writing (the shared
// synchronous gate for AppendEvents and the AsyncWriter).
func ValidateEvents(events []map[string]any) error {
	for _, event := range events {
		etype, _ := event["type"].(string)
		if !validTypes[etype] {
			return &InvalidTypeError{Type: etype}
		}
	}
	return nil
}

// AppendEvents appends N events as JSON lines in one open/write/close
// cycle. Every event is validated before the file is touched; an invalid
// type returns an error with nothing written. An empty list is a no-op.
func AppendEvents(sessionID string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	if err := ValidateEvents(events); err != nil {
		return err
	}
	records := make([]Event, 0, len(events))
	for _, event := range events {
		etype, _ := event["type"].(string)
		data, _ := event["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}
		records = append(records, Event{
			Ts:   time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00"),
			Type: etype,
			Data: data,
		})
	}
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(sessionPath(sessionID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, record := range records {
		if err := enc.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

// AsyncWriter is the P6 asynchronous session store: appends are enqueued to
// a single writer goroutine (serializing file writes across batch workers —
// no file-open contention) and flushed on turn end. Validation still happens
// synchronously at enqueue time, so an invalid type is never written.
type AsyncWriter struct {
	ch   chan appendRequest
	done chan struct{}
	wg   sync.WaitGroup // pending appends
}

type appendRequest struct {
	sessionID string
	events    []map[string]any
}

// NewAsyncWriter starts a writer goroutine (one per process: the CLI shares
// it across --batch workers).
func NewAsyncWriter() *AsyncWriter {
	w := &AsyncWriter{ch: make(chan appendRequest, 256), done: make(chan struct{})}
	go w.loop()
	return w
}

func (w *AsyncWriter) loop() {
	for {
		select {
		case req := <-w.ch:
			_ = AppendEvents(req.sessionID, req.events)
			w.wg.Done()
		case <-w.done:
			return
		}
	}
}

// AppendEvents validates synchronously and enqueues the append.
func (w *AsyncWriter) AppendEvents(sessionID string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	if err := ValidateEvents(events); err != nil {
		return err
	}
	w.wg.Add(1)
	select {
	case w.ch <- appendRequest{sessionID: sessionID, events: events}:
		return nil
	case <-w.done:
		w.wg.Done()
		return errors.New("session writer closed")
	}
}

// Flush blocks until every enqueued append is written (flush-on-turn-end).
func (w *AsyncWriter) Flush() { w.wg.Wait() }

// Close stops the writer goroutine. Call Flush first; pending appends are
// dropped otherwise.
func (w *AsyncWriter) Close() {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
}

// AppendEvent appends one event as a JSON line (thin wrapper).
func AppendEvent(sessionID string, event map[string]any) error {
	return AppendEvents(sessionID, []map[string]any{event})
}

// InvalidTypeError mirrors Python's ValueError for bad event types.
type InvalidTypeError struct{ Type string }

func (e *InvalidTypeError) Error() string {
	return "invalid event type: " + e.Type + "; allowed: [assistant error meta tool_call tool_result user]"
}

// readRecords yields the JSON records of a session file, one per line. The
// single tolerant JSONL reader: blank lines and corrupt (non-JSON) lines are
// skipped, and a file that cannot be opened yields nothing.
func readRecords(path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		out = append(out, record)
	}
	return out
}

// ReadEvents returns the raw JSONL records {"ts", "type", "data"} for a
// session, in file order. A missing session file yields [].
func ReadEvents(sessionID string) []map[string]any {
	path := sessionPath(sessionID)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return readRecords(path)
}

// eventToWire converts one persisted event to an OpenAI wire dict, or nil to
// skip it.
func eventToWire(record map[string]any) map[string]any {
	etype, _ := record["type"].(string)
	data, _ := record["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	switch etype {
	case "user":
		return map[string]any{"role": "user", "content": strOr(data["content"])}
	case "assistant":
		wire := map[string]any{"role": "assistant", "content": strOr(data["content"])}
		if reasoning, ok := data["reasoning_content"].(string); ok && reasoning != "" {
			wire["reasoning_content"] = reasoning
		}
		if calls, ok := data["tool_calls"].([]any); ok && len(calls) > 0 {
			wire["tool_calls"] = calls
		}
		return wire
	case "tool_result":
		return map[string]any{
			"role":         "tool",
			"tool_call_id": strOr(data["tool_call_id"]),
			"content":      strOr(data["content"]),
		}
	}
	return nil // tool_call, error, meta carry no replayable message
}

func strOr(v any) string {
	s, _ := v.(string)
	return s
}

// LoadMessages replays a session as OpenAI wire dicts in file order.
// tool_call/error/meta events are skipped (tool calls ride inside the
// assistant event). A missing session file yields [].
func LoadMessages(sessionID string) []map[string]any {
	var out []map[string]any
	for _, record := range ReadEvents(sessionID) {
		if wire := eventToWire(record); wire != nil {
			out = append(out, wire)
		}
	}
	return out
}

// ListSessions returns every `<id>.jsonl` sorted by id: {"id", "ts",
// "prompt"}. ts is the first event's timestamp (or nil), prompt the first
// user event's content (or nil).
func ListSessions() []map[string]any {
	store := StoreDir()
	entries, err := os.ReadDir(store)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	sort.Strings(ids)
	var out []map[string]any
	for _, id := range ids {
		var firstTS, firstPrompt any
		for _, record := range readRecords(filepath.Join(store, id+".jsonl")) {
			if firstTS == nil {
				if ts, ok := record["ts"].(string); ok && ts != "" {
					firstTS = ts
				}
			}
			if firstPrompt == nil && record["type"] == "user" {
				if data, ok := record["data"].(map[string]any); ok {
					if content, ok := data["content"].(string); ok && content != "" {
						firstPrompt = content
					}
				}
			}
			if firstTS != nil && firstPrompt != nil {
				break
			}
		}
		out = append(out, map[string]any{"id": id, "ts": firstTS, "prompt": firstPrompt})
	}
	return out
}

// DeleteSession deletes `<store>/<id>.jsonl`; true if the file existed.
// Only ever removes the session's own file.
func DeleteSession(sessionID string) bool {
	path := sessionPath(sessionID)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return false
	}
	return os.Remove(path) == nil
}

// PruneSessions deletes all session files except the newest `keep`, sorted
// by id. Returns the deleted session ids in deletion order. keep <= 0
// deletes every session file.
func PruneSessions(keep int) []string {
	store := StoreDir()
	entries, err := os.ReadDir(store)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			paths = append(paths, filepath.Join(store, e.Name()))
		}
	}
	sort.Strings(paths)
	limit := min(len(paths), max(0, len(paths)-keep))
	var deleted []string
	for _, p := range paths[:limit] {
		if err := os.Remove(p); err != nil {
			continue // vanished between listing and unlink; nothing to report
		}
		deleted = append(deleted, strings.TrimSuffix(filepath.Base(p), ".jsonl"))
	}
	return deleted
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
