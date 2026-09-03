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
($1.50/M): 5k tokens × 40 msgs/task ≈ 200k tokens ≈ **$0.30/task** —
which is roughly the entire pooled gap ($0.27/task). Even if the true
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
     (range 14–97). Tax = 3,171 × 50.2 × $1.50/M (Opus cache-read) =
     **$0.239/task ≈ 87% of the pooled $0.274/task gap.**
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
   codebase — **default unchanged** (legacy) until the powered A/B
   (item 6) confirms the lean path end to end;
   `DEFN_STRIP=verbose-tool-desc` opts in early.
   Measured via an in-process `tools/list` probe (not estimated):
   **legacy total 14,037 B / lean total 6,121 B — 56.4% smaller wire
   payload; description alone 8,950 B → 1,144 B, 87.2% smaller.**
   Token-equivalent: ~3,171 → ~1,454 tokens per call (tiktoken
   cl100k_base proxy). Projected task-cost effect using the SAME real
   call counts as item 1: prom-opus (50.2 calls/task, Opus cache-read)
   saves ≈$0.129/task ≈ 47% of the pooled $0.274/task gap;
   etcd-multifile-v2 (27.3 calls/task, Sonnet cache-read) saves
   ≈$0.014/task ≈ 20% of that corpus's $0.072/task gap. **Not yet
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
7. **Refactor-shaped corpus** (§4) — build the 10 tasks, gold = the
   actual upstream commit's diff. Run both arms once as a pilot on EC2
   (Sonnet, ~$10) purely to check the corpus is sane before any powered
   run. Only run after item 6 lands.
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

Explicitly do not: add nudges, gate ops, build new discovery ops, rerun
prom-opus a third time as-is, or trust any n=1 win.

## 6. Open questions for Opus to settle, not assume

- Does Claude Code send the full tool description on every call, or is
  MCP tool listing cached differently from the system prompt? (Affects
  R1's multiplier; measure, don't reason. Indirectly corroborated by
  chi-explore's +12.6% at zero defn calls, but not directly observed on
  the wire.)
- Multi-decl `create` shipped after the 07-11 forced-arm finding — has
  the write-heavy authoring shape ever been re-measured? (No record of
  it.)
