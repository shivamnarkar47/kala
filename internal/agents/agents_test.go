// Ported from tests/test_agents.py (88 lines): persona definitions +
// persistence (.kaal/agents.json).
package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/agents"
)

func TestDefaultAgentsAreTheFivePandavas(t *testing.T) {
	if len(agents.DefaultAgents) != 5 {
		t.Fatalf("want 5 defaults, got %d", len(agents.DefaultAgents))
	}
	names := []string{}
	for _, a := range agents.DefaultAgents {
		names = append(names, a.Name)
	}
	want := []string{"Yudhishthira", "Bhima", "Arjuna", "Nakula", "Sahadeva"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names: %v", names)
		}
	}
	for _, a := range agents.DefaultAgents {
		if a.Description == "" {
			t.Fatalf("agent %s missing description", a.Name)
		}
	}
}

func TestLoadMissingFileSeedsDefaults(t *testing.T) {
	root := t.TempDir()
	data := agents.Load(root)
	if len(data.Agents) != 5 {
		t.Fatalf("agents: %d", len(data.Agents))
	}
	if data.Active != "" {
		t.Fatalf("active: %q", data.Active)
	}
	// load never writes
	if _, err := os.Stat(filepath.Join(root, ".kaal", "agents.json")); err == nil {
		t.Fatal("load must not create the file")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	data := agents.State{
		Agents: []agents.Agent{
			{Name: "Karna", Description: "the relentless executor"},
			{Name: "Arjuna", Description: "precise"},
		},
		Active: "Karna",
	}
	if err := agents.Save(root, data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".kaal", "agents.json")); err != nil {
		t.Fatal("file missing")
	}
	loaded := agents.Load(root)
	if len(loaded.Agents) != 2 || loaded.Agents[0].Name != "Karna" || loaded.Active != "Karna" {
		t.Fatalf("round trip: %+v", loaded)
	}
}

func TestLoadCorruptFileSeedsDefaults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".kaal", "agents.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("{ definitely not json !!!"), 0o644)
	data := agents.Load(root)
	if len(data.Agents) != 5 || data.Active != "" {
		t.Fatalf("corrupt load: %+v", data)
	}
}

func TestLoadMalformedShapeSeedsDefaults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".kaal", "agents.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(`{"agents": "nope", "active": 3}`), 0o644)
	data := agents.Load(root)
	if len(data.Agents) != 5 || data.Active != "" {
		t.Fatalf("malformed load: %+v", data)
	}
}

func TestActiveAgentResolvesByName(t *testing.T) {
	data := agents.State{Agents: agents.DefaultAgents, Active: "Arjuna"}
	if a := agents.ActiveAgent(data); a == nil || a.Name != "Arjuna" {
		t.Fatalf("active: %+v", a)
	}
	if a := agents.ActiveAgent(agents.State{Agents: agents.DefaultAgents, Active: "Bogus"}); a != nil {
		t.Fatalf("bad name must resolve nil: %+v", a)
	}
	if a := agents.ActiveAgent(agents.State{Agents: agents.DefaultAgents}); a != nil {
		t.Fatalf("no active must resolve nil: %+v", a)
	}
	if a := agents.ActiveAgent(agents.State{Active: "Arjuna"}); a != nil {
		t.Fatalf("empty agents must resolve nil: %+v", a)
	}
}

func TestAgentNames(t *testing.T) {
	data := agents.State{Agents: agents.DefaultAgents}
	names := agents.AgentNames(data)
	want := []string{"Yudhishthira", "Bhima", "Arjuna", "Nakula", "Sahadeva"}
	if len(names) != len(want) {
		t.Fatalf("names: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names: %v", names)
		}
	}
	if names := agents.AgentNames(agents.State{}); len(names) != 0 {
		t.Fatalf("empty names: %v", names)
	}
}

// Guard: descriptions mention the Mahabharata theme (voice parity).
func TestDescriptionsAreDistinctive(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range agents.DefaultAgents {
		if seen[a.Description] {
			t.Fatalf("duplicate description for %s", a.Name)
		}
		seen[a.Description] = true
	}
	if !strings.Contains(agents.DefaultAgents[0].Description, "Dharma") {
		t.Fatal("Yudhishthira's description must stay distinctive")
	}
}
