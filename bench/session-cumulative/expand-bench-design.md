# Read-dominated session bench — design spec for expand op

*2026-07-17. Cheap-test #2 output.*

## Motivation

The 2026-07-11 `turns.txt` bench (add a rate-limit middleware) is
write-heavy: 4 write turns + 4 verify turns + 2 read turns. Static
projection (`./expand-projection.md`) shows expand v1 closes only
15-30% of the defn-vs-files gap on that workload because expand's
mechanism (multi-hop read collapse) can't touch write or verify turns.

Expand's cleanest test is a workload where reads dominate. This spec
designs one that matches expand's actual mechanism, then projects
before firing paid runs.

## Workload class we want

- **Read patterns dominant.** ≥80% of turns should require the model
  to inspect existing code.
- **Multi-hop natural.** Questions naturally trigger read→impact→read
  or read→search→read chains under files/defn today.
- **Answer verifiable.** Each turn produces an artifact (explanation,
  code map, refactor plan) that can be scored for correctness against
  ground truth.
- **Realistic-shaped.** Reflects the "understand this codebase then
  make a targeted change" workflow that dominates real Claude Code
  usage per the OpenHands trajectory analysis (memory:
  project_trajectory_analysis_2026_07_11).

## Repo choice

Use `github.com/go-chi/chi` again for:
- Cross-bench comparability with the 2026-07-11 write-heavy result
- Already ingested with defn in scratchpad/delta-bench/chi
- Small enough to fit in context, dense enough to have real
  call-graph structure (Mux, Router, Middlewares, ServeHTTP form a
  natural interconnection to explore)

Alternative if chi is too small: gin (also already ingested).

## Turn script (10 turns, target: `chi-explore.txt`)

```
1. Explain how chi.Mux handles routing. What's the flow when
   ServeHTTP is called? Focus on the top-level dispatch, not the
   trie internals.
2. Which functions in chi call routeHTTP directly, and under what
   conditions? Include test callers if any.
3. What's the relationship between chi.Mux and chi.Router? Is Router
   an interface Mux implements, or something else?
4. If I wanted to change the signature of Mux.Handle, which
   production callers would break? Which tests would need updates?
5. Explain chi.Middlewares. Where is it used, and what pattern does
   it enforce for users?
6. Show me how chi.Chain is constructed and what it returns. Include
   any type conversions in the chain.
7. Walk through the code path for a request matching a nested
   sub-router. What files does it touch, from ServeHTTP to the
   handler being invoked?
8. Which middleware in the middleware/ directory uses context values?
   List each with the key type it stores.
9. In chi.Mux.Route, how does the sub-router get its middleware
   inherited from the parent? Show the exact mechanism.
10. Summarize the extension points chi exposes to users — where can
    external packages plug in?
```

## Why these turns

Each maps cleanly to a multi-hop read pattern that expand v1
(body+callers) or expand v2 (+ file-siblings, tests) would collapse:

| Turn | Nature | Multi-hop pattern | Expand kind needed |
|---|---|---|---|
| 1 | Explain flow | read ServeHTTP + read callees | body + callees (v2) |
| 2 | Query callers | read routeHTTP + impact | body + callers (v1) |
| 3 | Relate 2 types | read Mux + read Router | body + file-siblings (v2) |
| 4 | Blast radius | read Handle + impact | body + callers (v1) |
| 5 | Explain type usage | read Middlewares + impact | body + callers (v1) |
| 6 | Chain code | read Chain + callees | body + callees (v2) |
| 7 | Cross-file trace | read ServeHTTP + trace | body + callees + file-siblings (v2) |
| 8 | Sweep + query | search + N × read | file-defs (existing) |
| 9 | Mechanism | read Route + read subroute code | body + file-siblings (v2) |
| 10| Summary | overview | overview (existing) |

## Re-projection after phantom-callers fix (be4baa8, 2026-07-17)

Prior to fix be4baa8, `impact`/`expand` on any Mux method returned 5-10×
phantom callers (routeHTTP: 26 reported, 2 real). This poisoned BOTH
defn arms because the model chases phantoms — reads phantom callers,
loses context to noise, and doesn't converge on the right answer in
1-2 turns.

Post-fix:
- `impact routeHTTP` returns 2 callers, matching `grep` in files-mode.
- `expand routeHTTP` shows precise 2-caller list.
- Turns 2, 4, 5 (blast-radius questions) now have clean signal.

Effect on projection: **both defn arms drop 3-6 tool calls** they
previously would have burned on phantom-chase Reads. The defn-vs-files
gap tightens materially even BEFORE the expand op enters the picture.

Restated projection:

| Arm | Pre-fix projection | Post-fix projection |
|---|---:|---:|
| files | 21-26 | 21-26 (unchanged — grep is always precise) |
| defn-today (impact + read) | 26-34 | **21-27** |
| expand-v1 forced | 17-23 | **15-20** |

The gap between defn-today and expand-arm narrows: fixing phantom
callers helps defn-today MORE than expand (because expand's one-call
collapse was already burning fewer turns). But both arms move closer
to files-mode parity.

**Firing recommendation is stronger after the fix**: we're now testing
a defn that is fundamentally competitive on precision. Whether expand
adds value on top is the actual expand question, not muddled by a
caller-graph bug.

## Per-turn tool-call projection (v1 body+callers only, pre-fix)

| Turn | files (est) | defn-today (est) | expand-v1 (proj) |
|---|---:|---:|---:|
| 1 | 3-5 (read files + grep) | 4-6 (read + impact + reads) | 3-4 (expand + reads) |
| 2 | 1-2 (grep + explanation) | 2 (read + impact) | 1 (expand callers) |
| 3 | 2-3 (2 reads) | 3-4 (2 reads + file-defs) | 2 (2 expand+file-siblings)* |
| 4 | 2 (grep + read) | 2 (read + impact) | 1 (expand) |
| 5 | 2-3 (grep + read) | 2 (read + impact) | 1 (expand) |
| 6 | 2 (2 reads) | 3-4 (reads + search) | 2 (2 expand) |
| 7 | 4-6 (multi-file trace) | 5-8 (many reads + impact) | 3-5 (expands + few reads) |
| 8 | 2-3 (grep + reads) | 2 (search + file-defs) | 2 (unchanged) |
| 9 | 2 (2 reads) | 2 (2 reads) | 1-2 (expand) |
| 10| 1-2 (search) | 1 (overview) | 1 (unchanged) |
| **TOT** | **21-30** | **26-36** | **17-25** |

*v2 kinds (`file-siblings`, `callees`) are the strongest wins; v1
alone still shows a projection win because turns 2, 4, 5 are pure
`read+impact→expand` collapses.

## What the projection says

**Expand v1 projects to 17-25 tool calls vs files 21-30 and
defn-today 26-36.**

- Expand v1 alone beats files-mode by 0-25% projected
- Expand v1 beats defn-today by 25-45% projected
- The gap widens with v2 include kinds

This is the workload class the design was aimed at. **Projection
supports firing one paid arm** if we accept:
1. This bench measures the read-dominated workload, not the mixed
   workload turns.txt measures.
2. Both benches together give the honest picture: turns.txt shows
   expand doesn't help write-heavy, chi-explore shows it does help
   read-heavy.

## Correctness gate

Each turn's answer must be evaluated against ground-truth facts:

- Turn 1: mentions Mux.tree, routeHTTP, method-specific dispatch
- Turn 2: names correct set of routeHTTP callers
- Turn 3: says Router is an interface, Mux implements it
- Turn 4: identifies at least 3 callers correctly
- Turn 5: identifies Middlewares as []func(http.Handler) http.Handler
- Turn 6: mentions Chain returns a Middlewares
- Turn 7: names files ServeHTTP → routeHTTP → Handler.ServeHTTP touch
- Turn 8: at least 2 middleware use context correctly identified
- Turn 9: mentions Route creates sub-mux and inheriting behavior
- Turn 10: names at least 3 extension points (Handler, Middlewares,
  Router interface)

Ground truth verified against chi source before running the bench.
Answers evaluated by rubric or LLM-judge (measure both).

## What we DON'T need to build first

- No new expand include kinds. v1 body+callers is enough to show a
  signal on 3 of 10 turns (2, 4, 5). If those alone project to a
  parity or win, we ship v1 and iterate.
- No new bench harness code — same `run-arm.sh` works with a new
  `chi-explore.txt` turns file.

## Estimated cost to run

- Files arm: ~$5-10 (10 turns, read-heavy dominated by cached prefix)
- Defn arm (expand-enabled): ~$5-10
- Total: ~$10-20 for one comparison — cheaper than turns.txt because
  no write/verify turns (which had huge Turn-1 tool counts).

Same cost decision point as before: fire only if this projection
holds AND we accept the workload-class narrowing.

## Sequencing

1. Author `chi-explore.txt` turns file. **(30m)**
2. Verify ground-truth answers against chi source (grep + code MCP).
   **(1h)** — REQUIRED before firing. If we don't know the right
   answer, we can't grade the model's output.
3. Static-project both arms per this design. **(done above)**
4. If projection still holds after ground-truth check, fire both
   arms. **(~$10-20)**
5. Analyze: turn count, weighted cost, correctness. Decide on ship.

## Success gates for this bench

Cannot promote expand to preamble without:

1. **Turn count.** Defn arm ≤ files arm. Not "close to" — at or below.
2. **Weighted cost.** ≤ files arm on total input + cached ($15/M + $1.5/M).
3. **Correctness.** ≥8/10 rubric score on both arms, no regressions.

If (1) passes but (2) fails: response bytes need trimming (see
byte-bench findings — collapse test-caller listing to summary).
If (1) fails: expand doesn't move the needle even on its target
workload; either the include set is wrong or the model isn't
adopting expand under the preamble.

## What this design deliberately does NOT claim

- Not claiming this bench proves expand wins in general. It measures
  ONE workload class: exploration/analysis of an existing codebase.
- Not claiming files-mode is generally worse. This bench is designed
  to isolate expand's mechanism against a read-heavy workload;
  turns.txt shows expand doesn't help write-heavy.
- Both benches together are the honest picture. Marketing needs to
  cite both.
