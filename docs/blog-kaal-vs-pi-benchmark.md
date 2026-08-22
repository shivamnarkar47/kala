# I Benchmarked My Homegrown Agent Harness Against Pi. It Lost. Here's the Honest Scorecard.

*A work-in-progress report on kaal vs pi — where kaal stands today, why it's slow, and the plan to close the gap.*

## The premise

I built my own coding-agent harness. It's called **kaal** — one static Go binary: agent loop, DSML-healing gateway, guarded tools, sessions, memory, a bubbletea TUI, six release artifacts, 264+ tests. It works. You can `curl | sh` it right now.

Then I benchmarked it against [pi](https://github.com/badlogic/pi-mono), Mario Zechner's minimal TypeScript harness, and the gap was not flattering. This post is the honest scorecard: what I measured, why kaal loses today, what's already fixed, and what's next. Not a hit piece on my own project — a baseline. You can't close a gap you won't measure.

## The scoreboard

| Metric | pi | kaal | Verdict |
|---|---|---|---|
| Time to first token (turn 1) | ~0.6 s | ~2.4 s | kaal pays TCP+TLS + a fat prefix before anything streams |
| Time to first token (warm turn) | ~0.5 s | ~1.3 s | even warmed, the prefix tax remains |
| Context sent per turn | ~9k tokens | ~31k tokens (~3.5×) | kaal carries AGENTS.md + memory digest + structure cache every turn |
| Turns to finish a typical refactor task | 5 | 11 | pi's leaner context reasons sharper; kaal also burns steps on re-reads its cache should have prevented |
| Perceived stream latency (TUI) | snappy | occasionally chunky | bursts over 2k chars throttle at 250 ms |
| Cost per task | low | $0 (free tier) | kaal wins the only row it wins |

(Numbers are from my own runs on one machine, same repo tasks for both agents — directional, not lab-grade. The point of `kaal bench` is to make them reproducible.)

The headline: **kaal is too slow.** Not broken — slow. And the interesting part is *why*, because almost none of the slowness is in Go.

## Why kaal is slow — the autopsy

### 1. The prompt prefix is a cargo ship

Every kaal turn ships: the first 200 lines of `AGENTS.md`, a memory digest capped at **4,000 estimated tokens**, and the first 120 lines of a structure cache — before the conversation even starts. Pi's founding thesis is *context engineering is paramount*, and Databricks' internal benchmark found pi sent roughly **3× less context per turn** than Claude Code and Codex while posting the highest pass rate. Kaal did the opposite: it treats context like free real estate. Every redundant token is paid twice — once in latency, once in reasoning quality, because fat prefixes dilute the model's attention on the actual task.

### 2. `Warm()` is an empty function

This one still makes me laugh. The Python harness had a real connection-warming trick: pre-open the keep-alive connection at TUI mount so the first turn skips TCP+TLS handshake. When I ported to Go, the seam survived — `internal/gateway/gateway.go:351` reads:

```go
// Warm pre-opens the transport connection so the first Stream skips the
func (g *Gateway) Warm() {}
```

A documented comment sitting on top of a no-op. First-turn latency includes a full connection setup, every session, every time.

### 3. Token budgeting by vibes

Kaal estimates tokens as `len(text) / 3`. That's fine for deciding whether to truncate history — and terrible for anything precise. The consequence is conservative over-truncation (throwing away useful turns "just in case") or overflow-retry cycles when the estimate is wrong in the other direction. A retry costs a full round trip plus the backoff ladder: **1s, then 2s, then 4s**. One bad estimate can add seven seconds of pure sleep to a turn.

### 4. Truncate-and-pray instead of compact

When history outgrows the budget, kaal drops the oldest turns. Pi does structured compaction — summarizing state so the model keeps its bearings across long tasks. Without it, long sessions either lose context or balloon; both look like "the agent got slow and dumb" from the chair.

### 5. Reasoning replay tax

Kaal's target models leak tool calls into content (healed by a DSML state machine) and require the previous turn's `reasoning_content` replayed verbatim or the gateway 400s on turn 2+. Correctness demands it — but it means every turn re-ships growing reasoning payloads. On a chatty model this compounds per step.

### 6. The model itself

Full honesty: kaal defaults to a **free-tier** model (`hy3-free`) because keyless $0 was the whole point. Pi's benchmark glory is on Opus at high thinking effort. Part of kaal's slowness is simply a cheaper brain. But that's the point of the exercise — a good harness should make a weak model *fast enough*, and that's the bar I'm holding kaal to.

## Current progress — what's already landed

The foundation that the speed work will stand on is real and tested:

- **19,792 lines of Go**, stdlib-only core, 264+ race-tested cases. The loop, dialect healing, and wire layer are stable — no perf work has had to fight correctness bugs.
- **Pooled transport**: `MaxConnsPerHost=4`, idle connections kept 90s — steady-state turns already reuse connections.
- **Adaptive TUI flushing**: deltas under 2k chars render instantly; larger streams throttle at 100–250ms. Perceived stream lag is bounded, and it's tunable.
- **Parallel read batches**: all-read tool batches (read/grep/glob) run concurrently, ≤4 workers.
- **Tool-result cache**: repeated reads hit `.kaal/tool-cache.json` instead of the disk.
- **v0.3 shipped**: checksummed installers, self-update, Windows bash parity.

In other words: the boring infrastructure is done. What remains is squarely the *context* problem — which is exactly the problem pi proved decides these benchmarks.

## The catch-up plan

Ordered by expected payoff:

1. **Measure first.** A `kaal bench` mode: TTFT, tokens/sec, bytes-per-turn, steps-per-task, logged per session. No more vibes-based slowness claims — including mine.
2. **Shrink the prefix.** Cut the AGENTS.md injection to a true digest, drop the memory digest cap from 4k toward pi-scale, make structure-cache injection lazy. Target: 3× less context per turn — pi's number, deliberately.
3. **Implement `Warm()` for real.** Pre-dial at startup; delete the embarrassing no-op. Cheap, immediate first-turn win.
4. **Real tokenizer for budgets.** Replace `len/3` with a proper BPE-count approximation so truncation stops being paranoid and retries stop happening.
5. **Compaction.** Port a summarize-on-overflow pass so long sessions stay lean instead of truncated.
6. **Re-benchmark, publish, repeat.** Same table, new numbers, in public.

## The takeaway so far

Pi didn't beat kaal with more engineering hours. It beat it with a smaller idea executed ruthlessly: *the fastest harness is the one that sends the least*. I built a feature-complete harness first and assumed speed would follow. It doesn't. Speed is a design property of the context, not a property of the language runtime — my Go binary is instant at everything except the only thing that matters, which is giving the model less to read.

Current status: behind, measuring, and climbing. Next update when `kaal bench` lands and the prefix diet shows up in the numbers.
