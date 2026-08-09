# defn — lessons learned & reference detail

Moved out of `CLAUDE.md` on 2026-08-05 to keep that file lean. `CLAUDE.md`
is reloaded every single turn of every session; this file is read on
demand — only when actually relevant (debugging the go-guard hook,
touching the emit/storage internals, doing perf work). See
`bench/cache-sim/` for the measurement behind this move: CLAUDE.md had
drifted to 5,037 tokens (vs. a recommended ~1,500), and every compaction
in a session re-pays that cost at the cache-write rate, not the cache-read
rate — so trimming it is a direct multiplier on the single biggest cost
driver found in this project's own 2026-08 caching investigation.

## 2026-08-07/08: sixteen real bugs from real trajectories, two design fixes

A two-day digging session working strictly from real `head-to-head-go`
defn-arm trajectories (grpc-go, go-zero, cli/cli tasks on defn-bench)
instead of theorizing about cost drivers — the standing practice this
project follows because synthetic sweeps had previously missed exactly
this class of bug. Every fix below traces to a specific, read
line-by-line tool-call sequence, not a guess. Commits: `773eeed`,
`008e271` (both released as `v0.26.14`), `9506b09`, `1004ba9`,
`60fb503`, `b272b6e` (`v0.26.15`), `38b5dc8`, `2ef68af`, `d19ba62`,
`dd51c81`, `77ba9a8` (`v0.26.16`+), `6b231ac`, `c7738b9` (`v0.26.18`),
`a23f2c5`, `51aeb99`, `8f65ee4` (`v0.26.19`-`v0.26.21`).

- **`edit` silently corrupted a definition instead of rejecting a
  multi-decl body** (`b272b6e`) — the most serious of the six. A real
  `new_body` concatenated 3 function declarations into one string.
  Unlike `create` (`countTopLevelDecls` + `handleCreateMultiDecl`), none
  of edit's three entry points — `handleEdit`, `handleFragmentEdit`,
  `handleApply`'s own "edit" case — ever checked decl count. The blob
  parsed fine (Go allows several func decls in one string) and passed
  the identity check (which only looks at the first decl), so it landed
  verbatim as the ONE target definition's `Body`. A later sync/re-ingest
  of the emitted file then split the extra two decls into duplicate
  definitions, producing a "redeclared in this block" build failure —
  and the edit itself had reported plain success, with nothing pointing
  back at the real cause. The real trajectory burned over a dozen
  confused follow-up calls recovering: a failed `apply`, four different
  `replace-hunk` attempts each failing on a *different*
  missing-required-arg error, and a pointless rename-to-`Xnew`-then-
  rename-back that fixed nothing. All three entry points now reject a
  multi-decl result up front, mirroring `create`'s own guard.
- **Cross-package interface dispatch never reached the ref graph**
  (`773eeed`). Two compounding bugs in `internal/resolve/resolve.go`'s
  pass 2: (1) interface-satisfaction pairing only checked a concrete
  type against interfaces in its OWN package, but Go's normal idiom is
  define-where-consumed/implement-elsewhere — grpc-go's `lbPicker.Pick`
  implements `balancer.Picker`, declared in a different package; (2)
  `go/types` reports a non-nil `Recv()` for interface method
  *declarations* too, identical in shape to a concrete method's
  receiver — without filtering `types.IsInterface(sig.Recv().Type())`,
  interface method decls leaked into `objToDef` via the same
  bare-`GetDefinitionByName` blast-radius-tiebreak fallback documented
  in the #219/#220 bug class, silently rebinding every call site
  dispatched through that interface to an unrelated same-named def
  elsewhere in the whole DB. Real-corpus confirmation: `defn impact
  '(*lbPicker).Pick'` went from 0 covering tests to 41 on grpc-go.
- **`apply`'s `create` case didn't support multi-decl bodies with
  `file:`** (`008e271`), unlike the standalone `create` op
  (`handleCreateMultiDecl`, motivated by 2026-07-11's finding). A real
  trajectory batched `edit` + a 2-decl `create` (file: set — the exact
  pattern this file and `CLAUDE.md` recommend) into one `apply` call and
  got rejected with "split into 2 create ops," burning a whole extra
  round-trip on retry. Pilot-verified on the same two tasks before/after
  the fix: writes down ~90% (9→1, 20→2), cost down ~84% ($2.28→$0.36
  combined), correctness *up* (F1 matched or beat the files-mode
  baseline instead of trailing it).
- **`read`/`outline` with `file:` and no `name:` gave unhelpful or
  crashing errors** (`9506b09`), seen independently in 4 trajectories —
  agents naturally reach for "show me this whole file" via `file:`
  alone. `read` said "name is required" (fine but unhelpful); `outline`
  had no upfront validation at all, fell through to
  `resolveEditTarget → findModuleByFile`, and either returned
  `definition "" not found` or — with a nil backend, the same
  construction `TestHandleCodeValidation` itself already relies on
  being safe for every other op — panicked. Both now point straight at
  `op:"overview", file:...`, which already serves this correctly.
- **`test` on a test function itself gave the dead-code message**
  (`1004ba9`), seen independently in 3 trajectories: an agent writes
  `TestFoo`, calls `op:"test", name:"TestFoo"` expecting it to run.
  `name` means "what covers this def" — nothing calls a test, so it
  always said "No tests cover TestFoo. Nothing to run.", indistinguishable
  from real dead code, forcing a second call with the differently-named
  `test:` param. Now checks the resolved def's own `Test` flag and
  points at `test:"TestFoo"` directly.
- **Circuit breaker (`#209`, below) self-heals instead of refusing**
  (`60fb503`). Quantified from the same trajectory sample: 18 of 175
  total tool calls (10%) were pure-waste breaker blocks — zero
  information returned, just the nag repeated. Worst case: 11
  CONSECUTIVE blocked calls, 26% of that one trajectory's entire tool
  budget, before the model switched to batching. The original design
  assumed one refusal would make the model immediately restructure its
  whole remaining strategy; that assumption doesn't hold reliably.
  `sessionCache` now tracks `pendingReadNames` — every nameable
  read-shaped call (read/outline/impact/methods/single-name expand)
  records its name whether or not it ends up blocked. A block on one of
  those ops now redirects through `expand` with every name accumulated
  since the last reset, instead of a bare refusal, and the redirect
  itself resets the counter. `search` (no single resolvable name) keeps
  the plain refusal. `circuitBreakerCheck`'s own signature and behavior
  are untouched — tracking is a separate, additive step at the
  `handleCode` call site — so all 8 pre-existing breaker unit tests
  needed no changes. Pilot-verified on the exact worst-case trajectory
  (the 11-consecutive-block one): re-run with the fix, that same task
  hit 0 plain blocks and 3 productive auto-batches instead.

- **`similar` had two structurally different algorithms silently
  swapped based on data shape** (`38b5dc8`, then collapsed further in
  `2ef68af`). Per the `Project-reverted-similar-cardinality-comod`
  postmortem, the reverted 2026-07-06 calque port was flagged for
  blocking candidate discovery on a signature `LIKE` prefilter. The
  live implementation (MinHash-of-body-shingles) moved past that for
  bodied defs, but kept the exact same flawed `LIKE` fallback for any
  def whose body was too short to shingle (interfaces, consts, vars) —
  and `ComputeMinHash` returns a fixed all-max sentinel for short
  input, so every such def would score 100% similar to every other one
  if the primary path were used naively. Fix collapsed to one
  unconditional formula: `ComputeMinHash(signature + "\n" + body)` for
  every definition, no branching, no threshold. Not trajectory-proven
  (no real run ever called `similar`) — proven directly via a
  sentinel-collision unit test instead.
- **`apply`'s name-based ops had no `receiver:` field at all**
  (`d19ba62`). Every batched `edit`/`delete`/`rename`/projection op
  resolved by bare name only, unlike every standalone handler (#219).
  A real trajectory needed to edit a same-named method disambiguated
  only by receiver, got a schema rejection passing `receiver:` into an
  apply op, then burned ~10 more retries on stale fragments and
  identity-check failures. Added `resolveApplyTarget`, a tx-aware
  mirror of `resolveEditTarget`'s exact precedence. Live re-verification
  on the same task: apply calls 10→3, writes 9→4, cost -31%, wall -30%.
- **`edit`/`handleFragmentEdit` claimed success on a rolled-back
  build** (`dd51c81`) — the identical anti-pattern `handleCreate`
  already got fixed for (see its own code comment), never applied to
  its two siblings. "Updated X (id=N)... BUILD FAILED" reads as
  partial success, not total rollback. Hit twice independently
  (cli-1069, grpc-2631).
- **`replace-hunk`'s `old`/`new` didn't accept `edit`'s
  `old_fragment`/`new_fragment` names** (`77ba9a8`) for the identical
  before/after-text concept — hit independently in cli-1069
  (standalone) and grpc-2631 (inside `apply`, mixing `edit` and
  `replace-hunk`). Now aliased at both entry points instead of erroring.
- **The bench harness itself was undercounting precision**
  (`6b231ac`, `bench/head-to-head-go/score_correctness.py`) —
  `resolve_defname_to_file`'s receiver-parsing branch assumed a
  `"Receiver.Method"` dotted-name convention defn's tool schema never
  actually uses (name and receiver are separate JSON fields). Every
  name-only write matched every same-named def in the whole repo, not
  just the one the tool call actually targeted. Confirmed via a real
  grpc-go-2631 trajectory: `regeneratePicker` matched 2 files
  (`balancer/grpclb/grpclb.go` + an unrelated `balancer/base/balancer.go`)
  when the agent had correctly disambiguated by receiver at the tool
  layer. This means some of this session's own "over-touching"
  precision numbers were partly measurement artifacts, not real
  agent misbehavior — a reminder that the measurement tool itself
  needs the same trajectory-driven scrutiny as the thing it measures.

- **`apply`'s `rename` op duplicated the definition instead of renaming
  it in place** (`c7738b9`, `v0.26.18`) — the same "sibling handler
  didn't get the fix" shape as the `receiver:` bug above, one layer
  deeper. `handleRename`'s own code comment already documents the trap:
  `UpsertDefinition` looks up rows by `(module,name,kind,receiver,test)`,
  so mutating `d.Name` in place before calling it inserts a fresh row
  under the new name instead of updating the old one — `handleRename`
  correctly uses the by-ID `RenameDefinition` instead, but `apply`'s
  own "rename" case never got the same treatment. A real trajectory
  batched `edit` + `rename` (pointer-receiver method) + `edit` of the
  just-renamed method + `create` in one `apply` call; the resulting DB
  had both the old and new names as separate rows, which
  `mergeDeclsIntoSource` then tried to write both — the old one
  already spliced out via `allowedRemovals`, so it matched nothing on
  disk, surfacing as a false "database and disk have diverged" warning.
  On the pre-#218 defn build that ran the original trajectory this
  landed as *silent* on-disk corruption (old method left behind,
  requiring a manual create+delete cleanup dance, ~12 extra tool
  calls); on current HEAD #218's contract correctly converts it into a
  clean whole-batch rollback instead — contained, but still a spurious
  failure on an otherwise valid batch. Root cause confirmed by directly
  instrumenting `mergeDeclsIntoSource` mid-test: both `allow` and
  `acquire` existed as separate rows simultaneously. Regression test
  reproduces the exact batch shape. Live pilot re-run of the exact
  originating task (chi rate-limit-middleware, `defn-forced`, same
  turns.txt) after the fix: 64→32 tool calls (-50%), $5.31→$3.00
  (-43%), all turns clean with zero retries — though that specific run
  happened to solve the task without ever needing a rename (the agent
  designed the ctx-aware method correctly from the start), so the
  pilot confirms overall trajectory health rather than replaying the
  exact rename+edit code path; the regression test is what proves the
  fix itself. Two follow-up digs (the `defn-natural` arm from the same
  original session, and turns 2-10 of this fresh pilot) turned up
  nothing new — worth recording as a negative result, not just
  positive ones: it means this investigation had genuinely run its
  course rather than stopping short.

Process note: mid-session the standing practice shifted from "ship a
release per individual fix" (7 releases in one sitting the day before)
to "batch fixes, verify with a real head-to-head-go pilot arm before
shipping something meant to matter" — the unit-test-only gate satisfied
the letter of "verify before push" but missed whether a fix changed real
agent behavior. The `apply` multi-decl fix above is the first one shipped
under the new rule, and the pilot numbers are why it was worth the extra
step: unit tests alone would never have surfaced the 90%/84% deltas.

### 2026-08-08 continued: pilot = fresh trajectories, not just fix confirmation

After the `apply`-rename fix (`c7738b9`) landed and two follow-up digs on
*existing* trajectory data came up clean, the standing instruction was
corrected: a pilot's job is to generate a fresh trajectory to dig into
for the *next* bug, not just confirm the fix that motivated running it
— and parity with files-mode is the floor, not the finish line ("we
don't stop until defn is much better than files"). A fresh EC2 pilot
turned up three more real bugs, all traced through one stubborn
example task (`grpc-go-2630`, "grpclb should drop only when at least
one connection is ready") that kept failing for a *different* reason
each time:

- **MCP startup always re-ran a full ingest, even when the DB was
  already fresh** (`a23f2c5`, `v0.26.19`) — `newMCPServer`'s startup
  goroutine unconditionally fired a full `packages.Load` + ingest +
  resolve, even right after a CLI `defn ingest .` (exactly what `defn
  init` and every bench harness setup step does) had just produced an
  identical DB. Until `s.ready` flipped, read-shaped ops raced the
  in-flight reingest — which *tears down and rebuilds* the defs table,
  not just leaves it stale — and returned actively wrong results
  tagged with only a soft "may be stale" text warning. Root-caused via
  a live grpc-go-2630 trajectory: the first `search` call landed
  mid-reingest and returned `Server`/`rpcStats`/`errDropped` instead of
  `regeneratePicker`, and the agent confidently edited the wrong
  function from that garbage. `alreadyFreshlyIngested` skips the
  reload when `last_ingest` already covers every `.go` file on disk.
  Live-verified: rerunning the same task on the fixed binary confirmed
  the stale-ingest warning is gone.

- **`search`'s `file:` param was accepted but silently ignored**
  (`51aeb99`, `v0.26.20`) — every search ran repo-wide regardless of
  the hint. Same trajectory: the agent called
  `search(pattern:"drop", file:"grpclb")` expecting scoping and got
  unfiltered results ranked by IDF instead. Fixed with a substring
  match on `source_file` (not `findModuleByFile`'s directory-suffix
  match used by read/outline/edit, which silently picks the wrong
  module for a bare hint like `"grpclb"` with no path separator).
  Regression-tested directly; did not by itself fix grpc-go-2630 (see
  below).

- **The `#203` starter bundle's "ground in the real question" path was
  never wired up for real users** (`8f65ee4`, `v0.26.21`) — the
  biggest find of the three. `lastUserQuestion()` reads
  `.defn/.last-question`, written by a Claude Code
  `UserPromptSubmit` hook (`hooks/defn-capture-question.sh`) — but that
  hook was *only* ever wired into this repo's own local dev settings
  (`.claude/settings.local.json`, gitignored). `defn init` never
  installed it for consuming projects. Every real `defn init` user was
  silently getting the weaker fallback (grounded on a bare search
  pattern or op name) with no way to know the stronger path existed.
  Confirmed on the same grpc-go-2630 trajectory: the starter bundle
  fired as designed but grounded on the literal term `"drop"` instead
  of the real problem statement, surfacing `Server.Drop`/`rpcStats.drop`
  — a real, legitimate, *unrelated* "drop" feature elsewhere in the
  same package. `writeClaudeHooks` embeds the script
  (`cmd/defn/assets/`, kept in sync with the repo's own copy by
  comment pointer since `go:embed` can't cross the `cmd/defn`
  directory boundary) and merges a `UserPromptSubmit` entry into
  `.claude/settings.json` — idempotent, provably preserves any
  existing hooks. Live-verified the hook now actually fires under
  headless `claude -p` (not just interactive sessions): `.last-question`
  held the real problem statement on the next rerun.

**Honest outcome on grpc-go-2630 itself**: four straight attempts
across all three fixes still landed on the same wrong function
(`(*lbPicker).Pick`) — not because the fixes didn't work (each was
independently live-verified doing exactly what it claimed), but
because the task is a genuinely hard lexical-disambiguation case:
`lbPicker`'s own doc comment authentically discusses "drop" handling
as part of normal per-request pick logic, so a keyword-driven approach
has real textual grounds for the wrong answer, not just noise. This is
a reasoning/disambiguation gap, not a defn tooling gap — flagged as
its own harder thread rather than chased further, per the standing
warning against benchmark-validity rabbit holes over fixing what's
actually fixable. (A 5th, unrelated rerun as part of the full n=9 batch
below did happen to land correctly — plausible model non-determinism,
not attributed to any specific fix.)

**A fourth bug, in the measurement tool, not the product**
(`3ac708b`, bench-only, no version bump): rescoring the full n=9
`head-to-head-go` set after the three fixes above showed
`grpc-go-2631`'s F1 apparently regress from 0.67 to 0.00 across two
otherwise-identical runs. Reading the actual trajectory showed a
*correct* edit matching the gold patch exactly
(`regeneratePicker`/`HandleSubConnStateChange`/`processServerList`) —
the agent had called `edit` with a combined
`name:"(*lbBalancer).regeneratePicker"` form instead of separate
`name`+`receiver` fields, which defn itself resolves fine, but
`score_correctness.py`'s injection-safety identifier gate rejected the
parens/asterisk outright and scored it as a total miss. Fixed with a
narrow, still-anchored regex for exactly this shape. A reminder,
again, that the scorer needs the same trajectory-driven scrutiny as
the thing it measures (see the receiver-resolve fix earlier in this
doc for the first instance of this exact lesson).

**Net result, full n=9 `head-to-head-go` set, same model, same tasks,
before vs after this whole arc** (`v0.26.18` → `v0.26.21`, honestly
rescored with the fixed scorer on both):

| | defn | files-mode |
|---|---:|---:|
| mean F1 | 0.783 | 0.711 |
| F1≥0.5 hit-rate | 7/9 | 8/9 |
| total cost | $3.55 | $3.79 |

For the first time on this benchmark, defn beats files-mode on *both*
correctness and cost, not just approaches parity on one while trailing
on the other. n=9 on one model is still not enough to call this
stable — the next pilot should keep pushing for a larger, repeated
sample before treating this ranking as settled.

## #209: enforcement alone made things worse, not better

A chi-explore bench with the go-guard hook live cost +154% vs. native
files — not because tool *choice* failed (100% of calls correctly went to
`code()`), but because removing the cheap-native-peek escape valve turned
an existing bundling bug into a 44-call binge. Root cause: `#203`'s
starter bundle used a hardcoded `"project structure"` placeholder
question for a bare `overview` call, returning content unrelated to what
was actually asked — the model correctly ignored it and did the work
itself, one small call at a time, several of them exact repeats. Three
fixes landed:

- **Intent capture**: `hooks/defn-capture-question.sh` (`UserPromptSubmit`)
  stashes the raw prompt into `.defn/.last-question`; `appendStarter`
  prefers it over the op-specific fallback, so the one starter-bundle
  shot per session is actually targeted at the real question.
- **Repeat-call dedup floor lowered** (512→200 bytes): a repeated small
  response (e.g. the same auto-downgrade note served twice) used to slip
  past dedup entirely, giving a blindly-retrying model zero signal to
  stop. The stub now also hints at `full:true` when the repeated content
  was itself a downgrade note.
- **Per-turn circuit breaker**: after `DEFN_CIRCUIT_BREAKER` (default 8)
  individual `read`/`outline`/`search`/`impact`/`overview`/`methods`
  calls without a `context`/`expand`/`apply` in between, further
  singleton calls are refused with a nudge to batch. Turn boundaries are
  detected via a token the same hook bumps once per prompt.
- **`DEFN_STRIP=field1,field2`** — single-feature A/B isolation for bench
  work. Disables one response-enrichment feature at a time without
  reverting code: `related-footer` (#202), `starter-bundle` (#203),
  `circuit-breaker` (#209), `dedup` (#77/#209). Read fresh per call, not
  cached — safe to change between bench arms without restarting the
  serve. Use this instead of guessing which shipped-this-session feature
  explains a bench delta.

If you're tuning any of `#205`'s enforcement further, re-run the
chi-explore bench (`bench/session-cumulative/`) after — enforcement
that isn't measured against the actual workload is how this regression
happened in the first place.

## Key Design Decisions

- **SQLite for storage.** Migrated from Dolt in v0.27 (Phase 4 big-bang,
  2026-07). Reasons: pure-Go build (no CGO/icu4c), ~10x smaller binary,
  faster ingest, lower steady-state RAM. Git-style branch/merge on
  definitions turned out to be a non-goal — users prefer git worktrees +
  `defn sync`.
- **Single tool, op dispatch.** One `code` tool with an `op` field
  instead of 17 separate tools. Dynamic Context Loading pattern — 46%
  fewer input tokens.
- **Name or file:line.** Name-based ops accept definition names OR
  `file:line` paths — bridging the gap between location-first and
  name-first workflows.
- **Disambiguation by blast radius.** When names are ambiguous (20+
  "Render" in gin), picks the definition with the most non-test callers.
- **Resolve includes test packages.** `Tests: true` in packages.Load +
  receiver-qualified lookups for correct method resolution.
- **`extractSignature` from body.** When definitions are updated via MCP,
  signature is recomputed from the new body text.
- **Definitions are the atomic unit.** Files are a build artifact from
  `defn emit`.

## Storage: SQLite, in detail

`modernc.org/sqlite` (pure Go, no CGO). Database stored in `.defn/defn.db`.
WAL mode; single writer, many readers. `defn gc` runs `VACUUM`.

Key tables:
- `definitions` — name, kind, exported, test, receiver, signature, hash
- `bodies` — source text (separate for fast metadata queries)
- `modules` — Go packages
- `refs` — which definitions call/reference which (edges in the call graph)
- `imports` — per-module import paths
- `project_files` — go.mod, go.sum, embedded files
- `definitions_fts` — FTS5 virtual table over body text, used by `op:search`

**Writing queries:** SQLite is more forgiving than MySQL on reserved
words, but keep the habit of backticking identifiers that look
English-ish (``SELECT `kind` FROM definitions``) — it stays portable if
the backend ever changes again. Full-text search uses SQLite FTS5
(`MATCH` syntax).

Versioning: use git + worktrees. The DB has no branch/commit ops; treat
`.defn/defn.db` as a build artifact you rebuild with `defn sync`. For
concurrent-branch experiments, run one `defn serve` per worktree
(deterministic per-project port; auto-shared within a worktree).

## Emit scoping (`Opts.TouchedFiles`), in detail

MCP mutation callers pass an `emit.Opts.TouchedFiles []string` of the
project-relative source_files they actually touched. Emit uses it to:

- Skip module files not in the set (write only the touched files).
- Skip mod.Doc auto-attach (doc lives where it already lives on disk).
- Skip the post-emit loc-index rebuild (only `defn lint` consumes it).
- Scope `goimports` to those files (via `Opts.GoimportsFiles`).

Project files (go.mod, go.sum) are ALWAYS written regardless of scope —
scoped-emit into a fresh tempdir would otherwise leave the tree
unbuildable.

Companion: `autoEmitAndBuildWithOpts` also passes `TouchedFiles` to
`buildTargetsForFiles` so `go build` targets just the touched packages
(`go build ./cmd/x ./internal/y` instead of `./...`), avoiding cgo-heavy
subtree drag.

## Perf measurement, in detail

Two CLI subcommands time a single mutation against a live `.defn` without
spinning up serve + MCP. Use them to compare defn's write path against
native (`AST splice + go build .`) on a real repo.

```bash
defn measure-rename [--in-place] <old-name> <new-name>
defn measure-edit   [--in-place] <name> <body-file>
```

- Without `--in-place`: fresh tempdir per run. Reports the CEILING cost
  (full emit + full build every time). Multi-package trees may fail to
  build in the tempdir — expected.
- With `--in-place`: pre-populates scratch with one full emit BEFORE the
  timer, then runs the timed mutation against a warm tree. This exercises
  the real file-scoped emit + package-scoped build path (#117/#118) a
  real MCP client sees. Pre-populate cost logs to stderr as
  `[prepopulate] full emit for rename: Xs` and is UNTIMED — factor it
  out when analyzing the wall.

Delta between ceiling and `--in-place` = the win from file-scoped emit +
package-scoped build.

Set `DEFN_MEASURE_TIMING=1` for a per-phase breakdown inside emit
(project-files / module-writes / goimports / refresh-file-sources /
rebuild-loc-index).
