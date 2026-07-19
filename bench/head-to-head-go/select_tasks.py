#!/usr/bin/env python3
"""
Select N Multi-SWE-bench Go tasks with a matched files-mode baseline
from Multi-SWE-bench_trajs (MopenHands Claude-3.7-Sonnet).

Emits `tasks.jsonl` — one record per task with:
  - instance_id, repo, base_commit_sha, problem_statement, fix_patch
  - baseline_files_mode: {tool_calls, input_bytes, output_bytes,
    edit_bytes, view_bytes, view_calls, edit_calls, files_touched}

Usage:
  python3 select_tasks.py [N]     # default N=10
"""

import glob
import json
import os
import re
import sys
from collections import defaultdict

TRAJ_ROOT = "/tmp/claude-1001/-home-justin-Documents-defn/69d9282b-ace7-4b5c-9954-9bdbda8d8860/scratchpad/mswe-go/claude37"
MAIN_ROOT = "/tmp/claude-1001/-home-justin-Documents-defn/69d9282b-ace7-4b5c-9954-9bdbda8d8860/scratchpad/mswe-go/main/go"
OUT = os.path.join(os.path.dirname(__file__), "tasks.jsonl")

N = int(sys.argv[1]) if len(sys.argv) > 1 else 10

# ---- Load main dataset (instance_id -> metadata) ----
main_by_id = {}
for jsonl in glob.glob(f"{MAIN_ROOT}/*_dataset.jsonl"):
    with open(jsonl) as f:
        for line in f:
            r = json.loads(line)
            main_by_id[r["instance_id"]] = r
print(f"main-dataset instances: {len(main_by_id)}", file=sys.stderr)

# ---- Trajectory files ----
traj_files = sorted(glob.glob(f"{TRAJ_ROOT}/*/Claude*.json"))
print(f"trajectory files: {len(traj_files)}", file=sys.stderr)


def wire_cost(traj):
    """Compute bytes shipped by the files-mode agent, split by shape."""
    input_bytes = 0  # tool_call arguments (agent -> tools)
    output_bytes = 0  # tool result contents (tools -> agent)
    tool_calls = 0
    view_calls = 0
    edit_calls = 0
    edit_bytes = 0
    view_bytes = 0
    files_touched = set()
    for msg in traj:
        role = msg.get("role")
        if role == "assistant":
            for tc in msg.get("tool_calls") or []:
                tool_calls += 1
                fn = tc.get("function", {})
                args_raw = fn.get("arguments") or ""
                input_bytes += len(args_raw)
                try:
                    args = json.loads(args_raw)
                except Exception:
                    continue
                if fn.get("name") == "str_replace_editor":
                    cmd = args.get("command", "")
                    path = args.get("path", "")
                    if cmd == "view":
                        view_calls += 1
                    elif cmd == "str_replace":
                        edit_calls += 1
                        edit_bytes += len(args.get("old_str", "")) + len(
                            args.get("new_str", "")
                        )
                        if path:
                            files_touched.add(path)
                    elif cmd == "create":
                        edit_calls += 1
                        edit_bytes += len(args.get("file_text", ""))
                        if path:
                            files_touched.add(path)
        elif role == "tool":
            c = msg.get("content", "")
            if isinstance(c, list):
                c = "".join(
                    x.get("text", "") if isinstance(x, dict) else str(x) for x in c
                )
            output_bytes += len(c or "")
            # View outputs are the dominant read-side cost; approximate
            # by attributing to previous view call if content starts with
            # the SWE-agent view-response marker.
            if isinstance(c, str) and (
                c.startswith("Here's the result of running `cat -n`")
                or "Here's the result" in c[:100]
            ):
                view_bytes += len(c)
    return {
        "tool_calls": tool_calls,
        "input_bytes": input_bytes,
        "output_bytes": output_bytes,
        "view_calls": view_calls,
        "edit_calls": edit_calls,
        "edit_bytes": edit_bytes,
        "view_bytes": view_bytes,
        "files_touched": sorted(files_touched),
    }


def extract_problem_statement(traj):
    for m in traj:
        if m.get("role") == "user":
            c = m.get("content", "")
            if isinstance(c, str) and "<issue_description>" in c:
                match = re.search(
                    r"<issue_description>(.*?)</issue_description>", c, re.DOTALL
                )
                if match:
                    return match.group(1).strip()
                return c
    return None


# ---- Score every trajectory, pair with main ----
scored = []
for fp in traj_files:
    inst_dir = fp.split("/")[-2]
    if inst_dir not in main_by_id:
        continue
    try:
        d = json.load(open(fp))
    except Exception:
        continue
    traj = d.get("fncall_messages") or d.get("messages") or []
    wc = wire_cost(traj)
    if wc["edit_calls"] == 0:
        continue  # skip tasks the agent didn't complete edits on
    ps = extract_problem_statement(traj)
    if not ps:
        continue
    scored.append(
        {
            "instance_id": inst_dir,
            "wire_cost": wc,
            "problem_statement": ps,
            "traj_path": fp,
        }
    )

print(f"trajectories with edits + problem statement: {len(scored)}", file=sys.stderr)


# ---- Rank by "interesting" — enough work to matter, not too big ----
def size_score(s):
    tc = s["wire_cost"]["tool_calls"]
    # Prefer 15-60 tool calls: enough work, not runaway
    if tc < 8:
        return -100
    if tc > 150:
        return -50
    return -abs(tc - 30)


scored.sort(key=size_score, reverse=True)

# Diversity: at least one from each repo
selected = []
by_repo = defaultdict(int)
for s in scored:
    if len(selected) >= N:
        break
    inst = s["instance_id"]
    # instance_id format: cli__cli-10009 -> repo = cli__cli
    repo = inst.rsplit("-", 1)[0]
    if by_repo[repo] >= max(2, N // 3):
        continue
    selected.append(s)
    by_repo[repo] += 1

# Fill remainder without repo cap
if len(selected) < N:
    have = {s["instance_id"] for s in selected}
    for s in scored:
        if len(selected) >= N:
            break
        if s["instance_id"] not in have:
            selected.append(s)
            have.add(s["instance_id"])

print(f"selected: {len(selected)}", file=sys.stderr)
print(
    "  by repo:",
    dict(
        (r, sum(1 for s in selected if s["instance_id"].startswith(r)))
        for r in {s["instance_id"].rsplit("-", 1)[0] for s in selected}
    ),
    file=sys.stderr,
)

# ---- Emit tasks.jsonl ----
with open(OUT, "w") as f:
    for s in selected:
        m = main_by_id[s["instance_id"]]
        rec = {
            "instance_id": s["instance_id"],
            "org": m.get("org"),
            "repo": m.get("repo"),
            "base_commit_sha": m.get("base", {}).get("sha"),
            "base_commit_ref": m.get("base", {}).get("ref"),
            "problem_statement": s["problem_statement"],
            "fix_patch": m.get("fix_patch"),  # gold
            "baseline_files_mode": s["wire_cost"],
            "baseline_traj_path": s["traj_path"],
        }
        f.write(json.dumps(rec) + "\n")

print(f"wrote {OUT}")

# ---- Summary ----
total_calls = sum(s["wire_cost"]["tool_calls"] for s in selected)
total_input = sum(s["wire_cost"]["input_bytes"] for s in selected)
total_output = sum(s["wire_cost"]["output_bytes"] for s in selected)
total_edits = sum(s["wire_cost"]["edit_calls"] for s in selected)
total_views = sum(s["wire_cost"]["view_calls"] for s in selected)
print(f"\nfiles-mode baseline totals across {len(selected)} tasks:")
print(f"  tool calls: {total_calls}")
print(f"  input (agent->tools):  {total_input:>10,} bytes")
print(f"  output (tools->agent): {total_output:>10,} bytes")
print(f"  total wire cost:       {total_input + total_output:>10,} bytes")
print(f"  edit calls: {total_edits}   view calls: {total_views}")
