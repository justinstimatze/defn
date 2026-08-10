# prometheus/prometheus head-to-head: defn vs files-mode

15 hand-curated real prometheus/prometheus bug-fix tasks (issue + linked
merged-PR fix), run through both arms live on 2026-08-09/10 with
`claude -p --model sonnet`, `--budget-usd 3.0 --max-turns 50` per task.

Harness bug found and fixed mid-run: `setup_workspace` in
`bench/head-to-head-go/agent_driver.py` ran `defn init`/`defn ingest`
unconditionally, so the files-mode arm's workdir got a defn-authored
CLAUDE.md telling it to use a tool (`mcp__defn__code`) that arm's
allowlist doesn't grant. All results below are from the fixed harness
(files-mode workdirs verified clean: repo's own pristine `CLAUDE.md`,
no `.defn/`).

## Aggregate (n=15 per arm)

| | defn | files |
|---|---:|---:|
| total cost | $11.90 | $9.88 |
| mean cost | $0.79 | $0.66 |
| median cost | $0.64 | $0.49 |
| mean wall time | 395s | 331s |
| rc==0 | 13/15 | 13/15 |

**Cost ratio (defn/files): 1.20x — defn costs 20% more on this batch.**
This does not meet CLAUDE.md's "parity is the floor" bar. Real-workload
result, not a synthetic sweep.

## Per-task

| instance | defn $ | defn s | defn rc | files $ | files s | files rc |
|---|---:|---:|---:|---:|---:|---:|
| prometheus-12024 | 1.63 | 638 | 0 | 1.74 | 452 | 1 |
| prometheus-16766 | 0.13 | 58 | 0 | 0.51 | 193 | 0 |
| prometheus-17395 | 1.22 | 624 | 0 | 0.32 | 90 | 0 |
| prometheus-18358 | 0.46 | 179 | 0 | 0.70 | 408 | 0 |
| prometheus-18534 | 1.30 | 498 | 0 | 1.59 | 569 | 1 |
| prometheus-18652 | 0.47 | 205 | 0 | 0.85 | 517 | 0 |
| prometheus-18712 | 1.07 | 375 | 1 | 0.31 | 233 | 0 |
| prometheus-18765 | 1.35 | 954 | 1 | 1.16 | 307 | 0 |
| prometheus-18841 | 0.10 | 67 | 0 | 0.17 | 211 | 0 |
| prometheus-18972 | 0.51 | 859 | 0 | 0.43 | 389 | 0 |
| prometheus-19017 | 0.64 | 263 | 0 | 0.37 | 405 | 0 |
| prometheus-19114 | 0.23 | 178 | 0 | 0.18 | 142 | 0 |
| prometheus-19184 | 1.32 | 495 | 0 | 0.49 | 108 | 0 |
| prometheus-19236 | 0.35 | 153 | 0 | 0.62 | 792 | 0 |
| prometheus-19338 | 1.12 | 381 | 0 | 0.43 | 146 | 0 |

`rc!=0` is a budget/turn-cap exhaustion, not necessarily a crash — not
yet cross-checked against whether the fix actually landed (correctness
scoring not run yet, only cost/completion).

## Not yet done

- Correctness scoring (does the produced diff actually fix the issue,
  per each task's `fix_patch`) — only cost/wall-time/rc measured so far.
- Digging into per-task swings (defn wins 18358/18712(rc)/19017/19114/
  19236/19338 on cost; files wins the rest, several by 2-4x) to find a
  root cause rather than reporting the aggregate as the whole story.
- Opus-based rerun, per standing instruction to use Opus for the "real"
  comparison later (Sonnet used here as the interim/cheaper pass).

## Data

Raw trajectories: `arm_defn/<instance_id>.json`, `arm_files/<instance_id>.json`
(same `fncall_messages` schema as the existing cli/grpc-go/go-zero corpus).
Use `bench/head-to-head-go/render_comparison.py <instance_id> --files
arm_files/<id>.json --defn arm_defn/<id>.json` for a side-by-side viewer.
