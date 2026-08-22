# AGENTS.md — kaal (`kaal`)

> **Durable anchor memory.** This file is the stable ground of the harness: its first 200 lines are carried, whole, into every kaal agent's system prompt (`internal/prompts` → `BuildSystemPrompt`). What endures belongs at the top; what changes from day to day belongs in `.agent-memory/` (see §6), not here.

## TL;DR / 30-Second Orientation

**What this is:** `kaal` is a Go agent harness — one static binary — that runs **opencode zen's keyless free tier** by default (`hy3-free` today; any `*-free` model works with no login) with tools, persistent memory, sessions, and a bubbletea TUI. Two providers: **opencode** (main; paid route zen/go/v1 + free tier zen/v1) and **Command Code** (`api.commandcode.ai/provider/v1`, OpenAI-shaped models only — e.g. `stealth/ox-alpha`; key `CMD_API_KEY`). Core packages are stdlib-only; `github.com/charmbracelet/*` appears ONLY in `internal/tui`; cobra (CLI) and modernc.org/sqlite (the omp auth-store read) are the other two dependencies.

**Get productive immediately:**
- `go build -o kaal ./cmd/kaal && ./kaal` — the bubbletea TUI (needs an API key)
- `./kaal run "PROMPT" [flags]` — one-shot headless agent run
- `go test -race ./...` — the whole suite (264+ tests)
- `kaal run --help` — every flag

**GOTCHA: the two hard things (know these before touching anything):**
1. **DSML healing.** The model does not deliver its tool calls cleanly. It emits them as a DSML XML envelope (`<｜DSML｜tool_calls>`, fullwidth pipe **U+FF5C**) that leaks into visible `delta.content` instead of arriving as structured `tool_calls`. `internal/dialect` `DialectFeed` heals it incrementally and strips leaked chat-template tokens (`<｜begin▁of▁sentence｜>`, `<｜Assistant｜>`, …). When both arrive, **structured tool_calls win** over healed ones (`internal/loop`).
2. **`reasoning_content` replay.** When an assistant turn has made tool calls, the next request MUST carry that turn's streamed `reasoning_content` verbatim; otherwise the gateway **400s on turn 2+**. `internal/messages` `AssistantMessage.ToWire()` always replays it (`ReasoningContent *string` — the Go zero-value trap). NEVER synthesize a placeholder.

## 1. Commands

*Purpose: every real command, verified against the installed CLI — copy, paste, and it runs.*

| Command | What it does |
|---|---|
| `go build -o kaal ./cmd/kaal` | Build the static binary (CGO off for static: `CGO_ENABLED=0`) |
| `go test -race ./...` | The full Go test suite |
| `go vet ./...` | Vet the tree |
| `kaal` | Launch the bubbletea TUI (default surface; starts keyless — `/connect <key>` adds a key) |
| `kaal run "PROMPT"` | One-shot headless run; answer to stdout |
| `kaal run --help` | All run flags |
| `kaal sessions list` | List sessions as `<id> <ts> <prompt>` |
| `kaal sessions show <id>` | Show one session's details/prompt |
| `kaal sessions delete <id>` | Delete one session |
| `kaal sessions prune [--keep N]` | Delete all but the newest N sessions |
| `kaal doctor` | Self-check: go, terminal, api key, gateway, structure cache, sessions dir |
| `kaal --version` | Print `kaal 0.3` and exit |
| `kaal update` | Self-update: git pull + rebuild, main-branch tarball overlay, or prebuilt release binary (no git/go needed) |
| `kaal diagrams <file.mmd>` | Render a mermaid diagram as terminal Unicode art via termaid (`uv tool install termaid`) |
| `git config core.hooksPath .githooks` | Enable build-check hooks: pre-commit & pre-push run gofmt + vet + `go test -race` + version probe; skip with `KAAL_SKIP_HOOKS=1` |
| `python3 scripts/benchmark.py` | p50–p99 TTFT/total latency against any OpenAI-compatible endpoint (defaults: zen free tier, keyless); `--base-url/--model/--api-key/--requests/--json` |
| `kaal bench` | same benchmark as a live TUI report card (progress bar + percentiles); flags mirror the script; `--json` emits raw samples; exit 130 on ctrl+c abort |

`kaal run -` reads the prompt from stdin in place of `"PROMPT"` — the route for piped input.

**`kaal run` flags** (from `--help`):

| Flag | Meaning | Default |
|---|---|---|
| `prompt` | task to run (positional) | required |
| `--dir DIR` | project directory — tools are cwd-constrained to it | cwd |
| `--model MODEL` | model id | saved default, else `hy3-free` (keyless) |
| `--max-steps MAX_STEPS` | max agent turns | 20 |
| `--memory-root MEMORY_ROOT` | memory directory | `<dir>/.agent-memory` |
| `--allow-dangerous` | skip the destructive-command DENY list | off |
| `--resume SESSION_ID` | continue a session | none |
| `--verbose` | print reasoning to stderr | off |
| `--json` | final JSON line `{"session_id","answer","steps","tool_calls","usage","cost"}` | off |
| `--batch FILE` | run prompts from FILE (one per line, or a JSON array), one session each | none |
| `--workers N` | max concurrent `--batch` tasks | `min(4, cpu count)` |
| `--no-tool-cache` | disable the read-only tool-result cache (`.kaal/tool-cache.json`) | off (cache on) |
| `--no-verify` | disable verify hooks after mutation (`.kaal/hooks.json`) | off (verify on) |
| `--agent NAME` | persona from `.kaal/agents.json` (the five Pandavas always exist) | none |

**Exit codes:** `0` answer produced · `1` config/key/gateway/agent error · `2` loop error (max steps, context overflow, tool loop, 5 consecutive tool failures).

**API keys:** per provider — except zen's free tier, which is **keyless**: `*-free` models (and `grok-code`/`big-pickle`) accept anonymous requests on `zen/v1`, so kaal runs with no login at all; dummy bearer tokens are rejected, so the gateway omits Authorization entirely when keyless. **opencode:** env `OPENCODE_API_KEY` → user key store `~/.config/kaal/api_key` (0600; saved from the TUI via `/connect`) → the opencode CLI's own login store `~/.local/share/opencode/auth.json` (`opencode-go` credential) → omp auth store `~/.omp/agent/agent.db` (read-only sqlite via modernc.org/sqlite — pure Go, no cgo). **Command Code:** env `CMD_API_KEY` (alias `COMMANDCODE_API_KEY`) → user key store `~/.config/kaal/api_key.commandcode` → shared auth files (`~/.commandcode/auth.json`, `~/.omp/agent/auth.json`, `~/.pi/agent/auth.json` — the shapes the official CLI and pi/OMP write). `/connect` opens a provider picker: opencode with a resolvable key jumps straight to its model list; command-code asks for its key first; "add another provider" is BYOK — base URL + key, kaal probes `<base>/models` live and persists the pick to `~/.config/kaal/custom_provider.json` (0600), which then owns that model id for every routing lookup (`config.ModelProvider`/`ModelBaseURL`; env override `<NAME>_API_KEY`). `/connect <key>` inline still saves to the active model's provider store. The TUI **starts keyless** (home screen hints `/connect`; sending is blocked until a key resolves) — only headless `kaal run`/`kaal doctor` exit 1 with instructions when missing. Never cache or write keys outside `config.SaveUserAPIKeyFor`.

**Sessions:** each session is a JSONL record at `~/.local/share/kaal/sessions/` (override: env `KAAL_SESSIONS_DIR`). The id takes the form `%Y%m%d-%H%M%S-%f` (microseconds, spin-guarded against same-tick collisions — `internal/sessions`).

**TUI slash commands** (`internal/tui`):

| Command | Action |
|---|---|
| `/help` | list commands and keys |
| `/new` | fresh session id, clear pane |
| `/resume <id>` | continue a session |
| `/sessions` | session switcher (Enter resume · n new · d delete) |
| `/models` | model switcher with filter + per-M prices |
| `/agents` | persona switcher (Enter activate · n new form · d delete) |
| `/connect [key]` | provider picker (opencode free · opencode go · command-code · add another BYOK); inline `<key>` saves to the active provider's store |
| `/memory` | show memory digest + file paths |
| `/model` | show current model id |
| `/verbose` | toggle reasoning display |
| `/sidebar` / `/topbar` / `/diagrams` | toggle chrome and mermaid auto-render |
| `/structure` | dump the structure cache |
| `/diagram <file.mmd>` | render a mermaid file via termaid |
| `/quit` | exit |

**TUI keys:** `enter` send · `shift+enter` newline · `ctrl+p/n` history · `tab` slash-complete · `ctrl+l` bottom · `ctrl+s` sidebar · `ctrl+t` topbar · `ctrl+d` diagrams · `ctrl+g` AI agent generator · `ctrl+c` cancel (HARD — aborts the in-flight SSE stream) · `ctrl+q` quit. The font the TUI shows is the terminal's own: configure the emulator.

## 2. Architecture

*Purpose: how the code is shaped, told in plain words — the layering, the flow of data, no diagram.*

**Data flow (one `kaal run`):** the road of a single run is fixed. `internal/cli` builds `Gateway` + `Memory` + `ToolRegistry` + `AgentLoop`, then calls `loop.Run(prompt, emit)`. Each turn follows the same road: `ToWireMessages()` → `gateway.Stream()` (SSE) → `DialectFeed` heals DSML out of `content` deltas → resolved `ToolCall`s → `ToolRegistry.Execute()` → the result is appended as a tool message and persisted to the session JSONL → and so it repeats until the model answers with no tool calls, or `max_steps` is spent.

**Layering:**
- **Core — stdlib-only:** `internal/config`, `internal/gateway`, `internal/dialect`, `internal/messages`, `internal/context`, `internal/loop`. No new dependencies, ever. (`golang.org/x/sync/errgroup` was the plan's allowance; the loop uses a semaphore channel instead.)
- **Persistence:** `internal/memory` (`.agent-memory/`), `internal/sessions` (JSONL store + `AsyncWriter`), `internal/structure` (`.kaal/STRUCTURE.md`), `internal/toolcache` (`.kaal/tool-cache.json`).
- **Tools:** `internal/tools` — OpenAI function schemas + guarded execution (path confinement, DENY list, 10k-rune caps).
- **Front-end:** `internal/tui` — the ONLY package importing bubbletea/bubbles/lipgloss/glamour. `internal/cli` (cobra) is the command surface; `internal/agents` the Pandava personas.
- **Port seam:** the `AgentEvent` stream in `internal/loop` — the TUI and `kaal run` both consume exactly this; never bypass it.
- **Gateway behavior:** retries 5xx/network up to 3× (1s/2s/4s backoff); 4xx raises immediately; never retries after visible content. Pooled transport (`MaxConnsPerHost 4`, idle 90s); batch workers get a transport per goroutine.
- **Parallel tool batches:** all-read batches (`read`/`grep`/`glob`) run concurrently (≤4 workers, worker id in ctx); any batch containing a mutator, `ask_user`, or a spawn runs serially in call order. Events, persistence, tool-loop detection, and failure counting are recorded in call order on the main goroutine.
- **grep:** rg-backed when `rg` is on PATH (streamed, stops at the result cap); the pure-Go fallback scans files in parallel (≤4) but joins deterministically in walk order — post-cap files never appear (the sentinel guarantee). RE2 means backreferences are rejected on BOTH engines (a documented divergence from Python's `re`).
- **Tool-result cache:** read/grep/glob results cached in `.kaal/tool-cache.json` keyed by `tool|sha256(args)|structure_signature`; a changed tree auto-misses; a mutating batch bypasses lookups and drops the cache.
- **Verify hooks:** after a mutating batch, the configured `.kaal/hooks.json` `verify` command runs (30 s timeout) and its output is appended as a `user` message (`[verify] …`, dimmed in TUI, stderr in `kaal run`) — content for the model, never a loop abort.
- **spawn_agent / spawn_parallel_task:** nested `AgentLoop`s on sub-tasks (own session ids, serial/parallel, depth-capped at 2, `allow_dangerous=false`, no tool cache, wall-timeout).
- **Async persistence:** one `sessions.AsyncWriter` per process serializes appends; the loop flushes on turn end so the store is durable when `Run` returns.

**Events:**
- `AgentEvent` (loop → front end): `content | reasoning | tool_start | tool_result | verify | step | done | error`
- `StreamEvent` (gateway → loop): `content | reasoning | tool_call | done | error`

**PATTERN — the canonical example:** `internal/loop/loop_test.go::TestTwoTurnToolCallFlow` holds the whole loop contract in one test: a fake gateway streams DSML, the loop heals it, executes, replays reasoning verbatim on turn 2, persists, and answers. Read this one test and the loop is known. The parity gate (`internal/parity`) diffed both armies on 22 corpus cases before the Python camp burned — it self-skips now that `harness/` is gone.

## 3. QUICK START MAP

*Purpose: which file serves which end, and when to open it.*

| File | Purpose | Open when… |
|---|---|---|
| `internal/cli/cli.go` | cobra command surface: run, sessions, doctor, update, diagrams; exit codes | tracing a command or exit code |
| `internal/tui/tui.go` | bubbletea workbench: conversation, sidebar, modals, slash commands; the loop runs on a goroutine, events marshal via `sendFn` | working on the UI |
| `internal/loop/loop.go` | `AgentLoop`: stream→heal→execute→persist; `AgentEvent` seam; cancel via ctx (`ErrCancelled`) | tracing agent behavior end-to-end |
| `internal/gateway/gateway.go` | SSE client; wire body/headers; retries; pooled transport; `Opener` seam | touching the wire protocol |
| `internal/dialect/dialect.go` | DSML state machine + leaked-token stripper | healing bugs — every agent touches this eventually |
| `internal/messages/messages.go` | Wire structs; `reasoning_content` replay rule | message shape, or 400s on turn 2+ |
| `internal/context/context.go` | Token estimate + history truncation (the ledger) | budget / overflow logic |
| `internal/tools/tools.go` | Tool registry, schemas, path safety, DENY list | tools or safety |
| `internal/sessions/sessions.go` | JSONL store, resume replay, `AsyncWriter` | sessions / resume |
| `internal/memory/memory.go` | `.agent-memory/` digest, caps, dedupe | memory behavior |
| `internal/jsonpy/jsonpy.go` | Python-compatible JSON (byte-parity for the wire) + `OrderedMap` | wire bytes, DSML argument order |

## 4. IF YOU SEE X → IT MEANS Y

*Purpose: to read a strange output and know its meaning at once.*

| You see… | It means… |
|---|---|
| DSML tags (`<｜DSML｜invoke …>`) in output | a tool call was healed by `DialectFeed` — don't strip it manually |
| HTTP 400 on turn 2+ | `reasoning_content` was dropped; replay it verbatim (`internal/messages`) |
| Model never calls tools | no `tool_choice` support — never send it (nor `temperature` / `stream_options` / `store`) |
| `…[truncated]` at the end of tool output | 10k-char cap (`MaxResultChars`, `internal/tools`) |
| `blocked by harness policy (destructive command)` | DENY list fired; re-run with `--allow-dangerous` only if intentional |
| `<think>…</think>` inside content | reasoning span — routed to `("reasoning", …)`, not answer text |
| `Discarding unclosed DSML section…` | an unclosed envelope that parsed ≥1 invoke is discarded; sections with 0 invokes are recovered as visible text (prose quotes of the envelope) |
| `tool loop detected` / `5 consecutive tool failures` | loop aborted; exit 2 |
| `(busy — ctrl+c cancels)` | TUI turn in flight; Ctrl+C is a HARD cancel (ctx → the SSE stream aborts) |
| `spawn_agent: recursion limit reached` | nested-agent depth cap (2 loops) — an expected guardrail, not an error |
| stale tool results after external edits | tool cache is signature-keyed (changed tree = miss); `--no-tool-cache` disables |
| `[verify] …` user message after a mutation batch | post-mutation self-check ran (`.kaal/hooks.json`) |
| a JSON result with `"steps": 1.0` | a float round-trip crept back in — parse with `json.Number` (the parity gate caught this once) |
| 403 `upgrade_required` on a command-code model | Go-plan account (no Provider API) — kaal auto-falls back to `/alpha/generate` and remembers it per process (`internal/gateway/commandcode.go`) |
| `generate HTTP 400 … messages[N].role` | tool-result wire shape broke: every role:"tool" part MUST carry `toolName` (CLI defaults "unknown"); assistant turns replay text|reasoning|tool-call parts — pinned by `TestBuildGenerateBodyToolRoundTripShape` |

## 5. PITFALLS

*Purpose: the mistakes that have cost hours — read them before you edit.*

- **PITFALL: core must stay stdlib-only.** `internal/{gateway,dialect,messages,context,loop,config,prompts,tools,memory,sessions,structure,toolcache}` map 1:1 to a future Rust port. No new deps in core. bubbletea/bubbles/lipgloss/glamour are legal ONLY in `internal/tui`; cobra only in `internal/cli`; modernc.org/sqlite only in `internal/config`.
- **PITFALL: never send `tool_choice`, `temperature`, `stream_options`, or `store`.** This model rejects them (`gateway.BuildBody` — the fields do not exist in the struct).
- **PITFALL: unicode markers are load-bearing.** ｜ = U+FF5C, ▁ = U+2581. Match them exactly (`FW = "\uff5c"`, `B = "\u2581"`; build fixtures from escapes, never paste glyphs). The model never trained on ASCII substitutes — transliterating breaks healing.
- **PITFALL: reasoning replay is mandatory.** `AssistantMessage.ToWire()` re-sends `reasoning_content` when present; never synthesize a placeholder. Dropping it 400s on the next turn. `ReasoningContent` is `*string` — nil means absent.
- **PITFALL: call `DialectFeed.Flush()` at end of stream** (the loop does); unclosed sections that parsed ≥1 invoke are deliberately discarded there; 0-invoke sections are recovered as visible text. An envelope that follows visible text in the same turn is a prose quote, never healed.
- **PITFALL: structured beats healed.** `calls = structuredCalls if len > 0 else healedCalls` in `internal/loop` — don't "fix" that precedence.
- **PITFALL: tool results are strings.** 10k-rune cap; `bash` timeout 30s default / 300s max; `grep` is case-insensitive unless `case:true`; `read` `offset` is 1-based; dir listings sort dirs and files separately (Python parity).
- **PITFALL: JSON parity is byte-level for the wire.** `internal/jsonpy` marshals with Python's separators and insertion-ordered `OrderedMap` for DSML args. `encoding/json` sorted keys are fine for CLI records, NOT for wire bodies.
- **PITFALL: an empty answer is an answer.** `stepLoop` distinguishes "turn done with empty content" from "continue" via the `done` bool — don't collapse them (the parity gate caught this).
- **PITFALL: TUI thread rules.** The loop runs on its own goroutine; it never touches widgets — events marshal via `sendFn` (`program.Send`). Streaming markdown re-renders the conversation on a flush cadence (instant under 2k chars, 100ms/250ms throttled).
- **PITFALL: cancel semantics.** Ctrl+C cancels the turn's ctx: the in-flight SSE stream aborts, and the partial turn's events are NOT persisted (Python's `TurnCancelled` semantics). The ask handler is ctx-aware — a cancelled turn answers `(cancelled)` instead of hanging on the modal.

## 6. Memory

*Purpose: what to write down, where, and when.*

**Files** (committed, in `.agent-memory/`): `project-state.md` · `decisions.md` · `patterns.md` · `lessons-learned.md`.

**Update triggers** — write when: a milestone is complete; an architectural decision is made; a non-obvious gotcha is discovered; anything has consumed excessive time. Use the `memory_append` tool (sections: `project-state | decisions | patterns | lessons-learned`) or edit the files directly.

**Rules** (`internal/memory`): 200-line cap per file (oldest `##` section pruned); digest capped at 4000 est. tokens and 60 lines/section, head-biased — put critical notes early, keep entries self-contained; verbatim dedupe returns `already recorded`; each session outcome is auto-appended to `project-state.md`.

**AGENTS.md is the durable anchor; `.agent-memory/` is the moving state.** Edit AGENTS.md only for stable, load-bearing facts; put evolving state in the memory files.

### `.kaal/` files — caches & config, NOT memory

No memory dwells under `.kaal/`; everything there is regenerable cache or explicit config: `STRUCTURE.md` (tree cache, signature-keyed — noise dirs skipped, depth ≤ 6, ≤ 20k entries, ≤ 500 lines, first 120 lines into the system prompt), `tool-cache.json` (read-only tool-result cache), `hooks.json` (verify-hook config), `agents.json` (personas, git-ignored via `.kaal/`).

## 7. Tool preferences

*Purpose: to explore with the least cost to the context.*

1. **Directory listing** (`read` on a directory → ~2-level listing) to orient → **`grep`** to locate → **line-selected `read`** (`offset`/`limit` or `:N-M`) to read only what's needed.
2. `grep` before whole-file reads, always. `glob` to map structure.
3. Never whole-file-read to find one symbol; never re-read files you already have.

## 8. Navigation order

*Purpose: the fastest road for a new agent.*

1. This file (you're here).
2. `internal/loop/loop_test.go::TestTwoTurnToolCallFlow` — the whole loop contract in one test.
3. `internal/loop/loop.go` → `internal/dialect/dialect.go` → `internal/messages/messages.go` (the two hard things).
4. `internal/cli/cli.go` + `internal/gateway/gateway.go` (entry + wire).
5. `internal/tools/tools.go` (safety) → `internal/sessions` + `internal/memory` (persistence).
6. `internal/tui/tui.go` last — it's a thin consumer of the `AgentEvent` stream.

## 9. The war record

The Python camp (7,785 lines across 17 `harness/` modules) was ported formation by formation, tested table by table, and diffed on a 22-case parity corpus before being burned at the P7 gate. The plan of that campaign lives in `docs/go-migration-plan.md`; the gate instrument (self-skipping without a Python toolchain) lives in `internal/parity`. The campaign's open questions are answered as of 2026-08-17: `kaal update` gained a release-fetch path (GitHub Releases asset swap — no git checkout or Go toolchain needed; source paths remain when both exist), and the `bash` tool gained Windows parity (`cmd.exe /C` + `System32` PATH when `/bin/sh` is absent).
