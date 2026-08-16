// Ported from tests/test_context.py — token estimation and history
// truncation.
package context_test

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/context"
	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

func sampleHistory() []messages.Message {
	return []messages.Message{
		messages.SystemMessage{Text: "you are kaal"},
		messages.UserMessage{Text: "old question"},
		messages.AssistantMessage{Content: "old answer"},
		messages.ToolResultMessage{ToolCallID: "call_1", Content: "old result"},
		messages.UserMessage{Text: "new question"},
		messages.AssistantMessage{Content: "new answer"},
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := context.EstimateTokens(strings.Repeat("x", 300)); got != 100 {
		t.Fatalf("want 100, got %d", got)
	}
	if got := context.EstimateTokens(""); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

func TestTruncateKeepsSystemAndLastUser(t *testing.T) {
	history := sampleHistory()
	truncated := context.TruncateHistory(history, messages.SystemMessage{Text: "you are kaal"}, 10)
	system, ok := truncated[0].(messages.SystemMessage)
	if !ok || system.Text != "you are kaal" {
		t.Fatalf("system: %+v", truncated[0])
	}
	// oldest user dropped, its assistant dropped, its tool result dropped
	if truncated[1].(messages.UserMessage).Text != "new question" {
		t.Fatalf("oldest user not dropped: %+v", truncated[1])
	}
	// last user kept, its assistant kept
	if len(truncated) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(truncated), truncated)
	}
	if truncated[2].(messages.AssistantMessage).Content != "new answer" {
		t.Fatalf("last assistant: %+v", truncated[2])
	}
}

func TestTruncateNoopWhenFits(t *testing.T) {
	history := sampleHistory()
	truncated := context.TruncateHistory(history, messages.SystemMessage{Text: "you are kaal"}, 1_000_000_000)
	if len(truncated) != len(history) {
		t.Fatalf("want %d, got %d", len(history), len(truncated))
	}
	if _, ok := truncated[0].(messages.SystemMessage); !ok {
		t.Fatal("system must lead")
	}
}

func TestTruncateNeverLosesLastUser(t *testing.T) {
	history := sampleHistory()
	truncated := context.TruncateHistory(history, messages.SystemMessage{Text: "s"}, 0)
	if len(truncated) != 3 {
		t.Fatalf("want system + last turn, got %d: %+v", len(truncated), truncated)
	}
	if truncated[1].(messages.UserMessage).Text != "new question" {
		t.Fatalf("last user lost: %+v", truncated[1])
	}
}

// referenceTruncateHistory is the pre-ledger reference semantics
// (re-serializes per dropped turn), pinned to identical output.
func referenceTruncateHistory(msgs []messages.Message, system messages.SystemMessage, maxPromptTokens int) []messages.Message {
	result := make([]messages.Message, 0, len(msgs))
	for _, m := range msgs {
		if _, ok := m.(messages.SystemMessage); !ok {
			result = append(result, m)
		}
	}
	count := func(msgs []messages.Message) int {
		total := 0
		for _, m := range msgs {
			b, _ := jsonpy.Marshal(m.ToWire())
			total += jsonpy.RuneCount(b) / 3
		}
		return total
	}
	for count(result) > maxPromptTokens {
		lastUser := -1
		for i, m := range result {
			if _, ok := m.(messages.UserMessage); ok {
				lastUser = i
			}
		}
		if lastUser == -1 {
			break
		}
		start := -1
		for i, m := range result {
			if _, ok := m.(messages.UserMessage); ok && i != lastUser {
				start = i
				break
			}
		}
		if start == -1 {
			break
		}
		end := len(result)
		for i := start + 1; i < len(result); i++ {
			if _, ok := result[i].(messages.UserMessage); ok {
				end = i
				break
			}
		}
		result = append(result[:start], result[end:]...)
	}
	return append([]messages.Message{system}, result...)
}

func TestEquivalentToReferenceOnRandomHistories(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for round := 0; round < 80; round++ {
		msgs := []messages.Message{messages.SystemMessage{Text: "you are kaal"}}
		n := 3 + rng.Intn(10)
		for i := 0; i < n; i++ {
			msgs = append(msgs, messages.UserMessage{Text: strings.Repeat("u", 1+rng.Intn(4000))})
			if rng.Float64() < 0.8 {
				reasoning := strings.Repeat("r", rng.Intn(500))
				var reasoningPtr *string
				if reasoning != "" {
					reasoningPtr = &reasoning
				}
				msgs = append(msgs, messages.AssistantMessage{
					Content:          strings.Repeat("a", 1+rng.Intn(3000)),
					ReasoningContent: reasoningPtr,
				})
				if rng.Float64() < 0.8 {
					msgs = append(msgs, messages.ToolResultMessage{ToolCallID: "c", Content: strings.Repeat("t", 1+rng.Intn(3000))})
				}
			}
		}
		system := msgs[0].(messages.SystemMessage)
		budgets := []int{0, 1, 500, 5_000, 50_000, 616_000, 1_000_000_000}
		budget := budgets[rng.Intn(len(budgets))]
		newResult := context.TruncateHistory(msgs, system, budget)
		oldResult := referenceTruncateHistory(msgs, system, budget)
		if !sameMessages(newResult, oldResult) {
			t.Fatalf("round %d budget %d: ledger and reference diverged", round, budget)
		}
	}
}

func sameMessages(a, b []messages.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ab, err1 := jsonpy.Marshal(a[i].ToWire())
		bb, err2 := jsonpy.Marshal(b[i].ToWire())
		if err1 != nil || err2 != nil {
			return false
		}
		if string(ab) != string(bb) {
			return false
		}
	}
	return true
}

func TestMessageTokenCostsFormula(t *testing.T) {
	reasoning := "r"
	msgs := []messages.Message{
		messages.SystemMessage{Text: "sys"},
		messages.UserMessage{Text: "user text"},
		messages.AssistantMessage{Content: "assistant", ReasoningContent: &reasoning},
		messages.ToolResultMessage{ToolCallID: "c", Content: "tool"},
	}
	costs := context.MessageTokenCosts(msgs)
	for i, m := range msgs {
		b, _ := jsonpy.Marshal(m.ToWire())
		if got := jsonpy.RuneCount(b) / 3; got != costs[i] {
			t.Fatalf("cost %d: want %d, got %d", i, got, costs[i])
		}
	}
	// The ledger total equals the wire count of the same messages.
	wire := make([]any, 0, len(msgs))
	for _, m := range msgs {
		wire = append(wire, m.ToWire())
	}
	total := 0
	for _, c := range costs {
		total += c
	}
	if total != messages.WireTokenCost(wire) {
		t.Fatalf("ledger total %d != wire count %d", total, messages.WireTokenCost(wire))
	}
}

func TestWireTokenCountDelegates(t *testing.T) {
	wire := []any{
		messages.WireUser{Role: "user", Content: "hello"},
	}
	if context.WireTokenCount(wire) != messages.WireTokenCost(wire) {
		t.Fatal("WireTokenCount must delegate to WireTokenCost")
	}
	if context.WireTokenCount(wire) == 0 {
		t.Fatal("cost must be non-zero")
	}
}

var _ = json.Marshal // keep encoding/json imported if helpers change
