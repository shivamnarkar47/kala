// Package context ports harness/context.py: token estimation and
// context-budget bookkeeping (the truncation ledger).
package context

import (
	"unicode/utf8"

	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

// EstimateTokens is the rough token estimate: one token per three
// characters (runes, as Python's len() counts).
func EstimateTokens(text string) int {
	return utf8.RuneCountInString(text) / 3
}

// WireTokenCount counts tokens for already-converted wire structs (same
// formula as the per-message estimator).
func WireTokenCount(wire []any) int {
	return messages.WireTokenCost(wire)
}

// MessageTokenCosts returns the per-message wire token cost, computed once
// (the truncation ledger): estimate of each message's Python-style JSON wire
// form.
func MessageTokenCosts(msgs []messages.Message) []int {
	costs := make([]int, 0, len(msgs))
	for _, m := range msgs {
		b, err := jsonpy.Marshal(m.ToWire())
		if err != nil {
			costs = append(costs, 0)
			continue
		}
		costs = append(costs, jsonpy.RuneCount(b)/3)
	}
	return costs
}

// TruncateHistory drops oldest user+assistant+tool-result triples until the
// history fits. Never drops the system message, the last user message, or
// the turn that follows it (its assistant reply and tool results). O(n)
// total: per-message token costs are computed once into a ledger, and each
// dropped turn subtracts its ledger slice.
func TruncateHistory(msgs []messages.Message, system messages.SystemMessage, maxPromptTokens int) []messages.Message {
	// Keep exactly one system message, at the front. The system stays
	// EXCLUDED from the token count.
	result := make([]messages.Message, 0, len(msgs))
	for _, m := range msgs {
		if _, ok := m.(messages.SystemMessage); !ok {
			result = append(result, m)
		}
	}
	ledger := MessageTokenCosts(result)
	total := 0
	for _, c := range ledger {
		total += c
	}

	for total > maxPromptTokens {
		// Oldest droppable turn: the first UserMessage that is not the last
		// one. Recompute the last-user index each round (deletions shift it).
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
		dropped := 0
		for _, c := range ledger[start:end] {
			dropped += c
		}
		total -= dropped
		result = append(result[:start], result[end:]...)
		ledger = append(ledger[:start], ledger[end:]...)
	}

	return append([]messages.Message{system}, result...)
}
