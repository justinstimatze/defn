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
3. **Payload diet from existing data** (~2 h, $0). Bytes-by-op histogram
   across all `arm_defn` trajectories on disk (prom-opus, etcd-v2,
   head-to-head-go, small-slice). For every op whose median result
   exceeds files-mode's ~470 B: make the enrichment opt-in or budgeted.
   Specifically audit: `read`'s Related footer, provenance tags, starter
   bundle size, ranked `search` JSON verbosity, `outline` caller lists.
4. **Tail-event detector, mechanical** (~1 h). Script over trajectories:
   flag any defn error/no-op result followed by ≥5 calls before the next
   successful write. Rank by calls burned. That is the bug-hunt queue —
   replaces reading trajectories by hand.
5. **Refactor-shaped corpus** (§4) — build the 10 tasks, gold = the
   actual upstream commit's diff. Run both arms once as a pilot on EC2
   (Sonnet, ~$10) purely to check the corpus is sane before any powered
   run.
6. **One powered A/B, once, after 2+3 land**: ≥3 repeats/task/arm on the
   15 prom tasks + the 10 refactor tasks, Opus, EC2 (~$300). This is the
   only run that can move the "parity is the floor" verdict. No more
   single-run reruns — they have failed to replicate twice.

Explicitly do not: add nudges, gate ops, build new discovery ops, rerun
prom-opus a third time as-is, or trust any n=1 win.

## 6. Open questions for Opus to settle, not assume

- Does Claude Code send the full tool description on every call, or is
  MCP tool listing cached differently from the system prompt? (Affects
  R1's multiplier; measure, don't reason.)
- Multi-decl `create` shipped after the 07-11 forced-arm finding — has
  the write-heavy authoring shape ever been re-measured? (No record of
  it.)
- The rename "0 callers" / stale-snapshot harness theory
  (`_defn_cache_path` / `_defn_binary_hash` `-dirty` collision in
  `agent_driver.py`) is unresolved; any future EC2 result is suspect
  until the cache key includes the git tree hash, not just a binary hash
  that collides across `-dirty` builds.
