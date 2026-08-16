// Ported from harness/tools.py `_TOOL_SPECS`: the OpenAI function-call
// schemas for every tool. Description texts are verbatim — the model reads
// them.
package tools

// SchemaFunction is one tool's wire shape.
type SchemaFunction struct {
	Type     string         `json:"type"`
	Function schemaFunction `json:"function"`
}

type schemaFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type spec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

func obj(required []string, properties map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func intPropMinMax(description string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max, "description": description}
}

func enumProp(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}

var toolSpecs = []spec{
	{
		Name: "read",
		Description: "Read text from a file, or list a directory. For files, prefer `:N-M`-style line " +
			"selectors: `offset` is the 1-based start line and `limit` is the number of lines to " +
			"read (omit both to read the whole file). A directory path returns a depth-limited " +
			"listing (about 2 levels; subdirectories end with '/'). Paths are resolved against the " +
			"project directory; escaping paths are blocked.",
		Parameters: obj([]string{"path"}, map[string]any{
			"path": strProp("File or directory path, relative to the project directory " +
				"(absolute paths inside it are allowed)."),
			"offset": intProp("1-based start line; omit to read from the top."),
			"limit":  intProp("Number of lines to read; omit to read to the end."),
		}),
	},
	{
		Name: "grep",
		Description: "Regex-search text files under `path` (default: the project directory), skipping " +
			"`.git`, `__pycache__`, and `node_modules` directories. Case-insensitive by default; " +
			"set `case: true` for case-sensitive matching. Returns matching lines as " +
			"`path:line_number: text`. An invalid pattern returns an error string; no matches " +
			"returns a short notice.",
		Parameters: obj([]string{"pattern"}, map[string]any{
			"pattern": strProp("Regular expression to search for."),
			"path":    strProp("Root to search under (default: the project directory)."),
			"case":    boolProp("Set to true for a case-sensitive match (default: case-insensitive)."),
		}),
	},
	{
		Name: "glob",
		Description: "Glob-match paths relative to the project directory (e.g. `src/**/*.py`), one path per " +
			"line. Patterns that would escape the project directory (e.g. `../`) are blocked.",
		Parameters: obj([]string{"pattern"}, map[string]any{
			"pattern": strProp("Glob pattern, relative to the project directory."),
		}),
	},
	{
		Name: "write",
		Description: "Write UTF-8 text to a file inside the project directory, overwriting existing " +
			"content. Returns `wrote <path> (<n> bytes)`. Escaping paths are blocked.",
		Parameters: obj([]string{"path", "content"}, map[string]any{
			"path":    strProp("File path, relative to the project directory."),
			"content": strProp("UTF-8 text to write."),
		}),
	},
	{
		Name: "edit",
		Description: "Exact-substring edit of a file: replace `old_text` with `new_text`. A single match is " +
			"replaced; zero matches return 'old_text not found'; multiple matches with `all` unset " +
			"return an error listing the count. With `all: true`, every occurrence is replaced. " +
			"Optionally scope the replacement to a line range: `offset` is the 1-based start line " +
			"and `limit` is the number of lines covered; when given, only matches inside that " +
			"range [offset, offset+limit) are considered. Escaping paths are blocked.",
		Parameters: obj([]string{"path", "old_text", "new_text"}, map[string]any{
			"path":     strProp("File path, relative to the project directory."),
			"old_text": strProp("Exact substring to replace."),
			"new_text": strProp("Replacement text."),
			"offset":   intProp("1-based start line of the replacement scope; omit to scope from the top of the file."),
			"limit":    intProp("Number of lines in the replacement scope; omit to scope to the end of the file."),
			"all":      boolProp("Replace every occurrence (default: replace only the single match)."),
		}),
	},
	{
		Name: "bash",
		Description: "Run a shell command in the project directory and return combined stdout and stderr " +
			"(capped at 10000 chars); runs via `/bin/sh -c` on unix and `cmd.exe /C` on Windows, in a " +
			"sanitized environment with a minimal PATH (the project `.venv/bin` (or `.venv/Scripts` " +
			"on Windows) when present, plus `/usr/local/bin`, `/usr/bin`, `/bin` on unix or " +
			"`%SystemRoot%\\System32` on Windows). `timeout` defaults to 30 seconds and may not exceed " +
			"300. Destructive commands (rm -rf, git push, git reset --hard, git clean -f, mkfs, dd, " +
			"fork bombs, `> /dev/sd*`) are blocked by policy unless the registry allows dangerous " +
			"commands.",
		Parameters: obj([]string{"command"}, map[string]any{
			"command": strProp("Shell command to run."),
			"timeout": intProp("Seconds before the command is killed; defaults to 30, max 300."),
		}),
	},
	{
		Name: "memory_append",
		Description: "Append a note to the project memory store under a fixed section, returning the memory " +
			"file path or 'already recorded' when the note is a duplicate.",
		Parameters: obj([]string{"section", "text"}, map[string]any{
			"section": enumProp("Memory section to append to.", memorySections),
			"text":    strProp("Note text to record."),
		}),
	},
	{
		Name: "spawn_agent",
		Description: "Run a nested kaal agent on a sub-task and return its JSON summary " +
			"{answer, steps, usage, session_id}; answer capped at 50000 chars.",
		Parameters: obj([]string{"task"}, map[string]any{
			"task": strProp("The sub-task for the nested agent to complete."),
			"dir": strProp("Sub-project directory for the nested agent (default: the " +
				"current project directory; escaping paths are blocked)."),
			"max_steps": intPropMinMax("Maximum turns for the nested agent (default: 5).", 1, 5),
			"timeout":   intPropMinMax("Wall-clock seconds before the nested run is abandoned (default: 120).", 1, 300),
		}),
	},
	{
		Name: "ask_user",
		Description: "Ask the user a question and wait for their answer. Use when you need a " +
			"decision, a confirmation, or information only the user has. The answer " +
			"is returned as the tool result.",
		Parameters: obj([]string{"question"}, map[string]any{
			"question": strProp("The question to ask the user."),
			"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional choices; when given, the user picks one."},
		}),
	},
	{
		Name: "spawn_parallel_task",
		Description: "Run several independent sub-tasks in parallel as nested kaal agents. " +
			"Each task: {task: string, dir?: string, max_steps?: int (1-5), " +
			"timeout?: int (1-300)}. Returns a JSON array of " +
			"{index, answer, steps, usage, session_id, error?}.",
		Parameters: obj([]string{"tasks"}, map[string]any{
			"tasks": map[string]any{
				"type":        "array",
				"description": "Sub-tasks to run concurrently; results come back in this order.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task":      strProp("The sub-task for the nested agent."),
						"dir":       strProp("Sub-project directory for the nested agent (default: the current project directory; escaping paths are blocked)."),
						"max_steps": intPropMinMax("Maximum turns for the nested agent (default: 5).", 1, 5),
						"timeout":   intPropMinMax("Wall-clock seconds before the nested run is abandoned (default: the tool-level timeout).", 1, 300),
					},
					"required": []string{"task"},
				},
			},
			"timeout": intPropMinMax("Default per-task wall-clock seconds (default: 120).", 1, 300),
		}),
	},
}

// Schemas returns the OpenAI function-call schemas for every tool. Built once
// and memoized; each call returns a fresh top-level list — a caller that
// mutates the list cannot corrupt the cache (the inner structs are shared).
func (r *Registry) Schemas() []any {
	out := make([]any, 0, len(toolSpecs))
	for _, s := range toolSpecs {
		out = append(out, SchemaFunction{
			Type: "function",
			Function: schemaFunction{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.Parameters,
			},
		})
	}
	return out
}
