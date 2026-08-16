// Package sessions ports harness/sessions.py: the JSONL session store for
// interactive and resumed conversations.
package sessions

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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

// NewSessionID returns a session id unique to the microsecond:
// %Y%m%d-%H%M%S-%f.
func NewSessionID() string {
	return time.Now().Format("20060102-150405-000000")
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

// AppendEvents appends N events as JSON lines in one open/write/close
// cycle. Every event is validated before the file is touched; an invalid
// type returns an error with nothing written. An empty list is a no-op.
func AppendEvents(sessionID string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	records := make([]Event, 0, len(events))
	for _, event := range events {
		etype, _ := event["type"].(string)
		if !validTypes[etype] {
			return &InvalidTypeError{Type: etype}
		}
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
