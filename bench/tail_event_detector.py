#!/usr/bin/env python3
"""tail_event_detector.py [<traj.json> ...]

Gap-analysis work-order item 5 (docs/gap-analysis-2026-09-02.md §5,
docs/lessons-learned.md's Handoff section): automate the bug-hunt that
manually found bug-report-2026-09-02-create-duplicates-shared-import-
alias.md. Flags any defn tool call whose result looks like an
error/no-op (reusing mine_trajectory.py's FRICTION_PATTERNS -- kept in
one place so the two scripts never drift apart on what counts as
friction) that is followed by >=5 calls before the next successful
*write* call. Ranks by calls burned -- that ranking IS the bug-hunt
queue: the longer defn flails before a write succeeds, the more likely
something is actually broken (not just the model being slow).

With no arguments, auto-discovers every `arm_defn*/*.json` trajectory
under bench/ (prom-opus, etcd-multifile-v2, head-to-head-go,
starter-bundle-ab, ...). Pass explicit paths to scope to one corpus.

CAVEAT, confirmed 2026-09-02 while building this: trajectories on disk
span 2026-07-22 through 2026-08-24, and defn has had real bug fixes
land throughout that window (e.g. commit 7d66258, 2026-08-10 -- the
definitions-table UNIQUE-constraint fix for duplicate init()/registration
collisions). A flagged event's `trigger` may describe a bug that is
ALREADY FIXED as of today, not a currently-live one. Confirmed directly:
this script's #1 hit against bench/prometheus-repo/arm_defn (dated
2026-08-09, pre-fix) was "Config named msk is already registered" --
exactly the symptom 7d66258 fixed the next day. Cross-checking the same
2 task IDs in bench/prometheus-repo-opus/arm_defn (dated 2026-08-20,
post-fix) shows zero flagged events for either. Before trusting any
flagged event as an open bug: check `git log -S "<snippet from the
trigger>"` and, if the same instance_id was ever re-run in a newer
corpus directory, diff this script's output against that rerun.

Pairing calls with results: fncall_messages tool-result entries carry
no tool_call_id (confirmed by inspection -- see mine_trajectory.py's
own handling), but they appear in the same relative order as the
tool_calls that produced them, one result per call, so a FIFO queue
correctly pairs call i with the i-th subsequent tool message even when
one assistant turn issues several tool_calls before any results land.
"""

import glob
import json
import os
import sys
from collections import deque

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "head-to-head-go"))
from mine_trajectory import COMBINED, target_key  # noqa: E402

# Ops that mutate stored source (the "byte-exact PUTGET" set from
# CLAUDE.md's Code Navigation section), i.e. what "a successful write"
# means for this detector. Deliberately excludes read-shaped ops, plan/
# query/test/sync/emit -- a passing test or a clean query isn't the
# "recovery" this detector is looking for, only a mutation landing.
WRITE_OPS = {
    "edit",
    "create",
    "delete",
    "rename",
    "move",
    "apply",
    "patch",
    "insert",
    "insert-header",
    "insert-precondition",
    "replace-slice",
    "replace-hunk",
    "wrap-in-defer",
    "rename-param",
    "add-import",
    "retarget-field-value",
}

MIN_CALLS_BURNED = 5


def paired_events(msgs):
    """Yield (op, args, result_content, is_defn) in call order."""
    pending = deque()
    for m in msgs:
        for tc in m.get("tool_calls") or []:
            is_defn = tc["function"]["name"] == "mcp__defn__code"
            try:
                args = json.loads(tc["function"]["arguments"]) if is_defn else {}
            except Exception:
                args = {}
            pending.append((args.get("op", "?"), args, is_defn))
        if m.get("role") == "tool" and pending:
            op, args, is_defn = pending.popleft()
            content = m.get("content", "")
            if isinstance(content, list):
                content = " ".join(str(c) for c in content)
            yield op, args, content, is_defn


def find_tail_events(path):
    data = json.load(open(path))
    iid = data.get("instance_id", path)
    events = list(paired_events(data["fncall_messages"]))

    flagged = []
    for i, (op, args, content, is_defn) in enumerate(events):
        if not is_defn or not COMBINED.search(content):
            continue
        # Scan forward for the next successful write: a defn write-op
        # call whose own result does NOT also look like friction.
        recovery_at = None
        for j in range(i + 1, len(events)):
            j_op, _, j_content, j_is_defn = events[j]
            if j_is_defn and j_op in WRITE_OPS and not COMBINED.search(j_content):
                recovery_at = j
                break
        calls_burned = (
            (recovery_at - i) if recovery_at is not None else (len(events) - i)
        )
        if calls_burned >= MIN_CALLS_BURNED:
            trigger_pat = COMBINED.search(content).group(0)
            flagged.append(
                {
                    "instance_id": iid,
                    "path": path,
                    "call_index": i,
                    "op": op,
                    "target": target_key(op, args),
                    "trigger": trigger_pat,
                    "calls_burned": calls_burned,
                    "unresolved": recovery_at is None,
                    "snippet": content[:200],
                }
            )
    return flagged


if __name__ == "__main__":
    paths = sys.argv[1:] or sorted(
        p for p in glob.glob("bench/**/arm_defn*/*.json", recursive=True)
    )
    if not paths:
        print(
            "no arm_defn trajectories found under bench/ -- pass explicit paths",
            file=sys.stderr,
        )
        sys.exit(1)

    all_flagged = []
    for p in paths:
        try:
            all_flagged.extend(find_tail_events(p))
        except Exception as e:
            print(f"skip {p}: {e}", file=sys.stderr)

    all_flagged.sort(key=lambda f: f["calls_burned"], reverse=True)

    print(
        f"=== tail-event detector: {len(paths)} trajectories, "
        f"{len(all_flagged)} events with >= {MIN_CALLS_BURNED} calls burned ===\n"
    )
    for f in all_flagged:
        tag = "UNRESOLVED" if f["unresolved"] else "recovered"
        print(
            f"  {f['calls_burned']:3d} calls  [{tag}]  {f['instance_id']}  "
            f"op={f['op']} target={f['target']!r}  trigger={f['trigger']!r}"
        )
        print(f"      call#{f['call_index']}  {f['snippet']!r}")
    if not all_flagged:
        print("  (none -- no defn error/no-op took >=5 calls to recover from)")
