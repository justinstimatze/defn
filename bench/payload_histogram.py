#!/usr/bin/env python3
"""payload_histogram.py [<traj.json> ...]

Gap-analysis work-order item 6 (docs/gap-analysis-2026-09-02.md §5,
docs/lessons-learned.md's Handoff section): bytes-by-op histogram across
every arm_defn trajectory on disk. Flags any op whose median result
exceeds files-mode's ~470 B baseline (from bench/tokens.py's own
byte-vs-token warning: measure both, bytes alone can mislead) so the
enrichment on that op can be made opt-in or budgeted. Reuses
tail_event_detector.py's `paired_events` (same call/result pairing, same
FIFO-queue rationale -- see that script's docstring) so this and the
tail-event detector never disagree on how a trajectory is walked.

With no arguments, auto-discovers every `arm_defn*/*.json` trajectory
under bench/. Pass explicit paths to scope to one corpus.

Requires tiktoken for the token column (bench/tokens.py's own rule:
never report a byte savings/cost claim without a token cross-check,
since short "compressed" placeholders often expand to MORE tokens than
the identifier they replace). Run via:
    uv run --with tiktoken python3 bench/payload_histogram.py
"""

import glob
import json
import os
import statistics
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(__file__))
from tail_event_detector import paired_events  # noqa: E402
from tokens import count_tokens  # noqa: E402

BASELINE_BYTES = 470  # files-mode's own median result size, per item 6's spec

# Dolt-era git-semantics ops, removed in the v0.27 SQLite migration (see
# CLAUDE.md's op list and commit 7d66258's fix #3) -- even a "not
# supported" response for one of these isn't a real payload-size signal,
# just leftover noise from pre-migration trajectories.
REMOVED_OPS = {
    "branch",
    "checkout",
    "merge",
    "commit",
    "status",
    "conflicts",
    "resolve",
    "merge-abort",
    "diff",
    "diff-defs",
    "history",
}


def collect(paths):
    by_op = defaultdict(list)  # op -> [byte_len, ...]
    for p in paths:
        try:
            data = json.load(open(p))
        except Exception as e:
            print(f"skip {p}: {e}", file=sys.stderr)
            continue
        for op, args, content, is_defn in paired_events(data["fncall_messages"]):
            if not is_defn:
                continue
            # Not a real op result at all -- the model guessed a
            # nonexistent op name (seen live: "ingest", "sql", "grep",
            # "add_import", stale-era "help"). The error string's size
            # says nothing about any real op's payload weight.
            if content.startswith('unknown op "'):
                continue
            if op in REMOVED_OPS:
                continue
            by_op[op].append(len(content.encode("utf-8", "replace")))
    return by_op


def pct(sorted_vals, p):
    if not sorted_vals:
        return 0
    idx = min(len(sorted_vals) - 1, int(len(sorted_vals) * p))
    return sorted_vals[idx]


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

    by_op = collect(paths)
    total_calls = sum(len(v) for v in by_op.values())
    print(
        f"=== payload histogram: {len(paths)} trajectories, {total_calls} defn calls, "
        f"{len(by_op)} distinct ops ===\n"
    )

    rows = []
    for op, vals in by_op.items():
        vals_sorted = sorted(vals)
        median = statistics.median(vals_sorted)
        rows.append(
            {
                "op": op,
                "n": len(vals),
                "mean": statistics.mean(vals_sorted),
                "median": median,
                "p90": pct(vals_sorted, 0.90),
                "max": vals_sorted[-1],
                "total": sum(vals_sorted),
                "over_baseline": median > BASELINE_BYTES,
            }
        )
    rows.sort(key=lambda r: r["median"], reverse=True)

    print(
        f"{'op':<20} {'n':>5} {'median B':>9} {'mean B':>8} {'p90 B':>8} {'max B':>8} {'total KB':>9}  flag"
    )
    for r in rows:
        flag = "OVER BASELINE" if r["over_baseline"] else ""
        print(
            f"{r['op']:<20} {r['n']:>5} {r['median']:>9.0f} {r['mean']:>8.0f} "
            f"{r['p90']:>8.0f} {r['max']:>8.0f} {r['total'] / 1024:>9.1f}  {flag}"
        )

    over = [r for r in rows if r["over_baseline"]]
    print(
        f"\n{len(over)}/{len(rows)} ops exceed the {BASELINE_BYTES} B median baseline."
    )

    print("\n=== token cross-check (bench/tokens.py rule: bytes alone can mislead) ===")
    print(f"{'op':<20} {'median tok':>10} {'p90 tok':>10} {'max tok':>10}")
    top_ops = {r["op"] for r in rows[:12]}
    by_op_content = defaultdict(list)
    for p in paths:
        try:
            data = json.load(open(p))
        except Exception:
            continue
        for op, args, content, is_defn in paired_events(data["fncall_messages"]):
            # Token counting needs the actual strings, not just the byte
            # lengths `collect()` kept -- a second pass, scoped to just
            # the top-12-by-median ops so this doesn't re-tokenize every
            # op's full result set.
            if is_defn and op in top_ops:
                by_op_content[op].append(content)
    for r in rows[:12]:
        contents = by_op_content[r["op"]]
        # Sample up to 200 results per op -- tiktoken is fast, but no
        # need to re-encode every single one when an op has thousands
        # of near-identical-shape results.
        sample = contents if len(contents) <= 200 else contents[:200]
        tok_counts = sorted(count_tokens(c) for c in sample)
        med = statistics.median(tok_counts)
        p90 = pct(tok_counts, 0.90)
        mx = tok_counts[-1]
        print(f"{r['op']:<20} {med:>10.0f} {p90:>10.0f} {mx:>10.0f}")
