# cache-sim

Replays real Claude Code session transcripts through an Anthropic
prompt-cache cost model, so "what if TTL/pricing/policy X were different"
questions get an actual number computed from real recorded trajectories
instead of another round of hand-written one-off analysis scripts.

## ⚠️ Known calibration gap — read before trusting any number

As of 2026-08-05, `simulate.py --calibrate` reproduces real recorded
cache_read/cache_creation totals to within **~14% error** on the one
session it's been validated against (the defn project's own long-lived
mega-session). That is **directional accuracy, not measurement-grade
accuracy** — good enough for "does A beat B, roughly how much" questions,
not for precise dollar claims.

**Leading unverified hypothesis for the gap:** the session this was
calibrated against repeatedly rebuilt and restarted `defn serve`
mid-session (active development of the very MCP tool being used). Each
restart likely changes the registered `code` tool's schema, which busts
Anthropic's prompt cache — a different tool schema is a different prefix
— but the simulator's compaction detector only looks for context
*shrinking* (`total_context[i] < total_context[i-1]`), so schema churn at
similar or larger size is invisible to it. **Not confirmed.** Chasing
this down means checking whether tool-schema hash/version changes
correlate with the turns where simulated cost diverges most from real
cost.

Until that's resolved: use this tool's output as **directional evidence**
(which of two scenarios costs more, roughly how much) — the same
epistemic status as the bucketed gap-vs-cache-read-fraction analyses that
motivated building it in the first place — not as a precise cost oracle.

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
write_mult)` replays cost turn by turn: inside the TTL window since the
last turn, prior accumulated context is a cheap read and only this turn's
new content pays the write rate; outside the window, the *whole*
accumulated context plus new content pays the write rate, since the cache
had fully lapsed. A compaction turn itself pays the write rate on the new
(smaller) post-compaction size — that omission was the first calibration
fix (see git history), cutting error from -16.8% to -14.0%.

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

- Chase the calibration residual (tool-schema-churn hypothesis above).
- Extend beyond raw Anthropic cache economics to defn's own dedup
  mechanism: sweep `staleEpochThreshold` values against real repeat-target
  data (see the read-locality analysis this tool's motivating
  investigation produced) to check whether 1 is actually the right choice
  or just a reasonable-sounding default.
- Validate against a second, independent session (e.g. gas6amus's stope)
  before trusting the calibration error is representative rather than
  specific to this one session's unusual "developing your own MCP tool
  while using it" shape.
