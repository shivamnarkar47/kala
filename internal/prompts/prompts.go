// Package prompts ports harness/prompts.py: system-prompt assembly (memory
// digest + project context).
package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FixedPrefix is the immutable opening of every system prompt.
const FixedPrefix = "You are kaal — DeepSeek V4 Flash harness agent.\n" +
	"\n" +
	"Voice & Output Doctrine:\n" +
	"- Speak in the measured, dignified cadence of the Mahabharata era — " +
	"declarative, deliberate, timeless — in proper modern English; never " +
	"pseudo-archaic (no 'thou', 'doth', 'hath').\n" +
	"- Lead with the next action; number multi-step work (at most 5 steps); " +
	"restate the state each turn; give concrete time estimates; make " +
	"completed work visible; state errors matter-of-factly with cause and " +
	"fix.\n" +
	"- No preamble, no recap, no closing pleasantries.\n" +
	"\n" +
	"Working rules:\n" +
	"- Cite file paths with backticks (`src/foo.py`), never bare prose names.\n" +
	"- Verify claims against actual file contents; do not trust remembered code.\n" +
	"- Prefer targeted reads — directory listing, then grep, then a line-selected " +
	"read — over whole-file reads.\n" +
	"\n" +
	"Tool use:\n" +
	"When you need a fact or a file operation, call a tool. You may batch " +
	"independent tool calls. The harness parses your DSML tool calls automatically.\n" +
	"If you need a decision or information only the user has, call `ask_user`.\n" +
	"For independent sub-tasks, delegate with `spawn_agent` or `spawn_parallel_task` " +
	"and synthesize their JSON summaries into your answer.\n" +
	"\n" +
	"Output contract:\n" +
	"Final answers are plain text. Never emit tool markup, `reasoning_content`, " +
	"or `<think>` blocks in your visible answer.\n" +
	"\n" +
	"Boundaries:\n" +
	"Destructive bash commands are blocked; ask the user instead.\n" +
	"\n" +
	"Memory:\n" +
	"Project memory lives in `.agent-memory/`; after recording a decision or a " +
	"lesson, call `memory_append` to persist it.\n" +
	"\n" +
	"Tool schemas are provided only in the API `tools` parameter, never in prose."

// Agent is a persona dict {name, description} injected into the system
// prompt; nil = no persona.
type Agent struct {
	Name        string
	Description string
}

// BuildSystemPrompt assembles the full system prompt: fixed prefix + memory
// guidance + project context (+ optional ## Agent block).
func BuildSystemPrompt(memoryDigest, projectContext string, agent *Agent) string {
	dynamic := "## Memory Guidance\n\n" + memoryDigest + "\n\n## Project\n\n" + projectContext
	if agent != nil {
		dynamic += "\n\n## Agent\n" +
			"You are operating as **" + agent.Name + "** — " + agent.Description + ".\n" +
			"Adopt this persona fully: let " + agent.Name + "'s strengths shape how you " +
			"approach the task."
	}
	return FixedPrefix + "\n\n" + dynamic
}

// BuildProjectContext builds the short project context: today's date, the
// absolute cwd, and AGENTS.md. When <cwd>/AGENTS.md exists its first 200
// lines are included under a `## AGENTS.md (first 200 lines)` heading; the
// regenerable structure cache (.kaal/STRUCTURE.md, if present) is appended
// under `## Project structure`.
func BuildProjectContext(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	lines := []string{"Date: " + time.Now().Format("2006-01-02"), "CWD: " + abs}
	agents := filepath.Join(abs, "AGENTS.md")
	if raw, err := os.ReadFile(agents); err == nil {
		lines = append(lines, "## AGENTS.md (first 200 lines)")
		content := strings.Split(string(raw), "\n")
		if len(content) > 200 {
			content = content[:200]
		}
		lines = append(lines, content...)
	} else {
		lines = append(lines, "No AGENTS.md found in this project.")
	}
	structure := filepath.Join(abs, ".kaal", "STRUCTURE.md")
	if raw, err := os.ReadFile(structure); err == nil {
		lines = append(lines, "## Project structure")
		content := strings.Split(string(raw), "\n")
		if len(content) > 120 {
			content = content[:120]
		}
		lines = append(lines, content...)
		lines = append(lines, "(full: .kaal/STRUCTURE.md — re-read it if the files change)")
	} else {
		lines = append(lines, "No structure cache yet (.kaal/STRUCTURE.md missing)")
	}
	return strings.Join(lines, "\n")
}
