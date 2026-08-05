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


def estimate_write_inflation(events, safe_gap_seconds=60.0):
    """Estimates how much MORE real cache_creation is charged on a hit turn
    than the naive "just this turn's new content" model predicts.

    Measured directly (2026-08-05): on turns with a gap < 60s since the
    prior turn -- unambiguously within ANY TTL tier, so unambiguously a
    cache hit -- real cache_creation summed 5.07x higher than new_tokens
    summed, on the defn mega-session. This isn't noise: cache breakpoints
    aren't replanted fresh every single turn, so a growing tail since the
    last TRUE breakpoint likely gets rewritten repeatedly across several
    consecutive turns before it settles into a stable read-only prefix --
    the same new content gets billed as a write more than once before
    it's fully absorbed. Ruled out first: schema churn from mid-session
    defn serve restarts (turns after a commit showed LOWER residuals than
    baseline, the opposite of what that hypothesis predicts).

    Returns 1.0 (no inflation) if there's no safe-gap data to measure
    from, rather than guessing.
    """
    last_ts = None
    sum_new = 0
    sum_real_cc = 0
    for e in events:
        if e["is_compaction"]:
            last_ts = e["ts"]
            continue
        if last_ts is not None:
            gap = (e["ts"] - last_ts).total_seconds()
            if 0 < gap < safe_gap_seconds:
                sum_new += max(0, e["new_tokens"])
                sum_real_cc += e["real_cache_creation"]
        last_ts = e["ts"]
    if sum_new == 0:
        return 1.0
    return sum_real_cc / sum_new


def simulate(events, ttl_seconds, read_mult=0.10, write_mult=2.0, write_inflation=1.0):
    """Replays events under the given TTL/pricing assumption. Returns
    (total_simulated_cost, per_turn list of (sim_read_cost, sim_write_cost)).
    Cost units are relative to base input price (read_mult/write_mult are
    the multipliers, e.g. 0.10 and 2.0 for the 1h tier per Anthropic's
    published pricing at time of writing).

    write_inflation multiplies the write cost of a turn's NEW content on
    a cache hit (not a full-rebuild miss, which already pays for the
    whole accumulated context and isn't further inflated) -- see
    estimate_write_inflation. Defaults to 1.0 (no inflation, the original
    naive model) for backward compatibility; pass the estimated value to
    use the calibrated model.
    """
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
            write_cost = new_tokens * write_mult * write_inflation
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
CALIBRATION STATUS (2026-08-05): the naive model (write_inflation=1.0, "a
hit only pays for its own marginal new content") underestimated real
recorded cost by ~14%. Root cause: on turns unambiguously within any TTL
window (gap < 60s), real cache_creation was measured at ~5x the naive
model's prediction -- cache breakpoints aren't replanted fresh every
turn, so a growing tail since the last true breakpoint gets rewritten
repeatedly across several consecutive turns before it settles into a
stable read-only prefix. write_inflation (auto-estimated per session via
estimate_write_inflation, ~5.07x on the defn mega-session) corrects for
this: calibration error dropped from -14.0% to +2.6%. RULED OUT along the
way: mid-session defn serve restarts busting the cache via tool-schema
churn -- turns after a commit showed LOWER residuals than baseline, the
opposite of that hypothesis's prediction. +2.6% residual remains
unexplained but is small enough for directional use; only validated on
one session (the defn mega-session) -- treat cross-session comparisons
with proportionate caution until validated on a second one. See
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
        "--write-inflation",
        type=float,
        default=None,
        help="Override the auto-estimated write-inflation factor (see estimate_write_inflation)",
    )
    ap.add_argument(
        "--no-inflation",
        action="store_true",
        help="Use the naive write_inflation=1.0 model instead of the calibrated one",
    )
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

    if args.no_inflation:
        inflation = 1.0
    elif args.write_inflation is not None:
        inflation = args.write_inflation
    else:
        inflation = estimate_write_inflation(events)
    print(
        f"write_inflation={inflation:.2f}x (estimated from gap<60s turns unless overridden)"
    )

    if args.calibrate:
        sim_total, _ = simulate(
            events, args.ttl, args.read_mult, args.write_mult, inflation
        )
        real_total = real_cost(events, args.read_mult, args.write_mult)
        err = (
            (sim_total - real_total) / real_total * 100 if real_total else float("nan")
        )
        print(
            f"TTL={args.ttl:.0f}s  simulated_cost={sim_total:,.0f}  real_cost={real_total:,.0f}  error={err:+.1f}%"
        )
    else:
        sim_total, _ = simulate(
            events, args.ttl, args.read_mult, args.write_mult, inflation
        )
        print(f"TTL={args.ttl:.0f}s  simulated_cost={sim_total:,.0f} (relative units)")


if __name__ == "__main__":
    main()
