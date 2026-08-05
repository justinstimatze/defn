# cache-sim

Replays real Claude Code session transcripts through an Anthropic
prompt-cache cost model, so "what if TTL/pricing/policy X were different"
questions get an actual number computed from real recorded trajectories
instead of another round of hand-written one-off analysis scripts.

## ⚠️ Calibration status — read before trusting any number

As of 2026-08-05, `simulate.py --calibrate` reproduces real recorded
cache_read/cache_creation totals to within **+2.6% error** on the defn
project's own long-lived mega-session, and **-2.9% error** on an
independent second session (gas6amus's stope, 223K turns) — cross-session
validated, not a one-off fit. That is close to measurement-grade for
"does A beat B, roughly how much" questions, though still not exact
enough for precise dollar claims.

**Root cause of the original ~14% gap, found and fixed:** the naive model
assumed a cache hit only pays for that turn's own marginal new content.
Measured directly: on turns unambiguously within any TTL window (gap <
60s), real cache_creation was ~5x the naive prediction (5.07x on defn,
5.85x on stope) — cache breakpoints aren't replanted fresh every turn, so
a growing tail since the last true breakpoint gets rewritten repeatedly
across several consecutive turns before it settles into a stable
read-only prefix. `estimate_write_inflation()` measures this per-session
automatically; `simulate()`'s `write_inflation` parameter corrects for it.

**Ruled out along the way:** mid-session `defn serve` restarts busting
the cache via tool-schema churn (active development of the very MCP tool
being used, so a real candidate) — turns following a commit showed
*lower* residuals than baseline, the opposite of what that hypothesis
predicts. Worth remembering as a ruled-out dead end if this residual ever
resurfaces, so it isn't re-investigated from scratch.

The remaining ~3% residual on both sessions is unexplained but small
enough for directional use as-is.

## Model

Per real assistant turn:

```
total_context[i] = cache_read_input_tokens[i] + cache_creation_input_tokens[i]
new_tokens[i]    = total_context[i] - total_context[i-1]
```

A negative delta means a real compaction happened between turns (folds in
naturally from the recorded numbers; no separate marker lookup needed,
though `isCompactSummary` events in the transcript can cross-validate).

Given that event stream, `simulate(events, ttl_seconds, read_mult,
write_mult, write_inflation)` replays cost turn by turn: inside the TTL
window since the last turn, prior accumulated context is a cheap read and
this turn's new content pays the write rate times `write_inflation`;
outside the window, the *whole* accumulated context plus new content pays
the write rate (uninflated -- a full rebuild already accounts for
everything, once). A compaction turn itself pays the write rate on the
new (smaller) post-compaction size. Two calibration fixes landed in order
(see git history): charging compaction turns for their rebuild (-16.8% ->
-14.0%), then applying the measured write-inflation factor (-14.0% ->
+2.6%).

## Usage

```bash
# Calibrate against a real session at a given TTL/pricing assumption --
# do this FIRST on any new session before trusting counterfactual runs.
python3 simulate.py <session.jsonl> --ttl 3600 --calibrate

# Counterfactual: what would this session have cost on a different TTL tier?
python3 simulate.py <session.jsonl> --ttl 300 --write-mult 1.25
```

`--read-mult`/`--write-mult` default to 0.10/2.0 (the 1-hour tier's
published multipliers vs. base input price at time of writing). Pass
`--write-mult 1.25` for the 5-minute tier's lower write premium.

## Next steps (not yet built)

- Chase the remaining ~3% residual (unexplained, but small enough to be
  low priority relative to the items below).
- Extend beyond raw Anthropic cache economics to defn's own dedup
  mechanism: sweep `staleEpochThreshold` values against real repeat-target
  data (see the read-locality analysis this tool's motivating
  investigation produced) to check whether 1 is actually the right choice
  or just a reasonable-sounding default.
- `write_inflation` is auto-estimated per session from its own gap<60s
  turns, which is a real strength (self-calibrating, no hardcoded
  constant) -- but its own stability hasn't been checked across dozens of
  sessions, only two. Worth confirming it clusters (5-6x-ish) rather than
  varying wildly before leaning on it for cross-session comparisons.
