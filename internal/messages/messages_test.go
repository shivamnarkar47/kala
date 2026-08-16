// Ported from tests/test_messages.py (85 lines).
package messages_test

import (
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

func TestAssistantWireWithReasoningAndToolCalls(t *testing.T) {
	reasoning := "let me think"
	msg := messages.AssistantMessage{
		Content:          "",
		ReasoningContent: &reasoning,
		ToolCalls:        []messages.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"location": "San Francisco, CA"}`}},
	}
	wire := msg.ToWire()
	want := messages.WireAssistant{
		Role:      "assistant",
		Content:   "",
		Reasoning: "let me think",
		ToolCalls: []messages.WireToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: messages.WireFunction{Name: "get_weather", Arguments: `{"location": "San Francisco, CA"}`},
		}},
	}
	wantB, _ := jsonpy.Marshal(want)
	gotB, err := jsonpy.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotB) != string(wantB) {
		t.Fatalf("want %s, got %s", wantB, gotB)
	}
	b, err := jsonpy.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tool_choice") {
		t.Fatalf("banned field leaked: %s", b)
	}
}

func TestAssistantWireOmitsOptionalFields(t *testing.T) {
	wire := messages.AssistantMessage{Content: "hi"}.ToWire()
	b, err := jsonpy.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"reasoning_content", "tool_calls"} {
		if strings.Contains(string(b), absent) {
			t.Fatalf("%s must be absent, got %s", absent, b)
		}
	}
	// Zero-value safety: an empty reasoning string is omitted exactly as
	// Python's truthiness check omits it — no empty "reasoning_content": "".
	empty := ""
	wireEmpty := messages.AssistantMessage{Content: "hi", ReasoningContent: &empty}.ToWire()
	b2, _ := jsonpy.Marshal(wireEmpty)
	if strings.Contains(string(b2), "reasoning_content") {
		t.Fatalf("empty reasoning must be omitted, got %s", b2)
	}
}

func TestToolResultWire(t *testing.T) {
	wire := messages.ToolResultMessage{ToolCallID: "call_1", Content: "ok"}.ToWire()
	b, err := jsonpy.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	// tool_call_id must precede content, as Python's dict order does.
	want := `{"role": "tool", "tool_call_id": "call_1", "content": "ok"}`
	if string(b) != want {
		t.Fatalf("want %s, got %s", want, b)
	}
}

func TestSystemMessagesCoalesced(t *testing.T) {
	msgs := []messages.Message{
		messages.SystemMessage{Text: "first"},
		messages.UserMessage{Text: "q"},
		messages.SystemMessage{Text: "second"},
	}
	wire := messages.ToWireMessages(msgs)
	if len(wire) != 2 {
		t.Fatalf("want 2 wire messages, got %d", len(wire))
	}
	b, err := jsonpy.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role": "system", "content": "first\n\nsecond"}, {"role": "user", "content": "q"}]`
	if string(b) != want {
		t.Fatalf("want %s, got %s", want, b)
	}
}

func TestUserWire(t *testing.T) {
	b, err := jsonpy.Marshal(messages.UserMessage{Text: "hello"}.ToWire())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role": "user", "content": "hello"}` {
		t.Fatalf("got %s", b)
	}
}

func TestWireTokenCost(t *testing.T) {
	sample := func() []any {
		msgs := []messages.Message{
			messages.SystemMessage{Text: "you are kaal — the harness agent"},
			messages.UserMessage{Text: "read the file, then summarize it carefully"},
			messages.AssistantMessage{
				Content:          "Let me look.",
				ReasoningContent: strptr("first read, then summarize"),
				ToolCalls:        []messages.ToolCall{{ID: "c1", Name: "read", Arguments: `{"path": "a.txt"}`}},
			},
			messages.ToolResultMessage{ToolCallID: "c1", Content: "file contents here"},
		}
		return messages.ToWireMessages(msgs)
	}
	t.Run("matches sum of per-message costs", func(t *testing.T) {
		wire := sample()
		expected := 0
		for _, m := range wire {
			b, err := jsonpy.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			expected += jsonpy.RuneCount(b) / 3
		}
		if got := messages.WireTokenCost(wire); got != expected {
			t.Fatalf("want %d, got %d", expected, got)
		}
	})
	t.Run("empty wire costs zero", func(t *testing.T) {
		if got := messages.WireTokenCost([]any{}); got != 0 {
			t.Fatalf("want 0, got %d", got)
		}
	})
}

func strptr(s string) *string { return &s }
