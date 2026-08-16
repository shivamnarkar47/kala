// Package agents ports harness/agents.py: the five Pandava personas —
// definitions + persistence (.kaal/agents.json).
package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Agent is one persona.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DefaultAgents are the five Pandava personas — the default cast.
var DefaultAgents = []Agent{
	{Name: "Yudhishthira", Description: "the Dharma Architect: principled, correctness-first architecture; deliberate planning, ethics of the codebase; never cuts corners"},
	{Name: "Bhima", Description: "the Mighty Performer: brute-force execution; heavy refactors, big sweeps, gets the job done with overwhelming force"},
	{Name: "Arjuna", Description: "the Precise Marksman: surgical precision; focused bug-hunting, minimal diffs, one clean shot at the target"},
	{Name: "Nakula", Description: "the Graceful Stylist: beauty and polish; UI/UX, naming, formatting, code that reads like poetry"},
	{Name: "Sahadeva", Description: "the Wise Strategist: the whole board; architecture strategy, tradeoffs, docs, sees moves ahead"},
}

// State is the persisted agents.json shape.
type State struct {
	Agents []Agent `json:"agents"`
	Active string  `json:"active"`
}

func seeded() State {
	agents := make([]Agent, len(DefaultAgents))
	copy(agents, DefaultAgents)
	return State{Agents: agents}
}

// valid reports whether data is a sane state dict (tolerant on shape; load
// never crashes).
func valid(data State) bool {
	if len(data.Agents) == 0 {
		return false
	}
	for _, a := range data.Agents {
		if a.Name == "" {
			return false
		}
	}
	return true
}

// Load reads agent state; missing/corrupt file seeds the defaults. Read-only:
// a missing file is NOT written on load.
func Load(root string) State {
	path := filepath.Join(root, ".kaal", "agents.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return seeded()
	}
	var data State
	if err := json.Unmarshal(raw, &data); err != nil {
		return seeded()
	}
	if !valid(data) {
		return seeded()
	}
	return data
}

// Save persists agent state atomically (temp + rename).
func Save(root string, data State) error {
	path := filepath.Join(root, ".kaal", "agents.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ActiveAgent resolves the active agent dict by name; nil for no/bad active
// name.
func ActiveAgent(data State) *Agent {
	if data.Active == "" {
		return nil
	}
	for i := range data.Agents {
		if data.Agents[i].Name == data.Active {
			return &data.Agents[i]
		}
	}
	return nil
}

// AgentNames returns the names of every agent in state, in list order.
func AgentNames(data State) []string {
	names := make([]string, 0, len(data.Agents))
	for _, a := range data.Agents {
		names = append(names, a.Name)
	}
	return names
}
