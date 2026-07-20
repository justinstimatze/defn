#!/usr/bin/env python3
"""
replay_caps.py — cheap static replay of the read-file cap + dedup levers
against recorded arm trajectories. Zero LLM cost; no defn subprocess.

For each arm/instance in arm_defn/, walk the recorded (assistant tool_call,
tool_result) pairs and simulate what the *response wire cost* would have
been if the cap and/or dedup had been applied. Sums per-arm and aggregate
byte deltas. Does NOT rerun the model — behavior shifts (model choosing
different tools in response to caps) are invisible here.

Usage:
  python3 replay_caps.py [--arm-dir arm_defn] [--cap-bytes 8000]
"""

import argparse
import hashlib
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

STUB_SIZE_ESTIMATE = 260  # dedup stub is roughly this long


def read_file_cap_bytes(payload: str, cap: int) -> int:
    """Mirror of Go's compactReadFile: if payload > cap, output = header +
    signature-lines + trailer. We can't cheaply reconstruct signatures from
    the payload alone, so we estimate: header ~ 80 bytes, one line per
    "## <name>" heading in the payload (~80 bytes avg), trailer ~ 220 bytes.
    Overestimates slightly compared to real Go path; conservative for
    savings claims.
    """
    if len(payload) <= cap:
        return len(payload)
    headings = payload.count("\n## ")
    estimated = 80 + headings * 80 + 220
    # Guard: never claim we made a payload BIGGER than the real cap
    if estimated >= len(payload):
        return len(payload)
    return estimated


def is_cacheable_op(op: str) -> bool:
    return op in ("read", "outline", "slice", "read-file", "file-defs")


def op_arg_key(op: str, args: dict) -> str:
    if op == "read":
        return (
            "read|" + str(args.get("name", "")) + ("|full" if args.get("full") else "")
        )
    if op == "outline":
        return "outline|" + str(args.get("name", ""))
    if op == "slice":
        return (
            "slice|"
            + str(args.get("name", ""))
            + "|"
            + str(args.get("slice", ""))
            + "|"
            + str(args.get("index", 0))
        )
    if op == "read-file":
        return "read-file|" + str(args.get("file", ""))
    if op == "file-defs":
        return "file-defs|" + str(args.get("file", ""))
    return ""


def is_write_op(op: str) -> bool:
    return op in {
        "edit",
        "insert",
        "create",
        "delete",
        "rename",
        "move",
        "apply",
        "insert-precondition",
        "replace-slice",
        "replace-hunk",
        "wrap-in-defer",
        "rename-param",
        "add-import",
        "patch",
        "sync",
        "resolve",
        "merge",
        "checkout",
        "commit",
        "merge-abort",
    }


def replay_one(arm_path: str, cap: int) -> dict:
    """Return per-instance byte accounting. Only measures mcp__defn__code
    responses — off-tool escape hatches (Grep/Glob/Agent/...) are separately
    accounted so we can see the defn-relevant slice cleanly."""
    with open(arm_path) as f:
        arm = json.load(f)
    msgs = arm.get("fncall_messages", [])
    off_tool_bytes = 0
    orig_bytes = 0
    capped_bytes = 0
    dedup_bytes = 0  # bytes after ALSO applying dedup on top of cap
    cap_hits = 0
    dedup_hits = 0
    session_cache = {}  # key -> hash; represents defn's session cache
    call_seq = 0
    call_queue = []  # FIFO of (tool_name, op, args) — ALL tools, so we
    # correctly attribute each tool_result to its owning tool_call.
    for m in msgs:
        role = m.get("role")
        if role == "assistant":
            for tc in m.get("tool_calls") or []:
                fn = tc.get("function", {})
                nm = fn.get("name", "")
                try:
                    args = json.loads(fn.get("arguments") or "{}")
                except Exception:
                    args = {}
                op = args.get("op", "") if nm.endswith("__code") else None
                call_queue.append((nm, op, args))
        elif role == "tool" and call_queue:
            tool_name, pending_op, pending_args = call_queue.pop(0)
            content = m.get("content", "") or ""
            n = len(content)
            if not tool_name.endswith("__code"):
                off_tool_bytes += n
                continue
            orig_bytes += n

            # Cap step: applies only to read-file
            capped = n
            if pending_op == "read-file":
                new_n = read_file_cap_bytes(content, cap)
                if new_n < n:
                    cap_hits += 1
                capped = new_n
            capped_bytes += capped

            # Dedup step (applied on top of capped payload)
            after_dedup = capped
            if pending_op is not None and is_cacheable_op(pending_op):
                call_seq += 1
                # Hash the (already capped) content for dedup key.
                # In production, dedup happens on the FINAL response bytes.
                hash_input = (
                    content
                    if pending_op != "read-file"
                    else (
                        content
                        if capped == n
                        else f"<capped payload for {pending_args.get('file', '')}>"
                    )
                )
                h = hashlib.sha256(hash_input.encode()).hexdigest()[:16]
                key = op_arg_key(pending_op, pending_args)
                if capped >= 512:
                    prev = session_cache.get(key)
                    if prev is not None and prev == h:
                        after_dedup = STUB_SIZE_ESTIMATE
                        dedup_hits += 1
                    else:
                        session_cache[key] = h
            elif pending_op and is_write_op(pending_op):
                session_cache.clear()
            dedup_bytes += after_dedup

    return {
        "instance": os.path.basename(arm_path).replace(".json", ""),
        "orig_bytes": orig_bytes,
        "off_tool_bytes": off_tool_bytes,
        "capped_bytes": capped_bytes,
        "dedup_bytes": dedup_bytes,
        "cap_hits": cap_hits,
        "dedup_hits": dedup_hits,
        "cost_usd": arm.get("cost_usd"),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arm-dir", default=os.path.join(HERE, "arm_defn"))
    ap.add_argument("--cap-bytes", type=int, default=8000)
    args = ap.parse_args()

    rows = []
    for name in sorted(os.listdir(args.arm_dir)):
        if not name.endswith(".json"):
            continue
        rows.append(replay_one(os.path.join(args.arm_dir, name), args.cap_bytes))

    if not rows:
        print("no arm outputs", file=sys.stderr)
        return

    print(
        f"=== static replay: read-file cap={args.cap_bytes}B + session dedup (stub={STUB_SIZE_ESTIMATE}B) ==="
    )
    print(
        f"  {'instance':30s}  {'code_orig':>10s} {'off_tool':>10s} {'capped':>10s} {'+dedup':>10s} {'cap#':>5s} {'ddp#':>5s}  {'save%':>6s}"
    )
    tot_orig = tot_capped = tot_ddp = tot_off = 0
    for r in rows:
        save = 0
        if r["orig_bytes"]:
            save = 100 * (r["orig_bytes"] - r["dedup_bytes"]) / r["orig_bytes"]
        tot_orig += r["orig_bytes"]
        tot_capped += r["capped_bytes"]
        tot_ddp += r["dedup_bytes"]
        tot_off += r["off_tool_bytes"]
        print(
            f"  {r['instance']:30s}  {r['orig_bytes']:>10,} {r['off_tool_bytes']:>10,} {r['capped_bytes']:>10,} {r['dedup_bytes']:>10,} {r['cap_hits']:>5} {r['dedup_hits']:>5}  {save:>5.1f}%"
        )
    print()
    print("=== AGGREGATE ===")
    print(
        f"  off-tool bytes (Grep/Glob/Agent/...): {tot_off:>12,} — escape hatch not addressable by defn levers"
    )
    if tot_orig:
        cap_save = 100 * (tot_orig - tot_capped) / tot_orig
        ddp_save = 100 * (tot_orig - tot_ddp) / tot_orig
        print(f"  orig output bytes:    {tot_orig:>12,}")
        print(f"  after read-file cap:  {tot_capped:>12,}  ({cap_save:.1f}% saved)")
        print(f"  after cap + dedup:    {tot_ddp:>12,}  ({ddp_save:.1f}% saved)")


if __name__ == "__main__":
    main()
