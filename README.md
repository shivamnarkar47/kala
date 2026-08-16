# kaal

**kaal** is a Go agent harness — one static binary — that runs **DeepSeek V4
Flash** with tools, persistent memory, sessions, and a bubbletea TUI. Core
packages are stdlib-only.

```sh
go build -o kaal ./cmd/kaal
export OPENCODE_API_KEY=sk-...
./kaal                        # the TUI
./kaal run "Summarize the repo structure" --dir .
```

## Quick start

1. **Build:** `go build -o kaal ./cmd/kaal` (or `go install github.com/kaal/kaal/cmd/kaal`).
2. **Key:** the key is sought in order — env `OPENCODE_API_KEY`, then
   `~/.config/kaal/api_key` (saved from the TUI via `/connect`), then the omp
   auth store (`~/.omp/agent/agent.db`, read-only).
3. **Run:** `kaal run "PROMPT"` prints the answer; `kaal` opens the workbench.

## Commands

| Command | What it does |
|---|---|
| `kaal` | the bubbletea TUI |
| `kaal run "PROMPT" [flags]` | one-shot headless run (flags: `--dir --model --max-steps --memory-root --allow-dangerous --resume --verbose --json --batch --workers --no-tool-cache --no-verify --agent`) |
| `kaal sessions list\|show\|delete\|prune` | manage the JSONL session store |
| `kaal doctor` | self-check (go, terminal, api key, gateway, structure cache, sessions) |
| `kaal update` | self-update (git pull + rebuild, or main-branch tarball overlay) |
| `kaal diagrams <file.mmd>` | render mermaid as terminal art (needs `uv tool install termaid`) |

Exit codes: `0` answer · `1` config/key/gateway error · `2` loop error.

## Development

```sh
go test -race ./...        # the whole suite (264+ tests)
go vet ./...
git config core.hooksPath .githooks   # pre-commit/pre-push gate
```

The tree:

```
cmd/kaal        the binary
internal/cli    cobra command surface
internal/loop   the agent loop (stream → heal DSML → execute tools → persist)
internal/gateway  SSE client, pooled transport, retries
internal/dialect  DSML envelope healer (U+FF5C / U+2581 markers are load-bearing)
internal/messages  wire model; reasoning_content replay (*string)
internal/context   token estimate + history truncation
internal/tools     tool registry: read/grep/glob/write/edit/bash/memory_append/spawn/ask
internal/sessions  JSONL store + async writer
internal/memory    .agent-memory digest/caps/dedupe
internal/structure .kaal/STRUCTURE.md tree cache
internal/toolcache read-only tool-result cache
internal/tui     the bubbletea workbench (the only charmbracelet import)
internal/jsonpy  Python-compatible JSON (wire byte-parity)
internal/agents  the five Pandava personas
internal/parity  the P7 gate instrument (self-skips without a Python checkout)
```

The two hard things: DSML healing (`internal/dialect`) and
`reasoning_content` replay (`internal/messages`) — both locked by table tests
ported from the Python camp and by a byte-identical turn-2 golden fixture.

## License / history

This is the Go heir of a Python harness (7.8k lines, 17 modules) ported
formation by formation and burned at a 22-case parity gate — the campaign
record lives in `docs/go-migration-plan.md`.
