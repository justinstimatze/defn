#!/usr/bin/env python3
"""
score_correctness.py — cheap correctness approximation for the defn arm.

The "real" correctness check is running the Multi-SWE-bench per-repo test
docker image against the arm's produced patch. That's out-of-scope here
(requires the multi-swe-bench harness with per-repo Dockerfiles).

Instead, we score by:
  - files_precision: (defn-arm-edited ∩ gold-patch-edited) / defn-arm-edited
  - files_recall:    (defn-arm-edited ∩ gold-patch-edited) / gold-patch-edited
  - files_f1:        harmonic mean

This is a rough approximation but catches the "did the agent hit the right
files" question — the necessary condition for a real fix. Loud caveat: an
arm that touches all the right files can still fail the actual tests; and
a novel-but-correct fix in different files would score 0 here.

Usage:
  python3 score_correctness.py [--arm-dir arm_defn]
"""

import argparse
import json
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
WORKDIR_ROOT = "/tmp/defn-h2h-go"


def gold_files(fix_patch):
    """Files touched by the gold fix_patch (unified diff, `diff --git a/... b/...`)."""
    files = set()
    if not fix_patch:
        return files
    for line in fix_patch.splitlines():
        m = re.match(r"^diff --git a/(\S+) b/(\S+)", line)
        if m:
            files.add(m.group(2))
    return files


def normalize_path(p, prefix_workdir):
    """Strip absolute workdir prefix + leading slash so path is repo-relative."""
    if not p:
        return p
    if p.startswith(prefix_workdir):
        p = p[len(prefix_workdir) :].lstrip("/")
    # Baselines often use /workspace/repo__ver/... prefixes; strip that too.
    p = re.sub(r"^/workspace/[^/]+/", "", p)
    return p.lstrip("/")


_SAFE_DEFNAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_.]*$")


def resolve_defname_to_file(name, workdir):
    """Ask defn where a named def lives. Returns list of candidate paths
    (possibly multiple — same def name across packages) or empty list."""
    if not name or not workdir or not os.path.isdir(os.path.join(workdir, ".defn")):
        return None
    # `name` comes from agent trajectory tool_call args. Reject anything
    # that isn't a plain Go identifier (or dotted receiver form) to avoid
    # SQL injection via the f-string interpolation below. `defn query`
    # accepts raw SQL so we cannot rely on it to parameterize.
    if not _SAFE_DEFNAME.match(name):
        return []
    try:
        out = subprocess.check_output(
            [
                "defn",
                "query",
                f"SELECT DISTINCT source_file FROM definitions WHERE name = '{name}'",
            ],
            cwd=workdir,
            text=True,
            stderr=subprocess.DEVNULL,
            timeout=10,
        )
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
        return []
    # defn query emits JSON: [{"source_file": "path/to/file.go"}, ...]
    try:
        rows = json.loads(out)
        if isinstance(rows, list):
            return [r["source_file"] for r in rows if r.get("source_file")]
    except (ValueError, KeyError, IndexError, TypeError):
        pass
    return []


def arm_touched_files(arm_data, workdir_hint):
    """Extract repo-relative paths the defn arm modified (via code tool ops
    or bash write commands). Best effort — normalize what we can."""
    touched = set()
    for msg in arm_data.get("fncall_messages", []):
        if msg.get("role") != "assistant":
            continue
        for tc in msg.get("tool_calls") or []:
            fn = tc.get("function", {})
            nm = fn.get("name", "")
            try:
                args = json.loads(fn.get("arguments") or "{}")
            except Exception:
                continue
            if nm.endswith("__code"):
                op = args.get("op", "")
                if op in (
                    "edit",
                    "create",
                    "insert-precondition",
                    "replace-slice",
                    "replace-hunk",
                    "wrap-in-defer",
                    "rename-param",
                    "add-import",
                    "insert",
                    "delete",
                    "rename",
                    "move",
                ):
                    f = args.get("file") or args.get("path")
                    if f:
                        touched.add(normalize_path(f, workdir_hint))
                    else:
                        for f in resolve_defname_to_file(
                            args.get("name"), workdir_hint
                        ):
                            touched.add(normalize_path(f, workdir_hint))
                elif op == "apply":
                    for sub in args.get("operations", []):
                        f = sub.get("file") or sub.get("path")
                        if f:
                            touched.add(normalize_path(f, workdir_hint))
                        else:
                            for f in resolve_defname_to_file(
                                sub.get("name"), workdir_hint
                            ):
                                touched.add(normalize_path(f, workdir_hint))
            # Bash-shape writes (rare, but possible): sed -i / echo > / tee
            if nm == "Bash":
                cmd = args.get("command", "") or ""
                for m in re.finditer(r"(?:tee|>|>>|sed -i)\s+(\S+\.go)", cmd):
                    touched.add(normalize_path(m.group(1), workdir_hint))
    return touched


def git_touched_files(workdir):
    """Fall-back: whatever git status says was modified after the run."""
    try:
        out = subprocess.check_output(
            ["git", "-C", workdir, "status", "--porcelain"], text=True
        )
    except subprocess.CalledProcessError:
        return set()
    files = set()
    for line in out.splitlines():
        m = re.match(r"^..\s+(.+)$", line)
        if m:
            files.add(m.group(1).strip())
    return files


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arm-dir", default=os.path.join(HERE, "arm_defn"))
    args = ap.parse_args()

    tasks_by_id = {}
    with open(os.path.join(HERE, "tasks.jsonl")) as f:
        for line in f:
            r = json.loads(line)
            tasks_by_id[r["instance_id"]] = r

    rows = []
    for inst_id, task in tasks_by_id.items():
        arm_path = os.path.join(args.arm_dir, inst_id + ".json")
        if not os.path.exists(arm_path):
            continue
        arm = json.load(open(arm_path))
        workdir = arm.get("workdir") or os.path.join(WORKDIR_ROOT, inst_id)
        gold = gold_files(task.get("fix_patch") or "")
        # Parse arm tool calls — git-status includes formatting churn from
        # `defn emit` and would over-report. Fall back to git-status only
        # if we can't extract any file paths from the trajectory.
        touched = arm_touched_files(arm, workdir) or git_touched_files(workdir)
        hit = gold & touched
        prec = len(hit) / len(touched) if touched else 0.0
        rec = len(hit) / len(gold) if gold else 0.0
        f1 = (2 * prec * rec / (prec + rec)) if (prec + rec) else 0.0
        rows.append(
            {
                "id": inst_id,
                "gold": sorted(gold),
                "touched": sorted(touched),
                "hit": sorted(hit),
                "precision": prec,
                "recall": rec,
                "f1": f1,
                "cost": arm.get("cost_usd"),
            }
        )

    if not rows:
        print("no arm outputs to score", file=sys.stderr)
        sys.exit(0)

    print(f"=== correctness (files-touched approximation) ===")
    print(
        f"  {'instance':30s}  {'P':>5s} {'R':>5s} {'F1':>5s}  gold  touched  hit  cost"
    )
    for r in rows:
        cost = f"${r['cost']:.3f}" if r["cost"] else "-"
        print(
            f"  {r['id']:30s}  {r['precision']:>5.2f} {r['recall']:>5.2f} {r['f1']:>5.2f}  "
            f"{len(r['gold']):>4}  {len(r['touched']):>7}  {len(r['hit']):>3}  {cost}"
        )

    import statistics

    print(f"\n=== AGGREGATE ({len(rows)} tasks) ===")
    print(f"  mean precision: {statistics.mean(r['precision'] for r in rows):.3f}")
    print(f"  mean recall:    {statistics.mean(r['recall'] for r in rows):.3f}")
    print(f"  mean F1:        {statistics.mean(r['f1'] for r in rows):.3f}")
    hits = sum(1 for r in rows if r["f1"] >= 0.5)
    print(f"  F1 >= 0.5: {hits}/{len(rows)}")
    total_cost = sum(r["cost"] or 0 for r in rows)
    print(f"  total cost:  ${total_cost:.2f}")


if __name__ == "__main__":
    main()
