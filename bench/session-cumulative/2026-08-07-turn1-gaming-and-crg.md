# 2026-08-07 — chi + rate-limit-middleware: turn-1-gaming re-check + code-review-graph

**Result headline:** the 2026-07-18 turn-1-gaming bug is NOT a prompt-content-
proximity issue — it's model-behavior-level. Decoupling turn 1's topic from
the real task (routing-tree explanation instead of "explain Timeout") did
NOT stop it: every arm, including `files`, still front-loaded the entire
10-turn task into turn 1. Real, honest, unexpected result (again).

## Motivation

`Feedback-defn-efficiency-floor` (memory) states parity with files-mode as
a standing, unmet goal. The last real session-cumulative data point
(2026-07-17) was itself invalidated by turn-1 gaming, and the planned
accountability re-run (#83) was deferred and never run. This session fixed
the harness's turn-1 prompt (decoupled from the task, per the documented
fix path) and re-ran, adding a real competitor
([code-review-graph](https://github.com/tirth8205/code-review-graph),
Tree-sitter-based, 40+ languages, 29K GitHub stars though watchers/forks
ratio is anomalous) as a fourth arm.

## Setup

- **Repo:** go-chi/chi, fresh clone per arm (isolation lesson from
  `Feedback-verify-bench-isolation` applied: separate workdir per arm,
  `git reset --hard` + `git clean -fdx` + DB re-ingest before the real run
  after a 2-turn smoke test touched all 4 workdirs).
- **Task:** same 10-turn rate-limit-middleware script as 2026-07-11/17,
  with turn 1 rewritten from "explain middleware.Timeout" (adjacent to the
  real task) to "explain chi's routing tree in tree.go / Mux.routeHTTP"
  (verified against real chi source, structurally unrelated to
  rate-limiting). See [`turns.txt`](./turns.txt).
- **Four arms**, all `claude -p --session-id`/`--resume`,
  `--strict-mcp-config`, run on a fresh EC2 t3.medium (clean box, no local
  swap contention — the local machine had 8.2GB swap in use at bench time,
  the same condition that blocked the #83 recon):
  - **files:** empty MCP config, Read/Edit/Write/Bash only.
  - **defn-natural:** defn `code` MCP tool available alongside file tools.
  - **defn-forced:** defn MCP tool only, file tools disallowed
    (`--disallowedTools=Read,Edit,Write,Bash(cat *),...`, same list as
    2026-07-11).
  - **crg-forced:** code-review-graph MCP tool only, same disallowed-tools
    list, for a like-for-like comparison against defn-forced.
- Model: whatever `claude -p` resolves to at time of run (`claude-opus-5`
  per the stream-json init event) — not the "Opus 4.8" of the 2026-07-11/17
  runs. Not re-normalized; a real current-model data point, not a controlled
  re-run of the old one.

## Result: turn-1 gaming reproduced on every arm, files included

First assistant message, turn 1, verbatim opening clause:

- files: *"I'll start by reading the routing tree code, then work through
  the implementation tasks."*
- defn-natural / defn-forced: *"I'll start by exploring the routing tree,
  then work through the implementation tasks."*
- crg-forced: *"I'll work through these in order. Let me start by exploring
  the routing tree."*

All four fully implemented `RateLimit`, wrote and ran tests, did the
context-cancellation refactor, and re-ran the race tests — entirely inside
turn 1 — despite turn 1's prompt having zero textual connection to
rate-limiting. Turns 2-10 in every arm are "already done" verification
no-ops (`files` completed all 10 turns in under 8 minutes).

**Conclusion: the 2026-07-18 fix hypothesis (turn-1 topical adjacency
causes gaming) was incomplete.** With adjacency removed, gaming still
happened, symmetrically, across every arm including the one with no agentic
graph tool at all. This looks like `claude -p --dangerously-skip-permissions`
agentic behavior at current model defaults, not a defn- or MCP-specific
effect. A real fix needs either explicit in-prompt turn-budget constraints
("do only what this message asks, nothing further") or genuinely blind
single-shot sessions with no `--resume` continuity signal — neither
implemented here.

## What this run IS still valid for

Because gaming hit every arm identically, the *relative* cost of doing the
same total amount of real work via each mechanism is still a fair
comparison — just not a read on genuine incremental-session compounding
(the thing `Feedback-defn-efficiency-floor` actually cares about). Treat
this as "total cost to complete the fixed task via mechanism X," closer in
spirit to `bench/mutations/` than to a real multi-turn session.

## Aggregate cost (10 turns, n=1 sample each)

| arm | total cost | tool calls | cache_read | cache_write | correct |
|---|---:|---:|---:|---:|---|
| files | $2.51 | 25 | 1,961,699 | 124,945 | yes (build+test pass) |
| defn-natural | $5.36 | 61 | 5,123,766 | 224,130 | yes |
| defn-forced | $5.31 | 64 | 5,137,231 | 222,293 | yes |
| crg-forced | $5.06 | 41 | 4,582,265 | 281,401 | yes |

Full per-turn CSV: [`2026-08-07-session-usage.csv`](./2026-08-07-session-usage.csv).
Raw stream-json (all 10 turns × 4 arms) in `2026-08-07-raw/out/` —
untracked per the bench-receipts gitignore convention, kept local for
digging in, not committed as-is.

## Reading the numbers honestly

- **files-mode wins outright again** — ~2.1x cheaper than any MCP-tool
  arm, matching every prior session-cumulative result back to 2026-07-11.
  Parity is still not achieved on this workload shape.
- **defn used the MOST tool calls of any arm** (61-64) despite being
  forced, more than code-review-graph's 41 under the identical
  restriction. Both defn arms cost essentially the same
  ($5.31-$5.36) regardless of natural-vs-forced, meaning the "does the
  agent substitute defn for files voluntarily" question from 2026-07-11
  didn't move the needle this run (though that's confounded by turn-1
  gaming eating the turns where substitution behavior would show).
- **code-review-graph is a real comparison point, not a strawman**: cheaper
  than both defn arms ($5.06 vs $5.31-5.36) and fewer tool calls (41 vs
  61-64) under the same forced-substitution condition, while still losing
  to files-mode by ~2x. Its own published "~65x" reduction claim (whole-
  corpus-dump baseline) does not reproduce here because this bench's
  baseline is grep/Read, not a full-repo dump — apples-to-apples, not a
  refutation of their number, which measures something else entirely.

## What NOT to conclude from this run

- "defn or code-review-graph save N% on real sessions" — still not
  supportable; turn-1 gaming means turns 2-10 are near-free padding on
  every arm, not evidence of incremental efficiency.
- "code-review-graph beats defn" as a general claim — this is one n=1 task
  on one small repo (chi), forced-substitution only. Real difference could
  easily be noise; needs repeat samples before treating the ranking as
  stable.
- That the turn-1-gaming *fix* landed — it didn't. The turns.txt rewrite is
  still a reasonable structural improvement (removes a confound) but is
  not sufficient by itself; see "What this run IS still valid for" above
  for the honest scope of what changed.

## Next steps if resumed

1. Repeat with N≥3 samples per arm before trusting the defn-vs-crg
   ranking — single-sample deltas this close (5.06 vs 5.31 vs 5.36) are
   within plausible run-to-run noise.
2. Try genuinely blind single-shot sessions (no `--resume`, fresh context
   per turn, prior turns' file changes still on disk but no conversation
   memory) as the real fix for turn-1 gaming, per the un-tried path (2)
   from `Project-session-bench-turn1-gaming-2026-07-18`.
3. Consider an explicit in-prompt constraint ("only do what this turn
   asks — do not anticipate later turns") as a cheaper structural test
   before the harder blind-session rebuild.
