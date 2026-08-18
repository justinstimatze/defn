#!/usr/bin/env python3
"""mine_trajectory.py <traj.json> [<traj.json> ...]
       mine_trajectory.py --latency <stream.jsonl>

Reusable defn-arm trajectory friction miner. Prints op frequency,
repeated (op, real-target) calls, and high-precision friction hits
(narrow literal error signatures, not a broad \berror\b substring
match -- that produced heavy false-positive noise in an earlier
one-off version of this script, matching the word "error" inside
ordinary doc-comment text returned by successful reads).

--latency mode reads the RAW claude -p stream-json (agent_driver.py
writes it to <workdir>/.claude-stream.jsonl, and it survives on disk
after the run per WORKDIR_ROOT's own doc comment) and splits total
wall-clock into: read-shaped tool latency (search/read/outline/impact/
context/expand -- pure defn round-trip, should be ~1-2s), write/test-
shaped tool latency (test/create/apply/edit/replace-hunk -- includes
real go build/go test execution time, NOT defn protocol overhead), and
model generation time (everything else -- Opus/Sonnet's own reasoning
between calls). Added 2026-08-18 after a user asked "does this imply
any changes" following a manual one-off version of this exact analysis
-- codifying it here means the next "is defn actually slow or is this
just go test being slow" question doesn't require redoing timestamp
archaeology from scratch.
"""
import json
import sys
from datetime import datetime
from collections import Counter, defaultdict

READ_SHAPED_OPS = {"search", "read", "outline", "impact", "context", "expand", "similar", "methods", "overview"}


def analyze_latency(stream_path):
    with open(stream_path) as f:
        lines = [json.loads(l) for l in f if l.strip()]

    def parse_ts(l):
        ts = l.get("timestamp")
        return datetime.fromisoformat(ts.replace("Z", "+00:00")) if ts else None

    calls = {}
    for l in lines:
        if l["type"] == "assistant":
            for c in l.get("message", {}).get("content", []) or []:
                if c.get("type") == "tool_use":
                    calls[c["id"]] = (parse_ts(l), c.get("input"))

    read_gaps, write_gaps = [], []
    for l in lines:
        if l["type"] == "user":
            for c in l.get("message", {}).get("content", []) or []:
                if c.get("type") == "tool_result":
                    call_ts, args = calls.get(c.get("tool_use_id"), (None, None))
                    result_ts = parse_ts(l)
                    if call_ts and result_ts and args:
                        gap = (result_ts - call_ts).total_seconds()
                        (read_gaps if args.get("op") in READ_SHAPED_OPS else write_gaps).append((gap, args.get("op")))

    events = [(parse_ts(l), l["type"] == "user" and l.get("tool_use_result") is not None) for l in lines if parse_ts(l)]
    events.sort(key=lambda x: x[0])
    tool_latency_total = sum((events[i][0] - events[i - 1][0]).total_seconds() for i in range(1, len(events)) if events[i][1])
    gen_time_total = sum((events[i][0] - events[i - 1][0]).total_seconds() for i in range(1, len(events)) if not events[i][1])
    total = tool_latency_total + gen_time_total

    print(f"=== latency breakdown: {stream_path} ===")
    print(f"  total wall (accounted): {total:.1f}s")
    print(f"  model generation (Opus/Sonnet thinking between calls): {gen_time_total:.1f}s ({100*gen_time_total/total:.0f}%)")
    print(f"  tool round-trip total: {tool_latency_total:.1f}s ({100*tool_latency_total/total:.0f}%)")
    if read_gaps:
        s = sum(g for g, _ in read_gaps)
        print(f"    read-shaped ops:  n={len(read_gaps)} sum={s:.1f}s mean={s/len(read_gaps):.2f}s  <- pure defn round-trip")
    if write_gaps:
        s = sum(g for g, _ in write_gaps)
        print(f"    write/test-shaped ops: n={len(write_gaps)} sum={s:.1f}s mean={s/len(write_gaps):.2f}s  <- includes real go build/test time, NOT defn overhead")
        for g, op in sorted(write_gaps, reverse=True)[:5]:
            print(f"      {g:6.1f}s  {op}")

FRICTION_PATTERNS = [
    r"rolled back",
    r"could not be matched",
    r"hunk not found",
    r"BUILD FAILED",
    r"build failed",
    r"undefined:",
    r"database is locked",
    r"\bBUSY\b",
    r"redeclared",
    r"already registered",
    r"could not be matched to an on-disk declaration",
    r"Updated 0 callers",
    r"unexpected additional properties",
    r"unknown op",
    r"NO TESTS MATCHED",
]
import re
COMBINED = re.compile("|".join(FRICTION_PATTERNS))


def target_key(op, args):
    # Multi-target ops (expand with names:[...], apply with operations:[...])
    # each call is almost never a real repeat of a prior one even when
    # name:/file: are both empty -- group on the actual batch contents,
    # not the args that happen to be unset for this op shape. An earlier
    # version of this script (and a separate, independently-written one
    # from a prior session) both made this exact mistake: grouping by
    # (op, name-or-file-or-"") silently collapsed every multi-target call
    # into one bucket, since ops like expand/apply/search don't always
    # populate name/file at all.
    if op == "search":
        return args.get("pattern", "")
    if op == "test":
        return args.get("test") or args.get("name", "")
    if op == "expand" and args.get("names"):
        return "|".join(sorted(args["names"]))
    if op == "apply" and args.get("operations"):
        return "|".join(sorted(o.get("op", "?") + ":" + (o.get("name") or "") for o in args["operations"]))
    return args.get("name") or args.get("file") or ""


def mine(path):
    data = json.load(open(path))
    iid = data.get("instance_id", path)
    msgs = data["fncall_messages"]
    op_counter = Counter()
    call_seen = Counter()
    friction = []
    for i, m in enumerate(msgs):
        for tc in m.get("tool_calls") or []:
            if tc["function"]["name"] != "mcp__defn__code":
                continue
            try:
                args = json.loads(tc["function"]["arguments"])
            except Exception:
                args = {}
            op = args.get("op", "?")
            op_counter[op] += 1
            call_seen[(op, target_key(op, args))] += 1
        if m.get("role") == "tool":
            content = m.get("content", "")
            if isinstance(content, list):
                content = " ".join(str(c) for c in content)
            hits = COMBINED.findall(content)
            if hits:
                friction.append((i, sorted(set(hits)), content[:250]))

    print(f"=== {iid} ({data.get('defn_version', '?')}) ===")
    print(f"  cost=${data.get('cost_usd', 0):.3f} elapsed={data.get('elapsed_sec', 0):.0f}s "
          f"rc={data.get('claude_rc')} n_msgs={len(msgs)}")
    print(f"  ops: {dict(op_counter.most_common())}")
    repeats = [(k, n) for k, n in call_seen.items() if n >= 3]
    if repeats:
        print(f"  repeated calls (>=3x): {repeats}")
    if friction:
        print(f"  friction hits ({len(friction)}):")
        for i, pats, snippet in friction[:10]:
            print(f"    msg#{i} {pats}: {snippet!r}")
    print()


if __name__ == "__main__":
    args = sys.argv[1:]
    if args and args[0] == "--latency":
        for p in args[1:]:
            analyze_latency(p)
    else:
        for p in args:
            mine(p)
