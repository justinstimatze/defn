#!/usr/bin/env python3
"""
bench/cache-sim/sweep_stale_threshold.py -- sweep internal/mcp/dedup.go's
staleEpochThreshold against real repeat-target data from a session
transcript, reporting actual TOKEN cost (not bytes -- see
feedback_tokens_not_bytes) for each candidate threshold.

For every real repeat occurrence of a dedup-eligible (op, target) pair,
this measures the token cost of what would ACTUALLY be served under a
given threshold T:
  - epoch_distance <= T: the cheap suppression stub (~40 tokens, measured
    from the real stub text format in internal/mcp/dedup.go).
  - epoch_distance > T: real content. For read/outline/slice (the
    subsumption-capable ops), the fixed code redirects to `expand`
    instead of the narrow op -- estimated at 1.6x the narrow response's
    token count (outline+callers+body vs. just body; NOT independently
    measured, since expand wasn't actually called at these historical
    points -- flagged as an estimate, not measured, throughout output).
    For plain dedup ops, it's the real op's own (measured) token count.

Usage: python3 sweep_stale_threshold.py <session.jsonl> [--max-threshold 10]
"""

import argparse
import json
import sys

try:
    import tiktoken

    ENC = tiktoken.get_encoding("cl100k_base")
except ImportError:
    print(
        "tiktoken not installed -- run via: uv run --with tiktoken python3 sweep_stale_threshold.py ...",
        file=sys.stderr,
    )
    sys.exit(1)

SUBSUMPTION_OPS = {"read", "outline", "slice"}
DEDUP_OPS = {
    "read",
    "outline",
    "slice",
    "read-file",
    "file-defs",
    "impact",
    "overview",
    "expand",
    "methods",
    "explain",
    "search",
    "find",
}
EXPAND_INFLATION_ESTIMATE = 1.6  # NOT measured -- see module docstring
STUB_TOKENS_ESTIMATE = 40  # measured from the actual stub text format


def ntok(s):
    if not s:
        return 0
    return len(ENC.encode(s, disallowed_special=()))


def target_key(op, args):
    if op in ("read", "outline", "slice", "impact", "expand", "methods", "explain"):
        name = args.get("name")
        if isinstance(args.get("names"), list) and args["names"]:
            name = ",".join(args["names"])
        if name:
            return f"{op}:{name}"
    if op in ("read-file", "file-defs"):
        f = args.get("file")
        if f:
            return f"{op}:{f}"
    if op == "overview":
        return f"overview:{args.get('file', '<project>')}"
    if op == "search":
        return f"search:{args.get('pattern', '?')}"
    if op == "find":
        return f"find:{args.get('file', '?')}|{args.get('line', 0)}"
    return None


def load_repeats(path):
    """Returns a list of dicts: {op, key, epoch_distance, real_tokens}
    -- one per REPEAT occurrence (2nd+ ask) of a dedup-eligible target.
    real_tokens is the token count of the ORIGINAL occurrence's tool_result
    (a proxy for "what this repeat would cost if served fresh" -- content
    is often identical or near-identical for a repeat of the same ask)."""
    epoch = 0
    last_seen_epoch = {}
    last_seen_tokens = {}
    repeats = []

    lines = open(path, errors="ignore").readlines()
    for i, line in enumerate(lines):
        try:
            d = json.loads(line)
        except Exception:
            continue
        if d.get("isCompactSummary") is True:
            epoch += 1
            continue
        if d.get("type") != "assistant":
            continue
        content = d.get("message", {}).get("content")
        if not isinstance(content, list):
            continue
        for c in content:
            if not isinstance(c, dict) or c.get("type") != "tool_use":
                continue
            if c.get("name") != "mcp__defn__code":
                continue
            args = c.get("input") or {}
            op = args.get("op")
            if op not in DEDUP_OPS:
                continue
            key = target_key(op, args)
            if not key:
                continue

            # Find this call's tool_result (next "user"-role message with a
            # matching tool_use_id), to measure its real token size.
            tool_use_id = c.get("id")
            result_tokens = None
            for j in range(i + 1, min(i + 6, len(lines))):
                try:
                    rd = json.loads(lines[j])
                except Exception:
                    continue
                if rd.get("type") != "user":
                    continue
                rcontent = rd.get("message", {}).get("content")
                if not isinstance(rcontent, list):
                    continue
                for rc in rcontent:
                    if (
                        isinstance(rc, dict)
                        and rc.get("type") == "tool_result"
                        and rc.get("tool_use_id") == tool_use_id
                    ):
                        rcc = rc.get("content")
                        text = (
                            "".join(
                                b.get("text", "") for b in rcc if isinstance(b, dict)
                            )
                            if isinstance(rcc, list)
                            else str(rcc or "")
                        )
                        result_tokens = ntok(text)
                break

            if key in last_seen_epoch:
                repeats.append(
                    {
                        "op": op,
                        "key": key,
                        "epoch_distance": epoch - last_seen_epoch[key],
                        "real_tokens": last_seen_tokens.get(key, result_tokens or 0),
                    }
                )
            last_seen_epoch[key] = epoch
            if result_tokens is not None:
                last_seen_tokens[key] = result_tokens
    return repeats


def cost_at_threshold(repeats, threshold):
    total = 0
    for r in repeats:
        if r["epoch_distance"] <= threshold:
            total += STUB_TOKENS_ESTIMATE
        elif r["op"] in SUBSUMPTION_OPS:
            total += int(r["real_tokens"] * EXPAND_INFLATION_ESTIMATE)
        else:
            total += r["real_tokens"]
    return total


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("session_path")
    ap.add_argument("--max-threshold", type=int, default=10)
    args = ap.parse_args()

    print(
        "NOTE: expand-redirect cost uses a 1.6x inflation ESTIMATE over the "
        "narrow op's measured tokens (not independently measured -- expand "
        "wasn't actually called at these historical points). Stub cost is "
        f"a {STUB_TOKENS_ESTIMATE}-token estimate from the real stub text "
        "format. Directional, not exact.",
        file=sys.stderr,
    )

    repeats = load_repeats(args.session_path)
    print(f"Loaded {len(repeats)} real repeat occurrences.\n")

    prev_cost = None
    for t in range(0, args.max_threshold + 1):
        cost = cost_at_threshold(repeats, t)
        delta = f"  (Δ {cost - prev_cost:+,d})" if prev_cost is not None else ""
        print(f"staleEpochThreshold={t:2d}  total_tokens={cost:8,d}{delta}")
        prev_cost = cost


if __name__ == "__main__":
    main()
