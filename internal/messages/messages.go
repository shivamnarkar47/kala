// Package messages is the wire message model for the OpenAI-compatible
// chat-completions API — a protocol-critical module: the
// reasoning_content-replay rule lives here. Assistant turns that made tool
// calls MUST re-send their reasoning_content verbatim; dropping it causes a
// 400 on the next turn with this gateway.
package messages

import (
	"strings"

	"github.com/kaal/kaal/internal/jsonpy"
)

// ToolCall is a function call as the OpenAI spec defines it: Arguments is a
// JSON string.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// WireFunction / WireToolCall are the wire shapes of a tool call. Field
// order mirrors the Python dict insertion order (byte-identity, P7 gate).
type WireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type WireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function WireFunction `json:"function"`
}

func (c ToolCall) ToWire() WireToolCall {
	return WireToolCall{ID: c.ID, Type: "function", Function: WireFunction{Name: c.Name, Arguments: c.Arguments}}
}

// Model message types.
type SystemMessage struct{ Text string }

type UserMessage struct{ Text string }

// AssistantMessage carries streamed reasoning. ReasoningContent is a
// *string — Go's zero value cannot distinguish an empty string from an
// absent field, and the replay rule depends on that distinction.
type AssistantMessage struct {
	Content          string
	ReasoningContent *string
	ToolCalls        []ToolCall
}

type ToolResultMessage struct {
	ToolCallID string
	Content    string
}

// Wire shapes per message kind. Field order mirrors the Python dict
// insertion order exactly (ToolResultMessage puts tool_call_id BEFORE
// content, as Python does), so marshaled bytes are identical.
type WireSystem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type WireUser struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type WireAssistant struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning_content,omitempty"`
	ToolCalls []WireToolCall `json:"tool_calls,omitempty"`
}

type WireToolResult struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// Message is any model message convertible to its wire form.
type Message interface {
	ToWire() any
}

func (m SystemMessage) ToWire() any { return WireSystem{Role: "system", Content: m.Text} }

func (m UserMessage) ToWire() any { return WireUser{Role: "user", Content: m.Text} }

// ToWire replays reasoning_content verbatim iff it was streamed (non-nil and
// non-empty); never synthesize a placeholder (V4 requires exact replay).
func (m AssistantMessage) ToWire() any {
	reasoning := ""
	if m.ReasoningContent != nil {
		reasoning = *m.ReasoningContent
	}
	calls := make([]WireToolCall, 0, len(m.ToolCalls))
	for _, c := range m.ToolCalls {
		calls = append(calls, c.ToWire())
	}
	return WireAssistant{Role: "assistant", Content: m.Content, Reasoning: reasoning, ToolCalls: calls}
}

func (m ToolResultMessage) ToWire() any {
	return WireToolResult{Role: "tool", ToolCallID: m.ToolCallID, Content: m.Content}
}

// ToWireMessages converts model messages to wire structs. Multiple system
// blocks are coalesced into one message ("\n\n"-joined) at the position of
// the first one before sending — safe for strict chat templates.
func ToWireMessages(msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	systemParts := []string{}
	firstSystem := -1
	for _, m := range msgs {
		if sm, ok := m.(SystemMessage); ok {
			if firstSystem == -1 {
				firstSystem = len(out)
			}
			systemParts = append(systemParts, sm.Text)
		} else {
			out = append(out, m.ToWire())
		}
	}
	if firstSystem != -1 {
		out = append(out, nil)
		copy(out[firstSystem+1:], out[firstSystem:])
		out[firstSystem] = WireSystem{Role: "system", Content: strings.Join(systemParts, "\n\n")}
	}
	return out
}

// WireTokenCost returns the total token cost of already-converted wire
// structs: one token per three characters of the Python-style JSON
// serialization (same formula as the context estimator).
func WireTokenCost(wire []any) int {
	total := 0
	for _, m := range wire {
		b, err := jsonpy.Marshal(m)
		if err != nil {
			continue
		}
		total += jsonpy.RuneCount(b) / 3
	}
	return total
}
