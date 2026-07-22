# defn benches — receipts

This directory is the source of truth for every performance claim defn
makes. If a number appears in a README, blog post, or talk without a
receipt here, it doesn't exist.

Each run captures **workload class**, **fixture**, **exact per-case
tool calls + input/output tokens**, and **correctness pass/fail**.
Runs that produced the wrong output are visible, not hidden.

## What we measure and why

Three kinds of workloads live here today:

- **`retrieval/`** — read-side questions ("what does X do", "who calls
  Y"). Static ground truth from a vendored corpus with a SHA1
  train/test split; provenance in [`retrieval/VENDORED.md`](./retrieval/VENDORED.md).
- **`mutations/`** — write-side workloads. Each case is a
  before-fixture, a natural-language instruction, and a golden after-
  fixture. The agent runs under `claude -p` in both `files` mode
  (Read/Edit/Write) and `defn` mode (the `code` MCP tool). A run is
  **correct** only if the final file, whitespace-canonicalized,
  equals the golden.
- **`trajectories/`** — static analysis of external agent
  trajectories (no defn run). Take a real-world OpenHands or similar
  trajectory and label each tool call as graph-collapsible / neutral.
  This is *not* a benchmark — it's evidence-gathering for what defn
  would save on real production workloads.
- **`session-cumulative/`** — end-to-end multi-turn Claude Code
  sessions run through `claude -p --resume`, per-turn usage measured
  including cache_read / cache_creation. This is the workload class
  users actually pay for.

Correctness is not optional. A defn win under a correctness drop is
not a win.

## Session-cumulative benches

| Run | Date | Task | Turns | Correct | Cost Δ | Notes |
|---|---|---|---|---|---|---|
| [chi-ratelimit](./session-cumulative/2026-07-11-chi-ratelimit.md) | 2026-07-11 | add RateLimit middleware to chi + ctx-cancel refactor | 10 | files 4/4 · defn 3/3 · defn-forced 4/4 | **+40% natural / +28% forced (still WORSE)** | Opus 4.8 uses defn *additively*. Hard tool restriction (`--disallowedTools`) forces substitution but reveals granularity cost: 13 `create` calls vs 1 `Write`. Defn's op vocabulary optimized for surgical mutations, not multi-symbol authoring. |

## Trajectory analyses

| Run | Date | Data source | N | Est. call Δ | Notes |
|---|---|---|---|---|---|
| [openhands-analysis](./trajectories/2026-07-11-openhands-analysis.md) | 2026-07-11 | `nebius/SWE-rebench-openhands-trajectories` | 1,991 aggregate + 4 labeled | **−15% to −25%** | first look at real production trajectories; 40–50% of every trajectory is test-run time, untouchable by defn |

## Mutation benches

| Run | Date | Workload | Fixture size | Correct (defn) | Input Δ | Notes |
|---|---|---|---|---|---|---|
| [rename-sweep](./mutations/2026-07-10-rename-sweep.md) | 2026-07-10 | rename-param, 7 sizes × 2 samples | 10 – 800 LOC | **14/14** | ~0% (noise) | **null result** — no crossover found; kills the crossover-curve marketing move |
| [chains-v9](./mutations/2026-07-10-chains-v9.md) | 2026-07-10 | multi-op chain | 3-file / ~25 LOC | **2/2** | −28% | first clean chain run — stale-binary bug fixed in bench harness |
| [chains-v4](./mutations/2026-07-08-chains-v4.md) | 2026-07-08 | multi-op chain | 3-file / ~25 LOC | 1/2 | — | **superseded** — see erratum on the doc; numbers invalid |
| [chains-v1](./mutations/2026-07-08-chains-v1.md) | 2026-07-08 | multi-op chain | 3-file / ~25 LOC | — | — | first cut — shared scratch dir, superseded |
| [v3](./mutations/2026-07-08-v3.md) | 2026-07-08 | single-op | 5 small + 3 big | **8/8** | ~0% | mutation harness where the 25k headless floor bites |
| [baseline](./mutations/2026-07-07-baseline.md) | 2026-07-07 | single-op | 5 small | **5/5** | not captured | wall-time only; superseded by v3 |

**Δ semantics:** "Input Δ" is the change in agent input tokens vs
files-mode on the *same* case. Negative = defn cheaper. A single
number here is a summary; per-case tables live in each linked doc.

## Reading the shape (the honesty caveats)

Three things every citation of a number here should carry:

1. **`claude -p` has a ~25k input-token per-invocation floor.** Base
   preamble, system context, and tool schemas take that much before
   any workload runs. Small fixtures can't clear it. Comparisons that
   don't disclose the floor mislead.

2. **Small fixtures LOSE.** On ~10-line files, defn frequently *adds*
   tool calls (agent uses more surgical ops than the naive Read+Edit
   chain). The crossover is around 50 lines / multi-file work. Any
   marketing-shape claim that hides the small-fixture losses is
   dishonest. See v3's `big-replace-slice` row for where the delta
   finally bites.

3. **Workload class matters more than aggregate.** The single-op
   mutation bench is the *wrong* place to shop for defn's win — its
   per-case cost is dominated by the 25k floor. Multi-op chains and
   read-side retrieval are where the delta actually shows.

## The five moves

1. **Show your receipts.** Every claim links to a re-runnable script
   and the exact table. This page is that.
2. **Gate on correctness.** "Correct-tokens-per-edit" — tokens spent
   only on runs that produced the right output. A chain-bench that
   scores 30% input savings alongside a 50% correctness drop scores
   **zero** on this metric. The v4 doc's numbers are precisely that
   shape and are marked invalid.
3. **Ship the audit workflow as a tool.** `defn bench --your-repo`
   should let users verify our claim on their own code. Not built
   yet; tracked as marketing artifact #3.
4. ~~**Show crossover, not peak.**~~ **RETIRED 2026-07-10 after a
   null-finding sweep.** See
   [`mutations/2026-07-10-rename-sweep.md`](./mutations/2026-07-10-rename-sweep.md):
   no input-token crossover exists on single-op mutations at
   LOC ≤ 800; the ~25k `claude -p` headless floor dominates any
   fixture-size effect. The workload class was wrong — defn's
   token wins live in multi-op multi-file chains and codebase-wide
   read-side retrieval, not single-op edits. Replacement work:
   scale the chain-bench and instrument the retrieval bench for
   input-token measurement.
5. **Meet the real production baseline: OpenHands, not `claude -p`.**
   Our early benches used `claude -p` — which has Grep + Read tools
   built in — as the "no-defn" baseline. That's a very strong
   baseline. Real production agent traffic runs against baselines
   with a handful of tools and no dedicated code lookup: e.g.
   OpenHands' five-tool set (`execute_bash`, `str_replace_editor`,
   `think`, `finish`, `task_tracker`). On 1,991 real successful
   OpenHands trajectories analyzed, 28.9% of bash calls were
   grep/find/ls and 61% of editor calls were `view` — an estimated
   15–25% of tool calls per trajectory would collapse under a code
   graph. See
   [`trajectories/2026-07-11-openhands-analysis.md`](./trajectories/2026-07-11-openhands-analysis.md).
   Enterprise cost-per-refactor argument still applies but grounds in
   this data instead of small-N synthetic runs.

## What we won't do

- Headline a "defn saves N%" without workload class.
- Screenshot a favorable per-case row without the surrounding table.
- Compare against artificially bad baselines (naive grep, Read-then-
  Edit-repeatedly loops that no competent agent uses).
- Cite a number without a linked receipt in this directory.
- Cite a bench-level number that hasn't cleared correctness.

## Reproducing

```bash
# All mutation cases:
go run ./cmd/defn-bench --mutations

# Multi-op chains only:
go run ./cmd/defn-bench --chains-only

# Retrieval:
go run ./bench/retrieval

# Fixture-size sweep — one mutation at 7 fixture sizes, writes CSV
# for the crossover plot (real token spend; ~15 min at samples=2):
go run ./cmd/defn-bench --size-sweep --samples 2 --size-sweep-csv sweep.csv

# Audit defn on YOUR OWN repo — read-side, no repo modification:
go run ./cmd/defn-bench --your-repo /path/to/your/module --task "who calls X?"
```

The bench harness now builds a fresh `./defn` at startup by default
(opt out with `DEFN_BENCH_NO_REBUILD=1`); log line at the top of
every run states which binary is in use and its mtime.
