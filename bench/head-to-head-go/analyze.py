#!/usr/bin/env python3
"""
Compare defn-mode arm outputs against files-mode baselines in tasks.jsonl.

Expects the defn-arm driver to write per-task trajectory JSON files with
the same fncall_messages schema as the baseline, under `arm_defn/<instance_id>.json`.
Computes wire cost the same way as select_tasks.py and reports deltas.

Usage:
  python3 analyze.py [--arm-dir arm_defn]
"""

import argparse
import glob
import json
import os
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def wire_cost(traj):
    input_bytes = output_bytes = 0
    tool_calls = view_calls = edit_calls = 0
    edit_bytes = view_bytes = 0
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
                nm = fn.get("name", "")
                # str_replace_editor is files-mode; `code` is defn-mode.
                if nm == "str_replace_editor":
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
                elif nm == "code":
                    op = args.get("op", "")
                    if op in (
                        "read",
                        "read-file",
                        "outline",
                        "slice",
                        "overview",
                        "search",
                        "impact",
                    ):
                        view_calls += 1
                    elif op in (
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
                    ):
                        edit_calls += 1
                        edit_bytes += len(
                            args.get("new", args.get("new_body", args.get("body", "")))
                        )
                        n = args.get("name") or args.get("file") or args.get("new_name")
                        if n:
                            files_touched.add(str(n))
        elif role == "tool":
            c = msg.get("content", "")
            if isinstance(c, list):
                c = "".join(
                    x.get("text", "") if isinstance(x, dict) else str(x) for x in c
                )
            output_bytes += len(c or "")
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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arm-dir", default=os.path.join(HERE, "arm_defn"))
    args = ap.parse_args()

    tasks = [json.loads(l) for l in open(os.path.join(HERE, "tasks.jsonl"))]
    print(f"tasks: {len(tasks)}", file=sys.stderr)

    rows = []
    missing = []
    for t in tasks:
        arm_path = os.path.join(args.arm_dir, t["instance_id"] + ".json")
        if not os.path.exists(arm_path):
            missing.append(t["instance_id"])
            continue
        d = json.load(open(arm_path))
        traj = d.get("fncall_messages") or d.get("messages") or []
        arm_wc = wire_cost(traj)
        b = t["baseline_files_mode"]
        rows.append({"id": t["instance_id"], "baseline": b, "defn": arm_wc})

    if missing:
        print(
            f"\nMISSING defn-arm output for {len(missing)}/{len(tasks)} tasks:",
            file=sys.stderr,
        )
        for m in missing[:20]:
            print(f"  {m}", file=sys.stderr)

    if not rows:
        print("\nNo defn-arm outputs to compare. Run defn-arm first.", file=sys.stderr)
        sys.exit(0)

    print(f"\n=== per-task deltas (defn / baseline) ===")
    print(
        f"  {'instance':30s}  {'calls':>15s}  {'input':>17s}  {'output':>17s}  {'total':>17s}"
    )
    for r in rows:
        b, dfn = r["baseline"], r["defn"]

        def d(a, k):
            return f"{dfn[k]:>4}/{b[k]:<4}={dfn[k] / max(b[k], 1):.2f}"

        total_b = b["input_bytes"] + b["output_bytes"]
        total_d = dfn["input_bytes"] + dfn["output_bytes"]
        total_ratio = total_d / max(total_b, 1)
        print(
            f"  {r['id']:30s}  {d(dfn, 'tool_calls'):>15s}  "
            f"{dfn['input_bytes']:>7,}/{b['input_bytes']:<6,}  "
            f"{dfn['output_bytes']:>7,}/{b['output_bytes']:<6,}  "
            f"{total_ratio:>17.2f}"
        )

    # Aggregates
    tot_b_in = sum(r["baseline"]["input_bytes"] for r in rows)
    tot_b_out = sum(r["baseline"]["output_bytes"] for r in rows)
    tot_d_in = sum(r["defn"]["input_bytes"] for r in rows)
    tot_d_out = sum(r["defn"]["output_bytes"] for r in rows)
    tot_b_calls = sum(r["baseline"]["tool_calls"] for r in rows)
    tot_d_calls = sum(r["defn"]["tool_calls"] for r in rows)

    print(f"\n=== AGGREGATE ({len(rows)} tasks) ===")

    def line(label, b, d):
        delta = (d - b) / max(b, 1) * 100
        print(f"  {label:20s}  baseline={b:>12,}  defn={d:>12,}  delta={delta:+6.1f}%")

    line("tool calls", tot_b_calls, tot_d_calls)
    line("input bytes", tot_b_in, tot_d_in)
    line("output bytes", tot_b_out, tot_d_out)
    line("total wire", tot_b_in + tot_b_out, tot_d_in + tot_d_out)

    ratios = [
        (r["defn"]["input_bytes"] + r["defn"]["output_bytes"])
        / max(r["baseline"]["input_bytes"] + r["baseline"]["output_bytes"], 1)
        for r in rows
    ]
    print(f"\nper-task total-wire ratio (defn/baseline):")
    print(
        f"  mean {statistics.mean(ratios):.3f}  median {statistics.median(ratios):.3f}"
    )
    print(f"  wins (ratio<1): {sum(1 for x in ratios if x < 1)}/{len(ratios)}")
    print(f"  losses (>1):   {sum(1 for x in ratios if x > 1)}/{len(ratios)}")


if __name__ == "__main__":
    main()
