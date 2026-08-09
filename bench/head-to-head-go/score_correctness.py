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
# Must match agent_driver.py's WORKDIR_ROOT -- homedir, not /tmp, so these
# survive an EC2 stop/start between the agent run and a later scoring pass.
WORKDIR_ROOT = os.path.expanduser("~/.cache/defn-h2h-go")


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
_SAFE_RECEIVER = re.compile(r"^\*?[A-Za-z_][A-Za-z0-9_]*$")
_SAFE_PAREN_RECV = re.compile(
    r"^\(\*([A-Za-z_][A-Za-z0-9_]*)\)\.([A-Za-z_][A-Za-z0-9_]*)$"
)


def resolve_defname_to_file(name, workdir, receiver=None):
    """Ask defn where a named def lives. Returns list of candidate paths
    (possibly multiple — same def name across packages) or empty list.

    Without a receiver, a bare name query can silently over-match: e.g.
    grpc-go defines regeneratePicker in both balancer/grpclb/grpclb.go and
    an unrelated balancer/base/balancer.go, and virtually every
    balancer.Balancer implementation across the repo defines the same
    lifecycle method names (HandleSubConnStateChange, etc). An edit call
    that correctly disambiguated by receiver at the tool layer would still
    get every matching file counted as "touched" here, inflating the
    scored file-touch count (and tanking precision) for something the arm
    never actually wrote to. Found 2026-08-08 via a real grpc-go-2631
    trajectory: reported touched=12 for an edit that only landed in 2
    files, traced to this exact query never using the receiver the tool
    call actually specified.
    """
    if not name or not workdir or not os.path.isdir(os.path.join(workdir, ".defn")):
        return []
    # (*Type).Method: defn itself resolves this combined pointer-receiver
    # form in `name` just fine (some real trajectories call edit/apply
    # this way instead of separate name+receiver fields -- confirmed
    # 2026-08-08, grpc-go-2631), but the parens/asterisk fail the plain-
    # identifier gate below, so a correct edit was scored as a total miss
    # (0 touched files) despite defn having applied it correctly. Handle
    # this exact shape before the general identifier check, extracting
    # receiver/bare name straight from the regex groups (still injection-
    # safe -- fully anchored to identifier characters only).
    paren_match = _SAFE_PAREN_RECV.match(name)
    if paren_match:
        recv_type, bare = paren_match.groups()
        recv = "*" + recv_type
    else:
        # `name` comes from agent trajectory tool_call args. Reject anything
        # that isn't a plain Go identifier (or dotted receiver form) to avoid
        # SQL injection via the f-string interpolation below. `defn query`
        # accepts raw SQL so we cannot rely on it to parameterize.
        if not _SAFE_DEFNAME.match(name):
            return []
        # defn's actual `code` tool schema passes name and receiver as
        # separate JSON fields (e.g. {"name": "Pick", "receiver": "*lbPicker"}),
        # never as a single dotted "Receiver.Method" string -- prefer an
        # explicit receiver arg when the caller has one.
        recv = None
        bare = name
        if receiver and _SAFE_RECEIVER.match(receiver):
            recv = receiver
        elif "." in name:
            # Fallback for a dotted-name calling convention, in case some
            # other harness or agent shape uses it.
            recv, bare = name.rsplit(".", 1)
    if recv:
        sql = (
            "SELECT DISTINCT source_file FROM definitions "
            f"WHERE name = '{bare}' AND (receiver = '{recv}' "
            f"OR receiver = '*{recv}' OR receiver LIKE '%{recv}')"
        )
    else:
        sql = f"SELECT DISTINCT source_file FROM definitions WHERE name = '{name}'"
    try:
        out = subprocess.check_output(
            ["defn", "query", sql],
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


WRITE_OPS = (
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
    "apply",
)


def arm_write_count(arm_data):
    """Number of write ops the arm attempted (regardless of resolution).
    Used to distinguish 'no writes' (informational answer) from 'writes
    made but names didn't resolve' (partial trajectory extraction)."""
    n = 0
    for msg in arm_data.get("fncall_messages", []):
        if msg.get("role") != "assistant":
            continue
        for tc in msg.get("tool_calls") or []:
            fn = tc.get("function", {})
            if not fn.get("name", "").endswith("__code"):
                continue
            try:
                args = json.loads(fn.get("arguments") or "{}")
            except Exception:
                continue
            if args.get("op") in WRITE_OPS:
                n += 1
    return n


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
                            args.get("name"), workdir_hint, args.get("receiver")
                        ):
                            touched.add(normalize_path(f, workdir_hint))
                elif op == "apply":
                    for sub in args.get("operations", []):
                        f = sub.get("file") or sub.get("path")
                        if f:
                            touched.add(normalize_path(f, workdir_hint))
                        else:
                            for f in resolve_defname_to_file(
                                sub.get("name"), workdir_hint, sub.get("receiver")
                            ):
                                touched.add(normalize_path(f, workdir_hint))
            # files-mode arm: Edit/Write/MultiEdit are the actual write
            # tools, not a `__code` op — no over-reporting risk here (no
            # emit-reformat side effect), so this is safe to trust directly,
            # unlike git-status for the defn arm (see comment in main()).
            # Missing this was a real bug: files-mode scored 0 touched files
            # on every task before this fix, not because it didn't write
            # anything (git status showed real, focused edits) but because
            # nothing was parsing its tool calls at all.
            if nm in ("Edit", "Write", "MultiEdit"):
                f = args.get("file_path")
                if f:
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
        # Parse arm tool calls first. git-status includes formatting churn
        # from `defn emit` and over-reports; only fall back to it when the
        # arm didn't make ANY write ops (informational-answer case), so an
        # informational-answer scores 0/0. If writes were attempted but
        # resolve failed (partial trajectory extraction), keep the partial
        # set rather than fall back to the noisy git snapshot.
        n_writes = arm_write_count(arm)
        touched = arm_touched_files(arm, workdir)
        # No git-status fallback: `defn emit` re-formats every file in a
        # module after any DB mutation, so git status is not a reliable
        # source-of-truth for what the agent chose to edit. If the arm
        # made zero code writes, score as unattempted (0/0). Callers can
        # filter on n_writes to separate "made bad edits" from "gave up".
        hit = gold & touched
        prec = len(hit) / len(touched) if touched else 0.0
        rec = len(hit) / len(gold) if gold else 0.0
        f1 = (2 * prec * rec / (prec + rec)) if (prec + rec) else 0.0
        rows.append(
            {
                "id": inst_id,
                "n_writes": n_writes,
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
        f"  {'instance':30s}  {'P':>5s} {'R':>5s} {'F1':>5s}  gold  touched  hit  wr  cost"
    )
    for r in rows:
        cost = f"${r['cost']:.3f}" if r["cost"] else "-"
        print(
            f"  {r['id']:30s}  {r['precision']:>5.2f} {r['recall']:>5.2f} {r['f1']:>5.2f}  "
            f"{len(r['gold']):>4}  {len(r['touched']):>7}  {len(r['hit']):>3}  {r['n_writes']:>2}  {cost}"
        )

    import statistics

    print(f"\n=== AGGREGATE ({len(rows)} tasks) ===")
    print(f"  mean precision: {statistics.mean(r['precision'] for r in rows):.3f}")
    print(f"  mean recall:    {statistics.mean(r['recall'] for r in rows):.3f}")
    print(f"  mean F1:        {statistics.mean(r['f1'] for r in rows):.3f}")
    hits = sum(1 for r in rows if r["f1"] >= 0.5)
    print(f"  F1 >= 0.5: {hits}/{len(rows)}")

    # Sub-aggregate: tasks where the arm actually attempted writes. This
    # separates the "gave up" cost from the "wrong edit" cost.
    attempted = [r for r in rows if r["n_writes"] > 0]
    if attempted and len(attempted) < len(rows):
        print(
            f"\n=== ATTEMPTED-ONLY ({len(attempted)}/{len(rows)} arms with writes>0) ==="
        )
        print(
            f"  mean precision: {statistics.mean(r['precision'] for r in attempted):.3f}"
        )
        print(
            f"  mean recall:    {statistics.mean(r['recall'] for r in attempted):.3f}"
        )
        print(f"  mean F1:        {statistics.mean(r['f1'] for r in attempted):.3f}")
        att_hits = sum(1 for r in attempted if r["f1"] >= 0.5)
        print(f"  F1 >= 0.5: {att_hits}/{len(attempted)}")
        no_writes = [r["id"] for r in rows if r["n_writes"] == 0]
        print(f"  no-write arms: {no_writes}")
    total_cost = sum(r["cost"] or 0 for r in rows)
    print(f"  total cost:  ${total_cost:.2f}")


if __name__ == "__main__":
    main()
