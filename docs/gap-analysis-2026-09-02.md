# What stands between defn and "superior to files mode" — 2026-09-02

Analysis pass (Fable, cheap) to hand to the next session (Opus) as a
work order. Every number below is already on record in
`docs/lessons-learned.md` or winze-memory; nothing here came from a new
bench run. Re-verify any mechanism named here against `git log`/source
before building on it — this doc goes stale the same way the v10 entry
did.

## 1. Ground truth — the pooled numbers (don't re-derive)

| comparison | correctness | cost | n | notes |
|---|---|---|---|---|
| prom-opus v3+v4 pooled (15 tasks × 2 runs, Opus) | F1 0.780 vs 0.782 — **tied** | **+24%** ($1.405 vs $1.131/task) | 30 | most trustworthy baseline; v3-only "defn wins" did not replicate |
| etcd-multifile-v2 (3 tasks, Sonnet) | F1 0.933 both — tied | **1.32×** | 3 | gap concentrated in etcd-21620 |
| head-to-head-go n=20 (Sonnet, v0.26.22) | F1 0.676 vs 0.680 — tied | defn cheaper ($2.97/$3.70 vs $4.24) | 20/10 | only corpus where cost favours defn; files arm run once |
| session-cumulative 07-11 (chi, Opus, 10 turns) | both correct | +40% natural, +28% forced | 1 | write-heavy authoring |
| small-slice n=9 rerun (search caller/test counts) | 9/9 rc=0 both | −5.6% cost, +7% wall | 9 | noise-dominated; not a win |

Structural facts from trajectories:
- ~77% of calls are read-shaped; `context`/`expand` (the consolidators)
  get ~0.7% spontaneous use. Both in-band nudges: 0/7, 0/19 follow-through.
- Adoption is **additive**: 25.6% of calls in a defn-arm session are
  still native Read/Grep/Glob. Every defn call that doesn't *replace* a
  native call is pure added cost.
- Per-tool-result payload is heavier: etcd-21620 mean 3,734 B (defn) vs
  469 B (files); even excluding the 3 outlier calls, 930 B vs 469 B.
- Cost gap is tail-dominated: 4–5 tasks/15 (12024, 19017, 18534, 16766,
  21620) drive most of the pooled gap; those are the ones where a write
  op failed or a rename/field op no-op'd and the model burned 15–25
  calls recovering.

## 2. Scorecard by axis of the vision

| axis | state | verdict |
|---|---|---|
| Total session cost (the CLAUDE.md bar) | +24% pooled on the powered corpus | **losing** |
| Wall-clock | ~parity, high variance; per-edit emit+build adds latency on big repos (21620: 521 s outlier) | not winning |
| Correctness (F1 vs gold) | tied on every powered comparison | parity, not superior |
| Write atomicity / ref-graph consistency | real capability (rename/move/apply), but the bug class keeps surfacing (mergeDecls bail, receiver-qualified ambiguity, field-rename, escaping paths ×5) | **theoretical win, not yet trusted** |
| Discovery quality (context/expand/explain/plan) | model won't reach for it; unmeasurable while unused | unproven |
| Reliability (no silent garbage, no whole-call hard-fail on one bad row) | improving fast (fuzzgen hazard families, #357/#363/eb3420c audits) | trending right |

Net: defn today is a correctness-neutral tool that costs ~a quarter more.
The gap is not a model-behaviour problem we can nudge away (proven dead);
it is three mechanical costs stacked on the same call count.

## 3. Root-cause model of the cost gap

cost ≈ Σ over N calls of (context so far) — so anything that adds bytes
to *every* call compounds ~N². Three additive contributors, ranked by
how deterministic the fix is:

**A. Schema tax (never quantified precisely — highest-leverage unknown).**
The `code` tool description alone is **8,987 bytes** (server.go:573–582,
one string) before the input schema. Estimate ~2.2k tokens for the
description, likely 4–6k tokens total with schema. That prefix is
cache-read on *every* API call in the session. At Opus cache-read
(**$1.50/M assumed here — later corrected to the real $0.50/M, see
item 1**): 5k tokens × 40 msgs/task ≈ 200k tokens ≈ **$0.30/task** —
which is roughly the entire pooled gap ($0.27/task) on this first-pass,
uncorrected estimate; the real-data item-1 measurement below supersedes
it. Even if the true
figure is half that, it's the single largest deterministic contributor
and applies identically to every task, including tasks where defn is
never called (chi-explore: +12.6% with zero defn calls — that *was* the
schema tax, measured). Fix is model-behaviour-independent.

**B. Per-call payload enrichment the model doesn't consume.** Related
footers, provenance tags, starter bundle, search JSON, outline-downgrade
notes, nudge text — each was added to save a follow-up call, but the
0.7%/0-of-19 numbers say the follow-up-avoidance rarely materialises,
while the bytes are paid on every call. Baseline read-class result is
~2× files-mode's. Existing `arm_defn/*.json` trajectories already
contain per-result byte counts — a bytes-by-op histogram costs nothing.

**C. Tail events from write-path failures.** One failed `rename`/`edit`/
`create` costs 15–25 recovery calls (each of which drags the full
context). This is what the trajectory-mining cycle has been fixing one
bug at a time; it works but is reactive. The mean is set by the tail.

Not a contributor worth more investment: apply-batching (caps at ~7%
call reduction — v10), nudges (dead), hard gating (#209 backfired),
auto-batch hijack (#312 reverted).

## 4. Where defn can *structurally* win (vs merely reach parity)

The bug-fix corpora (SWE-style) are read-dominated on repos the model
already has priors on — the regime where a good native agent is
hardest to beat (see `Feedbackbenchvsrealbaselines`). defn's real
asymmetries are:
- cross-package `rename`/`move`/signature change: 1 call vs 10–30
  Edit calls + grep loops;
- `test` scoped to a def vs `go test ./...` on a large module;
- multi-file authoring via multi-decl `create` (already shipped — the
  07-11 "13 creates vs 1 Write" gap is closed on paper, never re-measured);
- guaranteed-consistent ref graph after edits (no stale-grep misses).

No corpus currently exercises these. Recommend a **refactor-shaped
corpus** (10 tasks: rename exported symbol across packages, move a def
between packages, change a signature with N callers, extract a helper
used in K files) run *alongside* the bug-fix corpus — not instead of it,
since CLAUDE.md requires winning on winze's real shape too. If defn can't
win there either, the thesis is wrong, not the bench.

## 5. Work order for the Opus session, ranked

1. **DONE (2026-09-02, same day as this doc).** Quantified the schema
   tax exactly, mechanically, no estimate. Built an in-process probe
   (`internal/mcp` test, since deleted — it spun up the real
   `newMCPServer`, connected an in-memory MCP client via
   `sdkmcp.NewInMemoryTransports`, called the real `tools/list` RPC,
   and captured the literal wire JSON for the `code` tool) instead of
   reasoning about the description string in isolation. Result: the
   `code` tool's full `tools/list` entry (name + description + input
   schema, as actually generated by reflection over `codeParam`) is
   **13,915 bytes / 3,171 tokens** (tiktoken cl100k_base proxy) — not
   the ~9 KB description-only / "4-6k tokens" guessed in the first pass
   of this doc; the real schema (52-field struct) adds real weight
   beyond the description string.
   Combined with **real** per-task API-call counts (assistant-message
   count in `fncall_messages`, i.e. actual LLM round-trips, not a
   guessed "40 msgs/task"):
   - **prom-opus arm_defn** (15 tasks, local run): mean 50.2 calls/task
     (range 14–97). Tax = 3,171 × 50.2 × $0.50/M (Opus cache-read —
     **corrected 2026-09-09**: a least-squares fit against real billing
     data in `bench/session-cumulative/2026-08-07-session-usage.csv`
     [40 rows, max residual $0.0005] found actual `claude-opus-5`
     cache-read at $0.50/M, not the $1.50/M this doc originally assumed
     from the generic "10% of input price" heuristic; independently
     re-verified against 2 of those rows by hand before editing this) =
     **$0.080/task ≈ 29% of the pooled $0.274/task gap.**
   - **etcd-multifile-v2 arm_defn** (3 tasks, Sonnet): mean 27.3
     calls/task. Tax = 3,171 × 27.3 × $0.30/M (Sonnet cache-read) =
     **$0.026/task ≈ 36% of that corpus's $0.072/task gap.**
   Both corpora say the same thing from independent data: the schema
   tax is the single largest deterministic contributor to the cost
   gap, model-behavior-independent, present on every call whether or
   not the model ever invokes `code` (consistent with chi-explore's
   +12.6% at zero defn calls). **R2 is the main event, not a side
   quest — confirmed, not assumed.**
2. **DONE (2026-09-02, same session as item 1).** Shipped the lean tool
   surface: `internal/mcp/tool_help.go` adds `leanToolDescription`
   (1,144 bytes, one-line-per-op grouped by Read/discover, Write,
   Plan/verify, Other) alongside the untouched `legacyToolDescription`
   (8,950 bytes), plus an `opHelp` map carrying the full long-form
   per-op guidance (all 47 real ops — including 7 that were never
   documented anywhere before this: `context`, `test-coverage`,
   `batch-impact`, `file-defs`, `methods`, `insert-header`,
   `resummarize` — and correctly dropping the 5 Dolt-era ops the old
   description still advertised after they were removed in the v0.27
   migration: `diff`, `history`, `commit`, `status`, `diff-defs`),
   served on demand via the new `op:"help", topic:"<op>"`.
   `toolDescription()` picks between them via `stripped("verbose-tool-desc")`
   — same `DEFN_STRIP` plumbing as every other feature flag in this
   codebase — default was initially left unchanged (legacy) pending
   the powered A/B (item 6). **Flipped to default 2026-09-03 (item
   7b)**: two independent real-corpus validations (this doc's item 1
   and item 7b) plus zero functional downside (142 dependent tests
   pass, same ops/behavior, long-form guidance still available via
   `op:"help"`) made holding it behind a flag indefinitely just
   deadweight — the "powered A/B" gate was reserved for genuine
   behavior-risk changes, and this isn't one. `DEFN_STRIP=lean-tool-desc`
   now reverts to the old `legacyToolDescription` if a live session
   ever shows the model needs the inline long-form guidance more than
   the wire-cost savings.
   Measured via an in-process `tools/list` probe (not estimated):
   **legacy total 14,037 B / lean total 6,121 B — 56.4% smaller wire
   payload; description alone 8,950 B → 1,144 B, 87.2% smaller.**
   Token-equivalent: ~3,171 → ~1,454 tokens per call (tiktoken
   cl100k_base proxy). Projected task-cost effect using the SAME real
   call counts as item 1: prom-opus (50.2 calls/task, Opus cache-read
   **corrected 2026-09-09 to the real $0.50/M rate, see item 1's own
   correction — was $1.50/M**) saves ≈$0.043/task ≈ 16% of the pooled
   $0.274/task gap; etcd-multifile-v2 (27.3 calls/task, Sonnet
   cache-read) saves ≈$0.014/task ≈ 20% of that corpus's $0.072/task
   gap (Sonnet's $0.30/M rate not re-verified against real billing data
   the way Opus's was — flagged, not corrected). **Not yet
   re-measured on a live bench run — this is the same real-token-count
   math as item 1, not a fresh trajectory replay; item 6 is where that
   gets confirmed.** All 142 tests statically affected by `handleCode`
   pass. Scope cut from the original plan: did NOT implement
   "auto-appended to the first error for that op" — `handleCode`'s
   defer-based response pipeline is 800+ lines and touching its error
   path uniformly felt like a separate, riskier change; flagged as a
   follow-up, not done. Also filed
   `bug-report-2026-09-02-create-duplicates-shared-import-alias.md`:
   `code(op:"create")` into a file whose body imports an alias already
   used elsewhere in the package emits a duplicated import line
   (build-breaking) — hit twice reproducibly while building this
   change; root cause not yet traced into `internal/emit`.
**Reranked 2026-09-02, after items 1-2 landed** (see the matching note
in `docs/lessons-learned.md`'s Handoff section for the full rationale):
items 5/6 below both spend real EC2 money, and the harness
snapshot-cache-collision theory (formerly an open question in §6)
means any EC2 result today is unverifiable — fixed before that spend,
not after, or item 6's "no more single-run reruns" rule can't actually
be enforced. The create-duplicates-import bug (found while building
item 2) is cheap and confirmed, so it jumps ahead of the two $0
analysis items too — closing it now is cheaper than risking it
silently corrupting something item 3's own tooling touches.

3. **DONE 2026-09-02, same day, later session.** Harness snapshot-cache-
   collision risk: `agent_driver.py`'s `.defn` cache key now includes
   `_defn_source_tree_hash()` (defn repo's own HEAD + uncommitted-diff
   hash) alongside `_defn_binary_hash()` — an independent invalidation
   signal for the grpc-go-2629 rename theory (a stale `which defn` PATH
   resolution could otherwise make two different defn source states
   hash to the same cached binary). Additive-only to the cache key, so
   it can only cause more cache misses, never fewer.
4. **DONE 2026-09-02, same day, later session.** Fixed
   `bug-report-2026-09-02-create-duplicates-shared-import-alias.md` —
   root cause was NOT `internal/emit`'s import-merging pass; it was
   `handleCreate`'s single-decl path never stripping a leading
   `import (...)` block (unlike the multi-decl path's `sliceDecls`).
   Fixed by reusing `sliceDecls` for the strip and making the existing
   `#367` aliased-import-patch mechanism run unconditionally (idempotent).
   Confirmed via live repro against internal/mcp's own `sdkmcp` alias.
   Regression test:
   `TestHandleCreate_SingleDeclLeadingImportBlockNotDuplicatedInEmittedFile`.
**Reranked again 2026-09-02, same day, after items 3-4 also landed**:
swapped 5/6 below. With both $0 analysis items still untouched, the
same logic that jumped item 4 (a cheap, confirmed, mechanical bug fix)
ahead of the two analysis items applies one level down — the tail-event
detector is the automated version of the exact bug-hunt that produced
item 4, so running it first gives it one more cycle to surface another
cheap fix before the histogram's findings and the EC2 spend below lock
in. 7, 8, 9 keep their relative position; only their gate references
move to the new numbers.

5. **DONE 2026-09-02, same day, later session.** Built
   `bench/tail_event_detector.py`: flags any defn error/no-op result
   followed by ≥5 calls before the next successful write, ranked by
   calls burned — the bug-hunt queue, automated, reusing
   `mine_trajectory.py`'s `FRICTION_PATTERNS` (imported, not copied).
   Run against all 62 trajectories on disk: 48 flagged events (24 after
   excluding the stale pre-fix `bench/prometheus-repo/arm_defn` corpus).
   **Key finding**: the #1 hit (up to 38 calls burned) was "Config named
   msk/lightsail is already registered" — a test binary panic. Traced
   it (fresh prometheus clone, confirmed `go test -run
   TestMSKDiscoveryRefresh ./discovery/aws/...` passes cleanly with no
   defn involvement) and found it's **already fixed**: commit `7d66258`
   (2026-08-10) added `source_file` to `definitions`' UNIQUE constraint
   for this exact symptom. The flagged trajectory predates the fix by a
   day; the same 2 task IDs re-run in the newer `prometheus-repo-opus`
   corpus (2026-08-20, post-fix) show zero flagged events — confirms the
   fix holds. **Calibration lesson, now in the script's own docstring**:
   bench trajectories span 2026-07-22 to 2026-08-24 and real fixes have
   landed throughout, so a flagged event may be an already-fixed bug,
   not a live one — check `git log -S` on the trigger and look for a
   newer rerun before trusting a hit.
   **(a)+(b) followed up, same session**: `grpc-go-3476`'s full
   transcript shows `code(op:"sync")` hitting "dialoptions.go:34:2:
   backoff redeclared in this block" for grpc-go's ROOT package — same
   "X redeclared in this block" signature, same date (2026-07-22), as
   `go-zero-1907`'s. `dialoptions.go` sits in the package nearly every
   other grpc-go package imports, so once it holds a duplicate decl (the
   pre-`source_file` collision `7d66258` fixed), any whole-module
   `go/packages.Load` inherits a poisoned graph — which explains the
   rest of that trajectory too: the 5× `replace-hunk: hunk not found`
   hits were the model dry-run-probing for `cmp`/`len(a)` text that a
   fresh grpc-go clone confirms was never actually in those functions
   (model guessing wrong, not a matcher bug), and a `dry_run:true` edit
   call — which `handleEdit` returns from immediately, no build/emit —
   hanging 1801s until the client aborted is best explained by
   dependency resolution getting stuck on that same poisoned graph.
   High confidence, not directly confirmed (no post-fix rerun exists for
   either task, unlike prometheus above).
   **(c) still open**: model hallucinates a nonexistent op `"ingest"`
   across 4 go-zero tasks (13-20 calls each, all recovered) — a naming-
   confusion issue, not a code bug, possibly already helped by today's
   item 2 lean-tool-description + `opHelp`; lowest severity of the
   three (self-recovering), leave for whenever a fresh corpus exists.
6. **DONE 2026-09-02, same day, later session.** Built
   `bench/payload_histogram.py`: bytes-by-op histogram (reusing
   `tail_event_detector.py`'s call/result pairing) plus a mandatory
   token cross-check per `bench/tokens.py`'s own rule. Filters
   `unknown op "..."` noise and the Dolt-era `REMOVED_OPS` before
   computing stats. 1928 real calls / 29 ops; 8 exceed the 470 B median
   baseline by bytes, but 4 of those (`context`, `expand`, `pragmas`,
   `file-defs`) are low-volume, explicit consolidation/listing ops —
   big by design, not bloat. The real question is `test` (n=235),
   `impact` (n=22), `overview` (n=118), `read` (n=477, ~25% of all
   calls). **Token cross-check flips the read on `read`**: 833 B median
   but only 90 token median — the exact byte-vs-token trap
   `bench/tokens.py` warns about (Go's indentation is byte-heavy,
   token-cheap) — its "over baseline" byte flag is mostly a false
   alarm. **Directed measurement**: `read`'s Related footer is 17.4% of
   bytes / 19.7% of tokens (median, n=281), ~56-59 tokens/call; whether
   it earns that back by avoiding a follow-up `impact`/`outline` call
   is unmeasured (same adoption-tracking gap as the starter bundle's
   open question). **Real, current (not stale) tail**: `impact`'s max
   (45,410 B / 12,663 tokens) is from `etcd-multifile-v2` (2026-08-20,
   post the Aug-10 200-item cap fix) — a genuinely high-blast-radius
   type hitting the cap, not an uncapped runaway; whether 200 items is
   the right budget is a design question, not a bug. Did NOT gate item
   7 on this — nothing here rose to "confirmed bug, fix before EC2
   spend" the way item 4 did; the three leads found (footer adoption,
   impact's per-item budget, overview's tail) need a fresh corpus or a
   product call, not a quick fix.
   Gates item 7 — fix free bloat before paying for a pilot to measure it.
7. **DONE 2026-09-02/03.** Built the 10-task refactor corpus and ran
   the Sonnet pilot on both arms. Raw numbers looked bad for defn (mean
   F1 0.78 vs files' 0.86) but root-causing every gap instead of taking
   the numbers at face value found: two real, now-fixed-and-confirmed
   defn bugs (`safeWriteGoFile` stripping file mode to 0644 on every
   write; `handleTestByName` falling back to a fully unscoped
   `emit.Emit()` on a go-test package-path pattern like "./rest/..." in
   `test:`) that together explained a 14-phantom-file blowup on the
   go-zero move task — rerun against the fixed binary confirmed
   precision 0.41→0.90, matching files-mode exactly. And the single
   worst task (prometheus min/max rename, defn recall 0.18) turned out
   to be a harness confound, not a defn weak spot: its gold diff
   requires `goyacc` regeneration, a shell step files-mode's arm can
   run (`Bash` is in `FILES_ALLOWED_TOOLS`) and the defn arm structurally
   cannot (`Bash` is in defn's own `DISALLOWED_TOOLS`, by design).
   Excluding that task, defn's mean F1 across the other 9 (0.86) is
   within noise of files-mode's (0.87) — correctness parity, once
   measurement artifacts are stripped out. The cost/wall-clock gap
   (defn ~80% more $, ~2x slower) is separate and UNCHANGED by these
   fixes — still open. Full writeup: `bench/refactor-corpus/README.md`
   and `docs/lessons-learned.md`'s item 7.
7b. **DONE 2026-09-03, same corpus, cost decomposition.** Pooled the
    real per-task token/cost data across all 9 non-confounded
    refactor-corpus tasks (defn vs files, both arms' raw
    `.claude-stream.jsonl` pulled from EC2 — `total_cost_usd`,
    `num_turns`, and `usage.cache_creation_input_tokens` per task, not
    estimated). Total cost ratio 1.62× decomposes cleanly into two
    multiplicative factors: **call-count ratio 1.51×** (313 vs 207
    tool calls pooled) and **per-call weight ratio 1.26×** (1,403 vs
    1,116 fresh/cache-creation tokens per call, pooled). Two
    matched-call-count task pairs isolate the weight factor from the
    confound cleanly: `loopywriter-extract` (7 calls both arms, defn
    1.64× heavier/call) and `recode-signature` (defn 9 calls vs files'
    10 — defn made *fewer* calls, still 2.39× heavier/call). This
    confirms and refines §3's Lead B with a controlled method (matched
    call counts) the original byte-histogram approach couldn't use.
    **Tested and ruled out**: hypothesized that a repeat
    `read(full:true)` of an already-served def was paying the ~500B
    Related-footer cost (#202) again for nothing, and patched
    `handleGetDefinition` to suppress it using the existing
    `bodyServed`/`hasBodyServed` tracking (#176). A regression test
    proved this dead code before it shipped: `handleCode`'s own
    existing shortcut *already* short-circuits an unchanged repeat
    `read` (full or not) to a one-line "already read in this session"
    stub, well before reaching the footer logic — the exact waste this
    fix targeted is already eliminated by a coarser, pre-existing
    mechanism. Reverted the patch and the test; no functional change
    landed from this thread, which is the correct outcome, not a
    failure — a real test caught a redundant fix before it merged.
    **Validated (not just projected) the item-1/2 schema-tax fix's
    payoff on this specific corpus**: using the corpus's real per-task
    defn call counts (2–83) against item 1's measured legacy/lean
    token-equivalents (3,171 vs 1,454) at Sonnet's cache-read rate
    ($0.30/M — this corpus is Sonnet, not Opus like prom-opus),
    flipping the lean description on projects to **$0.161 saved of
    $6.169 total defn-arm cost ≈ 2.6%**. Free, zero-risk, already
    built and tested (item 2) — so flipped it to the default right
    here rather than leave a validated free win gated behind a flag
    (see item 2's updated note) — but confirms the schema tax is a
    much smaller slice
    of the gap on Sonnet/refactor-shaped tasks than on Opus/prom-opus
    (16% projected there — corrected 2026-09-09, was 47%, see item 2's
    own correction): call-count (1.51×) and per-call weight
    (1.26×) are the two real remaining levers here, and per-call
    weight's *fixable* (waste) component looks smaller than assumed —
    the #176 dedup machinery already catches the cheap win. Not
    re-litigated: apply-batching/nudges/hard-gating, per §3's own
    "not worth more investment" verdict — confirmed still true, no new
    evidence found to reopen it.
7c. **DONE 2026-09-03, same day.** Literature scan for external levers
    (user request: "look online for current avant garde ideas") came up
    mostly inapplicable to defn's actual constraints: Agent-Omit
    (arXiv:2602.04284) needs RL-training a custom model, not usable
    against a hosted Claude model; dynamic tool gating (arXiv:2604.21816)
    needs mid-session tool re-negotiation Claude Code's session model
    doesn't support; LOOP Skill Engine (arXiv:2605.14237) needs task
    *repetition*, which this one-off 10-task corpus doesn't have. Also
    checked and ruled out splitting the single 47-op `code` tool into
    several smaller tools: since every registered tool's full schema
    rides on every call regardless of which is invoked, splitting would
    duplicate the ~12 fields shared across most ops N times instead of
    once — the current consolidated-tool design is schema-cost-optimal,
    not naive.
    One idea *was* defn-native and shipped: `coupledChangeHint` (fired
    on a rolled-back signature-changing edit) only ever named up to 3
    callers, forcing a separate `read()` of each just to see its current
    call shape before writing a coupled fix — the literal mechanism
    behind cli-refactor-getcomment-signature's 8-hit churn. Added
    `findCallSitesInBody` (lightweight body-text AST scan, same
    established tradeoff as `extractSignature`/`prioritizeByBodyReference`
    — no type resolution, name-only match) so the hint now shows the
    actual call-site text inline per caller, cap raised 3→8 (matching
    `handleDelete`'s existing cap). New regression test
    `TestCoupledChangeHint_IncludesCallSiteText`; existing coupled-hint
    tests + 200+ affected tests across several partial full-suite runs
    show zero failures. Not yet re-measured on a live bench rerun of
    the affected task — that would be the natural next step if this is
    worth confirming quantitatively.
7d. **DONE 2026-09-07.** External bug report via MCP Dispatch from a
    winze session (real refactor, `rm`-ing 7 `.go` files), filed with
    authority against defn. Two real, unrelated correctness bugs, both
    fixed with regression tests:
    - **Root cause, not the reported symptom**: `resolve()`'s per-def
      ref collection only registers a def_id as a key in the `defRefs`
      map handed to `SetManyReferences` when at least one ref was found
      that pass (every case's append is gated on `len(refs) > 0`).
      `SetManyReferences`/`SetReferences` only clear a def's OLD refs
      for def_ids present as map keys — so a def edited down to ZERO
      outgoing refs (its last call removed) never got a key, and its
      stale ref survived every future resolve forever, not just until
      the next one. This is NOT the "#150 deferred resolve" bug the
      symptom first suggested — it reproduces even on a full,
      immediate, non-deferred `resolve.Resolve`. Confirmed via a
      debug probe: an explicit `ResolveFile` call after the edit still
      reported the stale caller. Fixed by registering `defRefs[fromID]
      = nil` immediately once `fromID` resolves, before any ref is
      collected, for all three cases (FuncDecl/ValueSpec/TypeSpec).
      New test `TestResolveFileClearsStaleCallRefWhenCallSiteRemoved`
      (internal/resolve) locks in the root cause directly; full
      `internal/resolve` suite (19/19) and 4 targeted `internal/mcp`
      sweeps (delete/rename/sync/apply, 600+ test invocations with
      overlap) show zero regressions.
    - **Separately real, `#150`-adjacent**: even with refs correctly
      clearable, `handleEdit`'s sig-stable fast path *defers* the
      resolve that would clear them until the next full sync or
      explicit `code(op:"sync")` — so `handleDelete`/`handleDeleteFile`'s
      #105 safe-delete caller-check, and `handleRename`'s caller-body
      rewrite, could still act on stale data within the same session.
      Added `markPendingResolve`/`drainPendingResolves` (server-scoped,
      mutex-guarded file set) — `handleEdit`'s deferred branch marks the
      file; `handleDelete`/`handleDeleteFile`/`handleRename` drain
      before trusting `GetCallers`. Cleared wherever a real resolve of
      that scope happens (`autoResolveFile`, `autoResolve`,
      `ingestAndResolve`). Zero-cost when nothing's pending (the common
      case). New test `TestHandleDelete_SeesSameSessionEditThatRemovedLastCallSite`.
    - **Also reported, narrower than described**: "no whole-file delete"
      was stale (`code(op:"delete", file:...)` shipped 2026-08-19,
      `a58ef77`/`86a3823`) — winze was likely on an older binary. But
      `code(op:"sync", file:<already-rm'd-file>)` genuinely still
      errored (`ingest file: parse ...: no such file or directory`)
      instead of pruning, since `ensureFresh`'s deleted-file healing is
      explicitly skipped for `op:"sync"` itself and `IngestFile`'s fast
      path had no missing-file branch of its own. Fixed in `IngestFile`
      (treat a missing file as zero live defs, reuse the existing
      `DeleteFile` cleanup) plus `handleSync`'s file/module paths
      (accurate messaging, skip the now-pointless `ResolveFile` call).
      New tests `TestIngestFile_MissingFilePrunesDefinitions`,
      `TestHandleSync_MissingFilePrunesInsteadOfErroring`.
    Replied to winze on dispatch with the (b) root-cause finding before
    fixing, per this project's "verify before reacting" habit — but
    this time verification itself (a debug probe) surfaced a DEEPER bug
    than either of us assumed going in.
7e. **DONE 2026-09-09.** User instruction: "figure out how to beat
    `code-review-graph`" (a real, direct competitor — Tree-sitter-based,
    read-only, went GitHub-trending on a "49x fewer tokens" headline;
    beat both defn arms on cost/call-count in a 2026-08-07 forced-
    substitution bench, n=1, never revisited). Delegated to a fresh
    agent with a full technical brief (standing rules, prior failed
    levers, real numbers) rather than continuing to theorize. Findings:
    - **crg's real mechanism** (read from its source, not its blog
      post): cross-file caller/rename resolution is bare-name matching
      with no type information (`callers_of("Close")` returns every
      same-named method in the repo); its rename is unguarded string
      replacement with no build gate; 30 MCP tools / 22.8 KB of
      docstrings (~5.5x defn's current lean schema); its own installed
      instructions tell its model "the graph is a hint, read the
      source, source wins," budgeted at ≤5 calls/task. It wins on cost
      by doing structurally less, not by a better architecture.
    - **Re-decomposed the 2026-08-07 loss at the correct price** (see
      the correction below) — the loss was NOT primarily schema/input
      tax, it was defn emitting 33-38% more OUTPUT tokens that run,
      because multi-decl `create` (shipped 2026-08-08, one day later)
      and `insert-header` (shipped 2026-09-02) didn't exist yet, so
      defn had to `create` two new files one def at a time instead of
      one shot. That specific gap may already be closed and has never
      been re-measured.
    - **Price correction (verified independently, not taken on faith):**
      this doc's Opus cache-read price ($1.50/M, the generic "10% of
      input" heuristic) was wrong. A least-squares fit against real
      billing data (`bench/session-cumulative/2026-08-07-session-usage.csv`,
      40 rows, max residual $0.0005) found `claude-opus-5`'s actual
      cache-read price is **$0.50/M** — confirmed by hand against 2 of
      those rows before touching anything. This changes items 1/2/7b's
      schema-tax percentages (see their own inline corrections): ~29%
      of the pooled prom-opus gap, not ~87%; the lean-desc saving is
      ~16%, not ~47%. Schema tax is real but was overstated 3x; result-
      byte volume and extra-message count are now the larger remaining
      share.
    - **A live bug found by mining real data, not by inspection**: the
      one-shot starter bundle (`#203`) unconditionally preferred the
      raw captured user prompt over the calling op's own default
      question, even when that prompt was pure conversational filler
      or a bench harness's fixed task-preamble sentence — both matched
      broadly against thousands of unrelated defs on common words
      ("go", "issue", "call"), burning the session's one bundle shot on
      an irrelevant dump. Measured on real `bench/prometheus-repo-opus`
      trajectories: fired on all 15/15 tasks keyed on the harness's
      literal first sentence, injecting a mean 8,588 B/task (~12.4% of
      all defn result bytes that corpus) for zero information — this
      session's own first bundle-eligible call reproduced it live,
      keyed on filler words from a venting message. Fixed: added
      `hasIdentifierShapedToken` (`internal/mcp/starter_bundle.go`) —
      the raw prompt is only preferred over the op's own default when
      it contains an actual identifier-shaped token (camelCase/
      snake_case), not just plain English. Test:
      `TestHasIdentifierShapedToken`; 234 tests statically affected by
      the change pass.
    - **Not yet done** (needs the user's go-ahead on spend, not
      proposed unilaterally): a small (~$45-75, Opus) re-run of the
      exact 2026-08-07 chi bench now that the write-side gaps are
      fixed, pre-registered against defn-natural ≤ crg-natural on total
      cost with ≥ equal correctness; then, only if that holds, a larger
      pre-registered head-to-head on the refactor-corpus specifically
      testing where crg's bare-name/no-build-gate rename should break
      on Go (duplicate method names) — a falsifiable correctness claim,
      not another cost-only comparison.
7f. **DONE 2026-09-09.** Live re-run of the exact 2026-08-07
    chi-ratelimit session-cumulative bench (Opus, same task, fresh
    isolated clones) to confirm whether the fixes since then actually
    moved real numbers, not just projections. Gap on this task: **+114%
    → +24%** (files $2.51→$2.44 fresh/cross-checked; defn-natural
    $5.36→mean $3.03 across n=3, $2.43–$3.45). All correct throughout.
    Also ran `code-review-graph` **naturally** (not forced, unlike the
    only prior data point) for the first time: its own graph tool was
    called ONCE in the whole 10-turn session — the model did the task
    almost entirely via native Bash/Read/Edit/Write, matching its own
    installed instructions ("the graph is a hint, read the source,
    source wins"). Its natural-mode cost ($2.76/40 calls) is close to
    files-mode's, not because of a better architecture but because it's
    barely used — the earlier "crg beats defn" data point was
    forced-substitution only, an artificial setup. n=1, not proven, but
    mechanistically clear (1 graph call out of 40 is not noise).
    **Root-caused the remaining +24% by reading the actual op sequence
    of all 3 defn runs against the 1 files run**: Bash counts are
    near-identical across every arm (task-mandated `go build`/`go
    test`, not a lever). Native `Read` in the defn arm is down to 0-2
    uses (real substitution now happening, a big change from July's
    "9 Read + 8 Bash even with the tool available"). The actual driver
    is **inconsistency in which defn op the model reaches for**, not a
    structural ceiling: one run (`defn-r2`) hit an EXACT match to
    files-mode's non-Bash call count (14 vs 14) using a clean, single
    consolidated path (`read-file` mostly); the other two used 2.5x/1.6x
    more calls by sampling several overlapping read-shaped ops for the
    same task (`read` AND `expand` AND `read-file` AND `outline` AND
    `overview` AND `context` AND `impact` AND `search` AND `help` in one
    run) instead of settling on one. Quantified the churn directly:
    r1/r2/r3 re-touch an already-covered target in 21-30% of their
    non-Bash calls, vs files-mode's own natural redundancy of ~8% (1
    legitimate re-read out of 12). **Found and fixed the actual gap**:
    `bodyServed` (the ledger that lets `read`/`outline`/`expand` skip
    re-serving a body already shown this session) was only ever WRITTEN
    by `read(full:true)` — `read-file` and `expand` both CONSULTED it
    but never recorded their OWN body serves, so two `expand` calls with
    overlapping-but-not-identical name lists (invisible to the existing
    same-args dedup) re-served the same bodies in full both times, and
    two `read-file` calls on the same file did too. Fixed: both ops now
    call `markBodyServed` for every body they actually render (not
    line-range-narrowed), and `read-file` now also consults
    `bodyServedEpochsAgo` per-def before rendering, matching the pattern
    `read`/`outline`/`expand` already had. New tests
    `TestHandleExpand_MarksBodyServedSoLaterOverlappingExpandSkipsIt`,
    `TestHandleReadFile_MarksBodyServedSoRepeatCallAndLaterReadSkipIt`
    (the latter also confirms the cross-op case: a plain `read` after a
    `read-file` now short-circuits too). Full `handleExpand`/
    `handleReadFile`-affected suites (241/241 tests each) pass.
    **Ruled out by checking real response bytes, not assumed**: the
    read auto-downgrade-to-outline threshold (1500B) is NOT a meaningful
    driver on this corpus — only 1 of 11 bare `read()` calls across all
    3 runs was actually downgraded, and it happened in the *most*
    efficient run with zero downgrade-then-refetch pattern anywhere. Not
    building a fix against unconfirmed evidence; may still matter on
    bigger real-world functions (refactor-corpus), unconfirmed.
    **On hard-gating, asked explicitly this session**: verdict is no,
    not now. The precondition from `#209`'s postmortem (cheapen/harden
    the consolidated tool before ever gating) is now mostly satisfied,
    but tonight's data shows gating is aimed at the wrong problem —
    native `Read` is already down to 0-2 uses per run; the churn is
    happening *inside* defn's own op menu (r1/r3 sampling 8 different
    read-shaped verbs), which a gate can't reduce and would only remove
    the escape valve for. Revisit only if a read-heavy (chi-explore-
    shaped, defn called 0x) workload shows native-peek escape becoming
    the dominant pattern after the op-menu fix below lands.
    **Not yet built** (next, per a fresh fable-agent plan given tonight's
    data): collapse the read-menu to one advertised ladder
    (`read-file`/`read`/`expand`/`impact`/`context`) in both the lean
    description and `defn init`'s CLAUDE.md template — `read-file`, the
    op that actually won parity in `defn-r2`, isn't even mentioned in
    the init template today. Then a small paid re-run (~$15-20, n≥5,
    defn-only) pre-registered on *variance* (non-Bash calls ≤14 in
    ≥4/5 runs), not just mean — consistency is the claim to test next,
    not another mean-cost number.
7g. **DONE 2026-09-09.** Full analysis of all 8 real defn chi-ratelimit
    trajectories (pre-fix r1-r3 + post-fix v1-v5) plus files/crg
    reference runs, requested directly: "analyze all the runs and make
    a detailed plan." Confirmed the two same-day fixes (7f) work exactly
    as designed — the "already read in this session" suppression marker
    fires 2-8 times in 4/5 post-fix runs (grepped directly from raw
    response text, not inferred), and mean `read-file` use roughly
    doubled (3.7→6.0/run). But the pre-registered variance bar (≤14
    non-Bash calls in ≥4/5 runs) still failed 0/5 — mean cost this batch
    ($3.53) was actually higher than the pre-fix batch ($3.03). Root
    cause of the non-improvement: NOT a regression from tonight's fixes
    — `impact` usage rose from ~1.0 to ~2.0 calls/run and `search` from
    ~1.3 to ~2.4/run, i.e. the model reinvested the saved capacity into
    doing MORE graph-based investigation, much of it genuinely
    task-driven (turn 6 explicitly asks a blast-radius question files-
    mode can only answer via manual grep+reasoning; defn answers it via
    `impact`, more rigorously but not for free).
    **New, separate, well-evidenced finding, same class as 7f's two
    fixes**: `batch-impact` already computes each call's real caller/
    test NAMES in memory (`allCallers`/`allTests`) but discarded them,
    returning only counts (`combined_callers: 136`). A real trajectory
    (`defn-v1`) called `batch-impact` for 6 names, got counts only, and
    immediately fell back to 2 individual `impact()` calls on names
    already in that batch — re-querying data already fetched, purely
    because the response shape couldn't answer "which functions,"
    only "how many." Fixed: response now includes `caller_names`/
    `test_names` (sorted, capped at `impactCallerCap`, zero new
    queries — the data was already collected). New test
    `TestHandleBatchImpact_IncludesCallerAndTestNames`; full
    `handleBatchImpact`-affected suite (237/237) passes.
    **Also checked, no fix needed**: `context`'s response never dumps
    full bodies (signature/doc + a byte/line count only; full bodies go
    to an internal buffer for the Sonnet-synthesis path, never the
    model-visible text) — the bodyServed gap `read-file`/`expand` had
    doesn't apply there.
    **Comparative note, not chased further**: `code-review-graph`'s ONE
    graph-tool call this session was used to explicitly split a real
    type-vs-method name ambiguity in prose FIRST, then targeted the one
    genuinely hard part (an interface-dispatched caller) — a
    reasoning-before-calling pattern our own ambiguity-note mechanism
    partly supports (it did fire: "2 definitions share the name") but
    didn't fully prevent the model from making several follow-up calls
    to sort out. Not a fix, a comparison point worth remembering.
8. **On hold, 2026-09-02 — user call**: "probably no 8 that seems way
   too expensive still. can't possibly be worth it." ≥3 repeats/task/arm
   on the 15 prom tasks + the 10 refactor tasks, Opus, EC2 (~$300) — not
   deleted, just not scheduled. If items 5/6/7 turn up a strong enough
   directional signal cheaply, revisit whether a full powered run is
   still worth it then, rather than spending $300 up front to confirm
   something the free fixes may have already mostly closed.
9. **Auto-append `opHelp[op]` to the first error per op** (item 2's cut
   scope) — polish on an already-shipped feature, no downstream
   dependency. Lowest priority, do whenever there's spare time.
9b. **DONE 2026-09-09.** Item 7f's other planned fix: `read-file` (the
    op that won parity in `defn-r2`) was never advertised anywhere a
    real user would see it — absent from both the lean tool
    description's "Orient before you read" guidance (present only as
    the 6th entry in a flat 18-op list, no disambiguation) and entirely
    absent from `defn init`'s generated CLAUDE.md "By intent" list.
    Added one short, cheap clause to the lean description pointing at
    `read-file` for whole-file coverage (kept minimal — this string
    rides on every call, can't afford to re-bloat it), and a full bullet
    + explicit read/read-file/expand disambiguation to the CLAUDE.md
    template, citing the real measured numbers (parity vs 1.6-2.5x more
    calls). New tests
    `TestLeanToolDescription_AdvertisesReadFileForWholeFileCoverage`,
    `TestDefnClaudeMDSection_AdvertisesReadFile`. Did NOT do fable's
    more aggressive "demote ~15 read-shaped ops to help-only" — that's
    a bigger, riskier redesign this session didn't validate; this is
    the conservative, cheap, evidence-backed piece of it. **Not yet
    measured**: whether this actually shifts real op-choice behavior —
    needs the same small paid re-run (n≥5, variance-focused) 7f already
    flagged as the next step, not done yet.

7h. **DONE 2026-09-09, same day.** Re-measured turn 6's answer quality
    (the real "which functions call X, which would be affected"
    question) across all 10 local trajectories instead of just cost —
    operationalizing 7g's "correctness-per-dollar, not just call-count
    parity" reframe. Checked whether each answer flagged the one
    genuinely subtle risk in the gold shape (`tree.go`'s `walk()` mixing
    a typed `Middlewares` return with an untyped `[]func(...)` variadic
    — the actual highest-risk site per a full manual read). Result:
    `files-3` and `crg-1` both caught it; 6 of 8 defn runs did too;
    `defn-r3` and `defn-v5` (2/8) missed it. Initially looked like a
    "defn's compact counts substitute for reading" pattern (worth
    flagging loudly), but checking the miss rate against BOTH reference
    arms found it's not systematic — most defn runs caught the same
    risk the reference arms did. Every arm reads `tree.go` once in turn
    1 for the (unrelated) routing-tree explanation task; whether that
    stays salient enough to cross-reference 5 turns later when
    answering a separate question looks like ordinary model-attention
    variance on a hard cross-file question, not a defn-specific gap.
    Logged as an observed-but-inconclusive correctness data point, not
    acted on — no clean lever here the way the other three fixes had
    one. Conclusion: defn is at rough correctness parity with files-mode
    on this hard question (both hit and both occasionally miss the same
    real risk) — the open gap is cost, not quality, and squeezing this
    one 10-turn task further risks overfitting; the next real test
    belongs on a corpus where the gap is bigger (refactor-corpus).

7i. **DONE 2026-09-10.** User asked "run it" on the refactor-corpus
    (defn-arm only, files-arm reused unchanged from the Sept 3 pilot —
    files-mode doesn't depend on defn's code). Real result, not a win:
    correctness held at rough parity (F1 0.723 vs files' 0.711,
    excluding the established goyacc confound) but pooled cost got
    **worse**, not better — 2.18x vs the original pilot's 1.62x. Same
    dynamic as 7f/7g's chi finding, at larger scale: 6 of 9 tasks got
    MORE expensive after tonight's fixes (up to +214% on one task),
    consistent with the model reinvesting cheaper-per-call defn ops
    into more thorough work rather than fewer total calls. One
    self-correction along the way: initially misdiagnosed
    `cli-token-type-constants-consolidate`'s F1=0.00 as a corpus/base-
    commit bug from an incomplete grep; the actual diff showed
    files-mode's fix was legitimate (consolidating a duplicated
    prefix-matching table, not just the named constants) and defn's
    model reasoned too narrowly about task scope — a real, if
    pre-existing (also failed pre-fix, differently), model gap.
    Forked a sub-agent to read all 9 non-confounded trajectories in
    full (not just cost outliers, per this project's own "read every
    trajectory" methodology) since the aggregate numbers alone didn't
    explain the regression. Found:
    - **Confirmed independently on a different corpus**: the read-file
      bodyServed fix (7f) works correctly here too —
      `cli-token-type-consolidate` calls `read-file` on the same file
      3x; the 2nd (no sync between) correctly suppresses all 18 defs,
      the 3rd (after an explicit sync) correctly re-shows them.
    - **NEW, real correctness bug, found and fixed**: `handleDelete`'s
      non-force path printed "Deleted X (id=N)" even when the write was
      actually rolled back by an emit-level failure — the exact
      misleading-message bug already fixed for `handleEdit` months ago,
      never applied here. A real trajectory
      (`etcd-refactor-event-helpers-move`) hit this directly: saw
      "Deleted IsCreateEvent (id=11732)" immediately followed by an
      emit parse error, read it as success-with-a-warning, burned 4
      extra calls sorting out that nothing had changed. Also found (and
      confirmed by writing the regression test) that the ONLY existing
      "rollback" test for delete actually uses `force:true` — the
      non-force build-failure-rollback path had never been tested at
      all. Fixed the message; added
      `TestHandleDelete_NonForceBuildFailureRollsBackWithHonestMessage`.
      **Separately discovered while building that test**: the code
      comment claiming "a real build can only catch [structural non-
      reference breaks like 'package main needs a func main']" is
      false — no build runs in this path at all (`commitOrRollbackOnEmit`
      is emit-only), confirmed by deleting the sole `func main` in a
      test fixture: it commits silently, zero warning, leaving a
      non-buildable package. Corrected the comment to stop claiming
      protection that doesn't exist; did NOT change the behavior
      (upgrading to a real build trades away the exact perf win the
      comment describes, for a rare case — a product tradeoff, not a
      bug fix, needs an explicit call not a unilateral change).
    - **Not yet built** (fork's other two findings, both cheap and
      well-evidenced, same shape as tonight's shipped fixes): (a)
      fragment-edit (`replace-hunk`) misses against large/repetitive
      files get zero diagnostic help — a bare "old_fragment not found"
      with no near-match hint, driving 5+ blind retries on
      `cli-refactor-getcomment-signature`'s 814-line test function
      (~28% of that task's calls). (b) the read auto-downgrade-then-
      refetch pattern, deprioritized on the chi corpus for lack of
      evidence, reproduces cleanly here: 4-for-4 wasted round trips on
      `prometheus-refactor-recode-signature`'s large method bodies —
      task shape matters, refactor-corpus's real methods are bigger
      than chi's small middleware functions.
8. **On hold, 2026-09-02 — user call**: "probably no 8 that seems way

## 6. Open questions for Opus to settle, not assume

- Does Claude Code send the full tool description on every call, or is
  MCP tool listing cached differently from the system prompt? (Affects
  R1's multiplier; measure, don't reason. Indirectly corroborated by
  chi-explore's +12.6% at zero defn calls, but not directly observed on
  the wire.)
- Multi-decl `create` shipped after the 07-11 forced-arm finding — has
  the write-heavy authoring shape ever been re-measured? (No record of
  it.)
