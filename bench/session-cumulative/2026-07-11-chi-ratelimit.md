# 2026-07-11 — chi + rate-limit-middleware: multi-turn cumulative session bench

**Result headline:** **defn cost 40% MORE than files-mode**, driven by
the agent using defn *additively* alongside Read/Bash rather than as a
replacement. Both arms correct. Real, honest, unexpected result.

## Motivation

Prior benches (`retrieval-llm/`, `mutations/`) measured stateless
per-invocation costs — every `claude -p` starts fresh, the ~25k
preamble floor dominates. Real users pay for **multi-turn Claude Code
sessions** where context accumulates. The hypothesis was that defn's
narrower per-call outputs compound across turns to a large session-
level savings — an acquaintance's uncited screenshot showed 85% saved
on a real session, and this bench was designed to see if we could
reproduce that shape.

## Setup

- **Repo:** [`go-chi/chi`](https://github.com/go-chi/chi) — small,
  familiar, real Go router.
- **Task:** add a token-bucket `RateLimit(rate, burst)` middleware
  with a ctx-cancel refactor. Multi-turn work: read routing internals
  → write middleware → tests → integrate example → refactor for ctx
  → verify. Well-scoped and testable.
- **Rigid 10-turn prompt script** — same user messages fed to both arms
  in the same order. See [`turns.txt`](./turns.txt).
- **Two arms**, both `claude -p` with `--strict-mcp-config`,
  `--session-id`, and `--resume`:
  - **files arm:** empty MCP config. Tools: Bash, Read, Write, Edit, etc.
  - **defn arm:** defn's `.mcp.json`. Tools: same as files + `mcp__defn__code`.
- **Model:** claude-opus-4-8 (default; Claude Code default routing).
- **Harness:** [`run-arm.sh`](./run-arm.sh). Analyzer: [`analyze.py`](./analyze.py).

## Results

```
=== FILES arm (10 turns) ===
turn  tool_use  input   cc_1h    cache_rd    output   cost
   1     20    47,535  183,821  1,845,504    289     $9.02
   2      1         7  181,628    195,632     18     $5.74
   3      1         5    5,036    305,895     11     $0.61
   4      1       133    2,574    235,319      3     $0.43
   5      1         4    2,420    239,355      3     $0.43
   6      1         7    4,321    405,299     13     $0.74
   7      2       140   11,242    584,850     19     $1.22
   8      2        10    5,084    524,145     12     $0.94
   9      1         4    1,862    267,633      3     $0.46
  10      1       133    1,684    270,898      5     $0.46
 TOT     31    47,978  399,672  4,874,530    376    $20.05

=== DEFN arm (10 turns) ===
turn  tool_use  input   cc_1h    cache_rd    output   cost
   1     42    36,927  185,564  5,575,619  1,048    $14.56
   2      2        13  245,996    525,325     21     $8.17
   3      1       134    8,126    391,590     10     $0.83
   4      1         5    3,722    409,306     13     $0.73
   5      1         4    1,914    311,285      5     $0.52
   6      1         5    3,683    419,090     10     $0.74
   7      1       392    6,057    427,173     10     $0.83
   8      1         4    2,574    329,080      5     $0.57
   9      1         4    2,162    333,974      6     $0.57
  10      1       133    2,008    337,607      6     $0.57
 TOT     52    37,621  461,806  9,060,049  1,134    $28.09
```

**Delta (defn vs files):**

| Metric | Files | Defn | Δ |
|---|---:|---:|---:|
| Tool uses | 31 | 52 | **+68%** |
| input_tokens | 47,978 | 37,621 | −22% |
| cache_creation_1h | 399,672 | 461,806 | +16% |
| cache_read_input_tokens | 4,874,530 | 9,060,049 | **+86%** |
| output_tokens | 376 | 1,134 | +202% |
| **USD cost (Opus 4.8 rates)** | **$20.05** | **$28.09** | **+40%** |

## Correctness

Both arms passed. Files arm produced 4 tests (all pass); defn arm
produced 3 tests (all pass) — different test naming, functionally
equivalent. `go build ./... && go test ./middleware/ -run RateLimit -v`
green in both clones.

## What actually happened

Turn 1 dominated both arms (task front-loading is standard Claude
Code behavior). Turn 1 profiles:

**Files turn 1** — 20 tool uses:
- 8 Bash, 6 Read, 3 Write, 3 Edit
- 20 tool_result blocks, sum 21,888 chars, mean 1,094 chars

**Defn turn 1** — 42 tool uses:
- 23 `mcp__defn__code`, 9 Read, 8 Bash, 1 ToolSearch, 1 Write
- `code` op mix: 9 create, 3 read, 3 sync, 2 search, 2 edit, 1 overview, 1 apply, 1 add-import, 1 delete
- 42 tool_result blocks, sum 32,783 chars, mean 781 chars

Defn's **per-result output IS smaller** — ~30% mean-char reduction. So
the "narrow reads" hypothesis holds on a per-call basis. But the agent
made **2.1x more tool calls**, because it used defn *additively* rather
than as a substitute:

- 9 Read + 8 Bash calls even with the code MCP available
- 3 `sync` calls (defn-specific overhead — reingest after file edits)
- 9 `create` calls to author the new middleware and tests

The extra round-trips more than eat the per-call compression. Cache-read
grows 86% because context accumulates faster with 2x round-trips.

## Why the acquaintance's 85% delta doesn't reproduce

If defn is **substituted** for Read/Bash — replacing them, not
supplementing — the per-call savings compound. That's the workload
class where 40-70% is plausible.

The reason this bench went the other way: **agent behavior**. Opus 4.8
defaults to Read for file inspection and Bash for greps, even with
defn's MCP available and CLAUDE.md instructing otherwise. To get the
graph win, the agent has to actually adopt the graph as its primary
lens. Ours doesn't.

## Full 5-arm result (2026-07-11)

| Arm | Tool uses | cache_read | Cost | Δ vs files | Correct |
|---|---:|---:|---:|---:|---|
| **files (baseline)** | **31** | 4.87M | **$20.05** | — | 4/4 |
| defn-forced (best defn config) | 61 | 7.97M | $25.74 | **+28%** | 4/4 |
| defn-batch-v2 (forced + tight batch preamble) | 49 | 7.12M | $27.93 | +39% | 4/4 |
| defn-natural (no restrict) | 52 | 9.06M | $28.09 | +40% | 3/3 |
| defn-batch (forced + naive batch preamble) | 63 | 11.17M | $40.67 | +103% | 4/4 |

**Ranking:** files wins by wide margin. Best defn config (forced) still
+28%. Batching preamble helped total tool-use count (49 vs 61) but
made cache_creation worse (554k vs 437k), net wash.

**Prompt tuning is exhausted; it can only move the delta within
+28–+40%.** To go below files-mode requires product changes to defn's
op vocabulary.

## Followup: hard-restricted `defn-forced` arm (2026-07-11)

Tried the tool-hijack approach — `--disallowedTools` blocking `Read`,
`Edit`, `Write`, and code-lookup bash (`cat`, `grep`, `find`, `head`,
`tail`, `ls`, `sed`, `awk`, `wc`, `rg`). Allowed bash restricted to
`go build*`, `go test*`, `gofmt*`, `go vet*`. Agent had to substitute
`code` ops for every read and edit.

**Result:**

| Arm | Tool uses | cache_read | Cost | Δ vs files |
|---|---:|---:|---:|---:|
| **files (baseline)** | **31** | 4.87M | **$20.05** | — |
| defn-forced | 61 | 7.97M | $25.74 | **+28%** |
| defn-natural | 52 | 9.06M | $28.09 | +40% |

Correctness: forced arm 4/4 tests pass (matches files arm exactly).

**Turn 1 tool profile (forced):**
- 36 `mcp__defn__code` (vs 23 natural, 0 files)
- 7 Bash (allowed build/test only, vs 8 natural, 8 files)
- 0 Read / 0 Write / 0 Edit
- 2 permission_denials from the disallow list

Op mix: 13 create, 9 read, 5 search, 3 edit, 2 impact, 1 apply, 1
delete, 1 sync, 1 slice.

**Substitution works but exposes granularity cost:**
- Files: 1 `Write` writes the whole 100-line ratelimit.go
- Defn-forced: 13 `code(op:"create")` calls — one per function/const/type
- Files: 1 `grep -rn Middlewares` returns all matches in one call
- Defn-forced: 1 `search` + follow-up `read` calls to actually see code

Per-call compression is real. Per-call *granularity* is worse for
multi-symbol authoring workloads. Net: hard substitution closes ~30%
of the natural-arm gap but doesn't reach parity with files-mode.

## What this reveals about defn as a product

Defn's op vocabulary was designed for **surgical single-op
mutations** — rename one function, edit one body, insert one
precondition. It's optimized for the case where the agent has a
narrowly-scoped edit target.

Multi-symbol authoring (writing a whole new file with 10+ defs) is
the OPPOSITE regime — one `Write` beats N `create`s. Files-mode
composes better there because `Write` naturally batches at the file
level.

To close the session-cumulative gap, defn probably needs:
- **Bulk-authoring op** — `code(op:"create-file", path, body)` that
  ingests a whole file worth of new defs in one call
- **Combined explore-and-read op** — `code(op:"read-with-refs")` that
  returns body + all callers in one round-trip, saving the
  search-then-read two-step
- **Op-level batching by default** — `apply` already exists but usage
  is rare; possibly promote it in the preamble or make the model use
  it automatically for related edits

## Batch preamble experiments (2026-07-11)

Two additional arms tested whether `apply` batching plus prescription
could close the gap:

- **defn-batch:** naive `--append-system-prompt` prescribing `apply`
  for related creates. Result: agent generated `create` ops WITHOUT
  `file:` — defs landed unpredictably; agent then made 10 `move`
  calls trying to reorganize + a cleanup loop. **+103% vs files.**
- **defn-batch-v2:** tightened preamble — apply MUST batch, every
  `create` MUST specify `file:`, never `move`, terser verification.
  Result: **+39%** — marginally better than natural, marginally worse
  than forced.

Key learning from batching: `apply` reduces total tool_use round-trips
(4 apply calls with 5+5+3+3 creates = 16 defs vs 16 sequential
creates), but exploration on the READ side still dominates. Batch-v2's
turn 1 had 17 exploration calls (`code(op:"read")` per def) vs
files-mode's 8 whole-file `Read` calls.

**Conclusion: prompt tuning is exhausted, gap is fundamental to op
vocabulary (see product plan below).**

## Product plan: enable defn to WIN

To go from +28% to negative Δ, defn needs whole-file-scoped ops
matching files-mode's per-file granularity on both sides:

1. **`code(op:"read-file", path:X)`** — return all defs' bodies in a
   file in one call. Mirror of files-mode `Read` on a .go file.
2. **`code(op:"read-package", path:X)`** — same but package-scoped;
   likely follow-up if #1 isn't enough.
3. **Enhance `overview`** or `apply` results so agent doesn't need
   verification round-trips.

Handoff memory: `project_session_bench_product_plan_2026_07_11`. Do
NOT create dual-path bugs — run `calque scan` before commit and
register any deliberate twins in `.calque/registry.md`.

## Followups

1. ~~Hard tool restriction to force substitution.~~ **DONE — see
   above.** Result: helps ~30%, still +28% vs files.
2. ~~Naive `apply` batching preamble.~~ **DONE — batch-v1** — worse
   than baseline due to file drift + move recovery.
3. ~~Tight `apply` batching with mandatory `file:`.~~ **DONE — batch-v2** —
   +39% vs files. Prompt tuning exhausted.
4. **NEXT: Implement `code(op:"read-file", path:X)` op** — the
   product change to test. Design in
   `project_session_bench_product_plan_2026_07_11` memory.
2. **Instrument CLAUDE.md more aggressively.** Current preamble says
   "prefer code over Read"; that's clearly not strong enough at Opus
   defaults. Try loud negative prescriptions.
3. **Look at Cline / Roo Code / Aider** — do they see the same
   additive-not-replacement pattern? If so, agent training bias is the
   real blocker to graph adoption, not defn's UX.
4. **Run this bench with Sonnet instead of Opus** — do smaller models
   substitute better?
5. **Re-check the acquaintance's actual measurement methodology.** The
   58k→8.5k comparison was almost certainly a per-read snapshot, not a
   cumulative session. Both can be true; but for marketing we need the
   session-level number, and this bench says the session-level number
   is worse-not-better under Opus's default behavior.

## What this means for marketing

- Do NOT ship "defn saves N% on real sessions" claims until we have
  session-level data that goes the right way.
- The trajectory analysis from
  [`../trajectories/2026-07-11-openhands-analysis.md`](../trajectories/2026-07-11-openhands-analysis.md)
  still holds — 15-25% call reduction on OpenHands with 5 tools. That's
  the honest ceiling for benching-based marketing right now.
- The Level 3 story (cache-optimized integration) is now dependent on
  first solving the substitution problem. Absent substitution, cache
  strategy is moot.

## Reproducing

```bash
# Setup
git clone https://github.com/go-chi/chi files-arm
git clone https://github.com/go-chi/chi defn-arm
(cd defn-arm && defn init . && defn ingest .)

# Run both arms (rigid script, --strict-mcp-config isolation)
./run-arm.sh files ./files-arm "--mcp-config=./empty-mcp.json"
./run-arm.sh defn  ./defn-arm  "--mcp-config=./defn-arm/.mcp.json"

# Analyze
./analyze.py ./out
```

## Data

- CSV per-turn: [`2026-07-11-session-usage.csv`](./2026-07-11-session-usage.csv)
- Raw stream-json turn files preserved in scratchpad (not committed —
  too large).
