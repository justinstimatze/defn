#!/usr/bin/env python3
"""
score_gitdiff.py — score defn-vs-files correctness by diffing the
actual on-disk workdir against base_commit_sha, instead of parsing
tool-call arguments (regex-based Bash-command parsing missed bulk
`grep -rl ... | xargs sed -i ...`-style edits entirely -- see the
2026-08-17 caddy-1676 finding). git diff --name-only catches every
edit mechanism (Edit tool, sed, python, defn's own file writes)
uniformly and correctly, for both arms.

Usage: score_gitdiff.py <repo_dir> <tasks_file> <arm_defn_dir> <arm_files_dir> [label]
"""
import json
import os
import re
import subprocess
import sys
import statistics

WORKDIR_ROOT = os.path.expanduser("~/.cache/defn-h2h-go")

# defn's own project-onboarding files, written by `defn init`/`defn ingest`
# on every run regardless of the task -- confirmed via direct reproduction
# (fresh init+ingest on etcd, before any agent turn: only .gitignore was
# modified). Counting these as "touched files" penalizes defn's precision
# for setup that has nothing to do with the specific task. The files arm
# never writes these, so this exclusion is defn-arm-only in practice but
# applied uniformly for correctness.
DEFN_SETUP_FILES = {".gitignore", ".mcp.json", ".mcp-defn-only.json", "CLAUDE.md"}
DEFN_SETUP_PREFIXES = (".codex/", ".claude/")


def is_defn_setup_file(path):
    if path in DEFN_SETUP_FILES:
        return True
    return any(path.startswith(p) for p in DEFN_SETUP_PREFIXES)


def gold_files(fix_patch):
    files = set()
    for line in (fix_patch or "").splitlines():
        m = re.match(r"^diff --git a/(\S+) b/(\S+)", line)
        if m:
            files.add(m.group(2))
    return files


def workdir_touched_files(instance_id, arm, base_sha):
    wd = os.path.join(WORKDIR_ROOT, f"{instance_id}__{arm}")
    if not os.path.isdir(wd):
        return None, f"workdir missing: {wd}"
    try:
        out = subprocess.check_output(
            ["git", "-C", wd, "diff", "--name-only", base_sha],
            stderr=subprocess.STDOUT, text=True, timeout=30,
        )
    except subprocess.CalledProcessError as e:
        return None, f"git diff failed: {e.output[:200]}"
    touched = set(l.strip() for l in out.splitlines() if l.strip())
    touched = {f for f in touched if not is_defn_setup_file(f)}
    # Also pick up untracked new files (a create op / new file via sed/python).
    try:
        untracked = subprocess.check_output(
            ["git", "-C", wd, "ls-files", "--others", "--exclude-standard"],
            text=True, timeout=30,
        )
        for l in untracked.splitlines():
            l = l.strip()
            if l.endswith(".go"):
                touched.add(l)
    except subprocess.CalledProcessError:
        pass
    return touched, None


def score_arm(tasks_by_id, arm_dir_name, arm_key):
    rows = []
    for inst_id, task in tasks_by_id.items():
        arm_json = os.path.join(os.path.expanduser(f"~/{arm_dir_name}"), f"{inst_id}.json")
        if not os.path.exists(arm_json):
            continue
        arm = json.load(open(arm_json))
        gold = gold_files(task.get("fix_patch") or "")
        touched, err = workdir_touched_files(inst_id, arm_key, task["base_commit_sha"])
        if touched is None:
            rows.append({"id": inst_id, "error": err, "gold": sorted(gold)})
            continue
        hit = gold & touched
        prec = len(hit) / len(touched) if touched else 0.0
        rec = len(hit) / len(gold) if gold else 0.0
        f1 = (2 * prec * rec / (prec + rec)) if (prec + rec) else 0.0
        rows.append({
            "id": inst_id, "gold": sorted(gold), "touched": sorted(touched), "hit": sorted(hit),
            "precision": prec, "recall": rec, "f1": f1,
            "cost": arm.get("cost_usd"), "rc": arm.get("claude_rc"), "msgs": len(arm.get("fncall_messages", [])),
        })
    return rows


def print_rows(label, rows):
    print(f"\n=== {label} ===")
    ok_rows = [r for r in rows if "error" not in r]
    for r in rows:
        if "error" in r:
            print(f"  {r['id']:30s}  ERROR: {r['error']}")
            continue
        cost = f"${r['cost']:.3f}" if r["cost"] else "-"
        print(f"  {r['id']:30s}  P={r['precision']:.2f} R={r['recall']:.2f} F1={r['f1']:.2f}  "
              f"gold={len(r['gold'])} touched={len(r['touched'])} hit={len(r['hit'])} "
              f"msgs={r['msgs']} rc={r['rc']}  {cost}")
    if ok_rows:
        print(f"  mean F1: {statistics.mean(r['f1'] for r in ok_rows):.3f}  "
              f"mean P: {statistics.mean(r['precision'] for r in ok_rows):.3f}  "
              f"mean R: {statistics.mean(r['recall'] for r in ok_rows):.3f}")
        total_cost = sum(r["cost"] or 0 for r in ok_rows)
        print(f"  total cost: ${total_cost:.2f}  mean ${total_cost/len(ok_rows):.3f}/task")


def main():
    repo_dir = sys.argv[1]
    tasks_file = sys.argv[2]
    arm_defn_dir = sys.argv[3]
    arm_files_dir = sys.argv[4]
    label = sys.argv[5] if len(sys.argv) > 5 else os.path.basename(repo_dir)

    tasks_by_id = {}
    with open(tasks_file) as f:
        for line in f:
            r = json.loads(line)
            tasks_by_id[r["instance_id"]] = r

    defn_rows = score_arm(tasks_by_id, arm_defn_dir.lstrip("~/"), "defn")
    files_rows = score_arm(tasks_by_id, arm_files_dir.lstrip("~/"), "files")
    print_rows(f"{label} — defn arm", defn_rows)
    print_rows(f"{label} — files arm", files_rows)

    print(f"\n=== {label} — per-task diff ===")
    files_by_id = {r["id"]: r for r in files_rows}
    for r in defn_rows:
        fr = files_by_id.get(r["id"])
        if "error" in r or not fr or "error" in fr:
            continue
        delta = r["f1"] - fr["f1"]
        flag = "  <-- defn worse" if delta < -0.01 else ("  <-- defn better" if delta > 0.01 else "  (tie)")
        print(f"  {r['id']:30s} defn F1={r['f1']:.2f}  files F1={fr['f1']:.2f}{flag}")


if __name__ == "__main__":
    main()
