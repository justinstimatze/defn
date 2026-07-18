# Expand arm — static turn-count projection

*Cheap-test #1 before firing paid runs. 2026-07-17.*

Goal: for each of the 10 turns in `turns.txt`, project the expected
`tool_use` count for three arms:

- **files** — actual from 2026-07-11 bench
- **defn-forced** — actual from 2026-07-11 bench (best defn config)
- **expand-arm** — projected: replace multi-hop read patterns with
  one `code(op:"expand", include:[body,callers])` call

## Ground rules for projection

- Only patterns matching MVP `include:[body,callers]` are counted as
  eliminated. Other multi-hop patterns (callees, tests, file-siblings)
  are NOT credited — those are v2 scope.
- Write turns (create/edit) are unchanged from defn-forced.
- Verify turns (go build, go test) unchanged from files (one Bash).

## Per-turn analysis

### Turn 1: "Look at middleware.Timeout and explain middleware pattern"

Nature: exploratory read + explanation. Wants body + context (callers
to understand how Timeout is used).

- files: **20** (from bench actual, includes 6 Read + 8 Bash + 3 Write + 3 Edit)
- defn-forced: **36** (from bench actual)
- expand-arm projection:
  - Turn 1 in defn-forced: ~9 read + 5 search + rest (from bench notes)
  - Realistic expand collapse: 3-5 `expand` calls replace ~9 `read` +
    ~2-3 `impact` calls that would have been made in a natural
    exploration flow
  - Projected: **~24-28** — modest win over defn-forced but likely
    still ABOVE files' 20

**Verdict: expand does not beat files on turn 1.** Multi-symbol
authoring turns are structurally files-favored (one Write = many
creates); expand doesn't touch that side.

### Turn 6: "Which functions call chi.Middlewares? What'd be affected?"

Nature: pure blast-radius query. Perfect fit for `expand
include:[callers]`.

- files: **1** (Bash grep -rn Middlewares)
- defn-forced: needs `impact` at minimum. Bench actual was 1 tool_use
  in files but defn-forced likely used impact + follow-up reads to
  answer "affected" question fully. Rough estimate: **3-5** tool calls.
- expand-arm projection:
  - `code(op:"expand", name:"chi.Middlewares", include:["callers"])`
    returns type body + all callers in one call
  - The "what'd be affected" question requires looking at callers,
    which are already in the expand response
  - Projected: **1-2** tool calls

**Verdict: expand ties or beats files on turn 6.** This is the
strongest target pattern for expand.

### Turns 2, 3, 7, 8: write turns (create middleware, tests, refactor)

Nature: multi-symbol authoring. Files-mode's one `Write` beats N
`create`s regardless of read strategy.

- No projection change from defn-forced. Expand is read-only.

### Turns 4, 5, 9, 10: verify turns (go build, go test)

Nature: single Bash. All arms already at 1 tool call.

- No projection change.

## Aggregate projection

| Turn | files | defn-forced | expand-arm (proj) | Δ vs files |
|---|---:|---:|---:|---:|
| 1 (explore) | 20 | 36 | 24-28 | +4 to +8 |
| 2 (write) | 1 | 9* | 9 | +8 |
| 3 (write) | 1 | 5* | 5 | +4 |
| 4 (verify) | 1 | 1 | 1 | 0 |
| 5 (verify) | 1 | 1 | 1 | 0 |
| 6 (query) | 1 | 3-5 | 1-2 | 0 to +1 |
| 7 (refactor) | 2 | 3* | 3 | +1 |
| 8 (refactor) | 2 | 2* | 2 | 0 |
| 9 (verify) | 1 | 1 | 1 | 0 |
| 10 (verify) | 1 | 1 | 1 | 0 |
| **TOTAL** | **31** | **61** | **48-54** | **+17 to +23** |

*starred defn-forced values are estimated splits from 61 total by
turn character; not from the CSV directly.

## What the projection says

**Expand v1 (MVP, body+callers only) is projected to close ~15-30% of
the defn-vs-files gap on turn count, but not reach parity.**

- Best case: 48 tool calls (files 31 = +55%)
- Worst case: 54 tool calls (files 31 = +74%)
- Baseline defn-forced: 61 tool calls (files 31 = +97%)

**Gate 1 (turn count ≤ files 31) FAILS in projection.** Expand v1
does not, on paper, get us to parity because:

- Write turns dominate the gap and expand is read-only
- File-siblings would collapse turn 1's `read`s but is v2 scope

## What this means for firing paid runs

**Do not fire paid session-bench runs on expand v1 yet.** The
projection says the delta will be smaller than defn-forced but still
+50-70% vs files-mode. That's not the "materially better" bar the
efficiency-floor principle demands.

**Better options before paying:**

1. **Ship `include:file-siblings` first** (v2 kind). Turn 1 could
   collapse `read middleware.Timeout` → `expand file-siblings` and
   see the whole middleware/ dir in one call. Likely closes another
   5-10 tool calls on turn 1.
2. **Design a bench where reads dominate.** turns.txt is
   write-heavy — 4 write turns + 4 verify turns. A pure-exploration
   bench (tasks 6-type across a bigger repo) would put expand in its
   strongest workload and let us prove/disprove the design cheaply.
3. **Static-only:** run the byte micro-bench (cheap test #2) to at
   least confirm expand is byte-competitive on the patterns it does
   cover, before deciding whether to build v2 include kinds.

## Recommendation

Do NOT fire paid runs on turns.txt with expand v1 alone. Options in
priority order:

1. Continue to cheap test #2 (byte micro-bench) — confirm the
   design isn't inflating on the patterns it does cover.
2. Ship `expand include:file-siblings` (v2 include kind, ~1-2h).
3. Author a read-dominated bench script and re-project. If that
   projection shows parity or better, fire that bench (not
   turns.txt).

Then paid run. Total cheap-work: 3-5h. Total paid: one arm ~$15-25.
