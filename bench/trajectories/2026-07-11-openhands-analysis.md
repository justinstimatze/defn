# 2026-07-11 — OpenHands trajectory analysis

**Workload class:** static analysis of external agent trajectories, not
a defn run. Every number here is an *estimate of what defn would do* on
someone else's trajectory, not a measured defn execution.

**Dataset:** [`nebius/SWE-rebench-openhands-trajectories`](https://huggingface.co/datasets/nebius/SWE-rebench-openhands-trajectories).
67,074 trajectories from Qwen3-Coder-480B running under OpenHands v0.54.0
on real GitHub issues from `nebius/SWE-rebench`. 32,161 successful.
Avg 64 turns per trajectory. All Python. **We assume the code is Go**;
the mapping from Python bash/editor actions to Go defn ops is
conceptual, not language-verified.

## Why this dataset

OpenHands exposes only 5 tools to the agent: `execute_bash`, `think`,
`finish`, `task_tracker`, `str_replace_editor`. No Grep tool, no
dedicated Read tool. Every "find X" is a shell grep; every function
view is a str_replace_editor `view` with a line range. This is the
regime where a graph is expected to pay — and it's the real production
baseline for many agents, not a synthetic stripped-down comparison.

`claude -p` (the baseline in `bench/retrieval-llm/`) has a Grep tool
and Read with narrow ranges built in — it's a much stronger baseline
than OpenHands. That's why our earlier retrieval bench showed near-
parity: we were racing against an already-tuned baseline. Real
production OpenHands trajectories are the workload where the graph
should show.

## Aggregate profile: 1,991 successful trajectories (row group 0)

| Tool | Total calls | % |
|---|---:|---:|
| `execute_bash` | 60,517 | 51.7% |
| `str_replace_editor` | 48,105 | 41.1% |
| `think` | 5,154 | 4.4% |
| `finish` | 1,926 | 1.6% |
| `task_tracker` | 1,403 | 1.2% |
| **Total** | **117,105** | 100% |

Bash breakdown (top classes):

| Bash kind | Count | Note |
|---|---:|---|
| `python` / `python+pytest` | ~35,000 | test runs, defn does not collapse |
| **grep / find / rg / ls** | **17,486 (28.9% of bash)** | one defn op each |
| `git` | 2,307 | |
| `rm` / `mv` / etc. | ~2,000 | |

Editor breakdown:

| Editor op | Count | % |
|---|---:|---:|
| `view` (read) | 29,423 | 61.2% |
| `create` | 10,324 | 21.5% |
| `str_replace` | 8,353 | 17.4% |
| `undo_edit` | 5 | — |

Average trajectory: **58.8 tool calls**. Median ~50. Compare to our
`bench/retrieval-llm/` benched trajectories (~2–17 calls per question) —
real production trajectories are **an order of magnitude longer**, and
that's where the graph exploration fat lives.

## Per-trajectory labeled walk-through: `encode__starlette-1240`

Task: add `|` and `|=` operators to `MutableHeaders`. 39 tool calls.
Full labeled walk in the session transcript. Summary:

| Category | Count | Defn treatment |
|---|---:|---|
| Eliminated by defn (grep-then-view of same lines) | 8 | 0 calls |
| Collapsed to 1 defn op (grep+multi-view → `code(op:"read")`/`impact`) | 5 | 1 call each |
| Same 1:1 cost (single reads, single edits) | 2 | 2 calls |
| No defn equivalent (tests, reproducers, thinks, git) | 24 | 24 calls |

Hand-labeled estimate: **39 → 26 calls, −33%**.

## Auto-heuristic on 4 varied trajectories

Heuristic used (matches hand-label to within 5–10 pp): `search-bash`
50% eliminated / 50% kept, `editor-view` 40% eliminated / 60% kept as
`code(op:"read")`, everything else unchanged.

| Trajectory | Total | Est. defn | Δ |
|---|---:|---:|---:|
| starlette (bug: `MutableHeaders \|=`) | 39 | 32 | −18% |
| graphene | 31 | 27 | −13% |
| WrightTools | 66 | 54 | −18% |
| pre-commit | 91 | 79 | −13% |
| **Sum** | **227** | **192** | **−15.4%** |

Hand-labeling starlette got −33%; auto-heuristic got −18% on the same
trajectory. The truth is somewhere between — the auto-heuristic can't
see "grep-then-view of lines I already had" as pure waste. Honest
range: **−15% to −25% call reduction**.

## Second-order effect: bytes-per-call

Not captured in the call count. `view file range=[580,680]` returns
~100 lines including neighbors. `code(op:"read", name:"Foo")` returns
just the def body — typically 5–30 lines. On the ~30% of calls that
are views, defn probably delivers 50–70% smaller output per read.

Under multiplicative math: 25% fewer calls × 40% smaller reads on 30%
of calls ≈ **20–40% total input token reduction**. This is a rough
projection, not a measured number.

## What this analysis is NOT

- A defn run. No tokens were spent. No code was executed.
- A benchmark. No pass/fail. No competitive comparison.
- Language-verified. All Python; graph-collapsibility mapped
  conceptually to Go defn ops.
- Beyond n=4 hand analysis + aggregate on n=1,991.

## What this analysis IS

- Evidence that a real production baseline (OpenHands with 5 tools,
  no dedicated Read/Grep) leaves 15–25% of tool calls on the table
  that a code-graph would eliminate.
- Evidence that the win is *not* 5–10x. The 40–50% test-run tail is
  untouchable and dominates the trajectory.
- A defensible next-bench design: replay one trajectory with defn,
  count real tokens, compare.

## Followups

- **Actually replay one trajectory with defn** to convert the estimate
  to a measured number. Best candidate: starlette-1240 (39 calls,
  small enough to translate manually into a defn prompt, well-scoped
  bug fix). Cost: 2–3 hours, N=1.
- Widen sample to include the failed trajectories too — do failed
  runs have a *higher* graph-collapsibility fraction? (If defn would
  have avoided the loops that caused failure, that's a correctness
  angle, not just efficiency.)
- Cross-tab by repo size — does defn's win grow with repo size?
- Sanity check on other 16 row groups (this is only row group 0).

## Data

- Parquet at `~/.cache/huggingface/hub/datasets--nebius--SWE-rebench-openhands-trajectories/`
- Analysis scripts (throwaway) in `/tmp/claude-1001/.../scratchpad/`
- Row group 0: 4,096 trajectories, 1,991 resolved
