# head-to-head-go

First defn-legitimate wire-cost benchmark on Go.

Two arms on the same task set:

- **files-mode baseline** — recorded from Multi-SWE-bench_trajs MopenHands
  Claude-3.7-Sonnet on Go instances (cli/grpc-go/go-zero). Wire cost
  already computed by `select_tasks.py` from the recorded trajectories.
- **defn-mode arm** — Claude on the same problem_statement + repo at the
  same base_commit, but with the `code` MCP tool wired in and files-mode
  tools removed. Trajectory recorded, wire cost computed identically.

Motivated by the Go recon
(`scratchpad/recon_go.py`, memory: `project_go_recon_2026_07_18.md`):
- 59.9% whole-file view rate → outline/overview should win reads
- 47% bash defn-substitutable → search/find should win discovery
- 52.3% replace-hunk realistic savings → write-side already measured
  as shape claim; live confirmation belongs here

## Layout

- `select_tasks.py` — extracts N tasks from Multi-SWE-bench_trajs +
  main, emits `tasks.jsonl` with baseline wire cost per task
- `tasks.jsonl` — 10 tasks, one JSON record per line
- `run_defn_arm.sh` — per-task defn-mode agent runner (skeleton)
- `analyze.py` — compares files-mode baseline vs defn-mode arm outputs

## Task record schema

```json
{
  "instance_id": "cli__cli-10009",
  "org": "cli", "repo": "cli",
  "base_commit_sha": "8da27d2c8ac8b781cf34a5e04ed57cfe4b68fa55",
  "base_commit_ref": "trunk",
  "problem_statement": "Artifact download path traversal check fails on valid path...",
  "fix_patch": "diff --git a/... (gold patch)",
  "baseline_files_mode": {
    "tool_calls": 30,
    "input_bytes": 12345,
    "output_bytes": 78901,
    "view_calls": 8,
    "edit_calls": 4,
    "edit_bytes": 2345,
    "view_bytes": 65432,
    "files_touched": ["/workspace/cli__cli__0.1/pkg/cmd/run/download/zip.go"]
  },
  "baseline_traj_path": "/tmp/.../claude37/cli__cli-10009/Claude-3.7-Sonnet-....json"
}
```

## Running the defn arm (per task)

Not fully automated yet. Manual steps for one task:

```bash
# 1. Clone repo at base_commit
task_id=cli__cli-10009
sha=$(jq -r 'select(.instance_id=="'$task_id'").base_commit_sha' tasks.jsonl)
git clone https://github.com/cli/cli /tmp/bench-workdir/$task_id
cd /tmp/bench-workdir/$task_id && git checkout $sha

# 2. Ingest with defn
defn ingest .
defn serve &          # background; deterministic port

# 3. Run Claude with defn MCP + problem_statement
#    (not yet wired — see `run_defn_arm.sh` for the shape)
```

## What still needs building

- **Agent invocation.** Programmatic Claude call with defn MCP wired
  and files-mode tools disabled, using the problem_statement as the
  initial user message. Options: `claude -p` in a subprocess with an
  MCP-only config, or Anthropic API directly.
- **Correctness scoring.** Two approximations:
  1. Cheap: did the model touch the same files as the baseline?
  2. Real: apply the model's patch to a clean repo, run
     `fixed_tests` from the Multi-SWE-bench record, check pass/fail.
- **Sandbox.** Real correctness needs the Multi-SWE-bench docker
  images (repo-specific test envs). See
  https://github.com/multi-swe-bench/multi-swe-bench.

## Comparison metric

For each task, compute:
- **input tokens** (agent → tools payload)
- **output tokens** (tools → agent payload)
- **total wire cost**
- **tool calls**
- **correctness**

Aggregate: geomean delta per metric, plus per-task loss/win histogram.
Discipline: report the loss cases loudly.

## Baseline totals (n=10)

- Tool calls: 312
- Input bytes: 197,431
- Output bytes: 823,449
- **Total wire: 1,020,880 bytes**
- Edit calls: 101 · View calls: 52
