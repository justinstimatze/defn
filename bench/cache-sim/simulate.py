#!/usr/bin/env python3
"""
bench/cache-sim/simulate.py -- replay real Claude Code session transcripts
through a prompt-cache cost model, calibrate it against the ACTUAL recorded
cache_read/cache_creation numbers, then run counterfactual TTL/pricing
scenarios against the same real event sequence.

Motivation (2026-08-05): a long investigation this session found real,
measured caching costs (compaction resets, TTL-lapse taxes) by hand-writing
one-off analysis scripts per question. This formalizes that into a reusable
tool so future "what if X were different" questions get an actual number
instead of another round of ad hoc scripting -- and so a calibration
mismatch surfaces gaps in our own understanding mechanically, rather than
via more manual digging.

Model
-----
Per real assistant turn, total context at that point is estimated directly
from recorded usage:

    total_context[i] = cache_read_input_tokens[i] + cache_creation_input_tokens[i]

This holds regardless of whether turn i was a cache hit or a miss -- either
way, cache_read + cache_creation together account for "the whole prefix
Anthropic had to account for" at that turn. The turn-over-turn delta is
this turn's genuinely new content:

    new_tokens[i] = total_context[i] - total_context[i-1]

A negative delta means the context SHRANK -- i.e. a real compaction
happened between turn i-1 and i (folds in naturally; no need to cross-
reference isCompactSummary markers separately, though we do for validation).

Given that event stream, the simulator replays cost under a chosen
(ttl_seconds, read_multiplier, write_multiplier): each turn either falls
inside the TTL window since the previous turn (cheap: prior accumulated
context at the read rate, only this turn's new content at the write rate)
or outside it (expensive: the WHOLE accumulated context + new content, all
at the write rate, since the cache had fully lapsed).

Usage
-----
    python3 simulate.py <session.jsonl> [--ttl 3600] [--calibrate]

--calibrate replays with the given ttl and reports how closely the
simulated cache_read/cache_creation totals track the REAL recorded totals
-- the sanity check that must pass before trusting any counterfactual run.
"""

import argparse
import json
import sys
from datetime import datetime


def parse_ts(s):
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except Exception:
        return None


def load_events(path):
    """Returns a list of dicts: {ts, real_cache_read, real_cache_creation,
    total_context, new_tokens, is_compaction}, one per usage-bearing
    assistant turn, in order."""
    raw = []
    with open(path, errors="ignore") as f:
        for line in f:
            try:
                d = json.loads(line)
            except Exception:
                continue
            if d.get("type") != "assistant":
                continue
            ts = parse_ts(d.get("timestamp", ""))
            u = d.get("message", {}).get("usage")
            if not (ts and u):
                continue
            cr = u.get("cache_read_input_tokens", 0) or 0
            cc = u.get("cache_creation_input_tokens", 0) or 0
            if cr + cc == 0:
                continue
            raw.append(
                {
                    "ts": ts,
                    "real_cache_read": cr,
                    "real_cache_creation": cc,
                    "total_context": cr + cc,
                }
            )

    events = []
    prev_total = None
    for r in raw:
        new_tokens = (
            r["total_context"] - prev_total
            if prev_total is not None
            else r["total_context"]
        )
        is_compaction = new_tokens < 0
        events.append({**r, "new_tokens": new_tokens, "is_compaction": is_compaction})
        prev_total = r["total_context"]
    return events


def simulate(events, ttl_seconds, read_mult=0.10, write_mult=2.0):
    """Replays events under the given TTL/pricing assumption. Returns
    (total_simulated_cost, per_turn list of (sim_read_cost, sim_write_cost)).
    Cost units are relative to base input price (read_mult/write_mult are
    the multipliers, e.g. 0.10 and 2.0 for the 1h tier per Anthropic's
    published pricing at time of writing)."""
    total_cost = 0.0
    accumulated = 0
    last_ts = None
    per_turn = []
    for e in events:
        if e["is_compaction"]:
            # Compaction resets accumulated size to whatever the new
            # (smaller) total_context is -- but that new prefix still has
            # to be freshly cached on this turn, which is a real write,
            # not a free reset. Calibration against real transcripts
            # (2026-08-05) showed a systematic ~17% underestimate before
            # this was charged.
            accumulated = e["total_context"]
            write_cost = accumulated * write_mult
            total_cost += write_cost
            per_turn.append((0.0, write_cost))
            last_ts = e["ts"]
            continue
        new_tokens = e["new_tokens"]
        if last_ts is not None and (e["ts"] - last_ts).total_seconds() < ttl_seconds:
            read_cost = accumulated * read_mult
            write_cost = new_tokens * write_mult
        else:
            read_cost = 0.0
            write_cost = (accumulated + new_tokens) * write_mult
        total_cost += read_cost + write_cost
        per_turn.append((read_cost, write_cost))
        accumulated += new_tokens
        last_ts = e["ts"]
    return total_cost, per_turn


def real_cost(events, read_mult=0.10, write_mult=2.0):
    """The ACTUAL cost implied by the real recorded cache_read/cache_creation
    numbers, at the given pricing multipliers -- the ground truth to
    calibrate against."""
    total = 0.0
    for e in events:
        total += (
            e["real_cache_read"] * read_mult + e["real_cache_creation"] * write_mult
        )
    return total


CALIBRATION_CAVEAT = """\
================================================================================
CAVEAT (2026-08-05, still open): this simulator's cost model is calibrated
to ~14% error vs. real recorded cache_read/cache_creation totals on the one
session it's been validated against, even after fixing the compaction
write-cost omission. Leading unverified hypothesis: rebuilding/restarting
defn serve mid-session changes the registered `code` tool's schema, which
busts Anthropic's prompt cache invisibly to this model's size-based
compaction detector (it only catches context SHRINKING, not schema churn
at similar size). NOT confirmed -- treat all numbers from this tool as
DIRECTIONAL ONLY until that gap is chased down and closed. See
bench/cache-sim/README.md.
================================================================================
"""


def main():
    print(CALIBRATION_CAVEAT, file=sys.stderr)
    ap = argparse.ArgumentParser()
    ap.add_argument("session_path")
    ap.add_argument(
        "--ttl", type=float, default=3600.0, help="TTL in seconds to simulate"
    )
    ap.add_argument("--read-mult", type=float, default=0.10)
    ap.add_argument("--write-mult", type=float, default=2.0)
    ap.add_argument(
        "--calibrate",
        action="store_true",
        help="Compare sim vs real at the given TTL/pricing",
    )
    args = ap.parse_args()

    events = load_events(args.session_path)
    n_compactions = sum(1 for e in events if e["is_compaction"])
    print(
        f"Loaded {len(events)} usage-bearing turns, {n_compactions} inferred compactions."
    )

    if args.calibrate:
        sim_total, _ = simulate(events, args.ttl, args.read_mult, args.write_mult)
        real_total = real_cost(events, args.read_mult, args.write_mult)
        err = (
            (sim_total - real_total) / real_total * 100 if real_total else float("nan")
        )
        print(
            f"TTL={args.ttl:.0f}s  simulated_cost={sim_total:,.0f}  real_cost={real_total:,.0f}  error={err:+.1f}%"
        )
    else:
        sim_total, _ = simulate(events, args.ttl, args.read_mult, args.write_mult)
        print(f"TTL={args.ttl:.0f}s  simulated_cost={sim_total:,.0f} (relative units)")


if __name__ == "__main__":
    main()
