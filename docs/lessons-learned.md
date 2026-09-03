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

## Lessons from trajectory-driven bug hunting

Three days (2026-08-07 to 2026-08-09) digging real `head-to-head-go`
defn-arm trajectories (grpc-go, go-zero, cli/cli tasks) instead of
theorizing about cost drivers — the standing practice this project
follows because synthetic sweeps had previously missed exactly this
class of bug. Twenty-eight real bugs came out of it (in defn's MCP
tool, `internal/emit`, `internal/ingest`, `internal/resolve`, and the
bench's own scorer); full bug-by-bug detail lives in
`git log --oneline v0.26.13..v0.26.27` and each commit's message, not
repeated here. The durable, reusable lessons:

- **Two different packages' files sharing a basename could collide on
  emit's write path.** The most severe find: `emitModule` grouped a
  package's definitions by `filepath.Base(SourceFile)` instead of the
  full relative path. The original write-up (and the fix commit's own
  message) explained this as "`store.Module` is per `go.mod`, spanning
  every package" — that explanation was itself wrong and was corrected
  2026-08-09 (see below): `store.Module` is one row per Go package,
  confirmed by re-reading `ingestPackage`'s `db.EnsureModule(pkgPath,
  ...)` call (keyed on `pkg.PkgPath`, one per package) and by querying
  defn's own single-go.mod DB, which has 30 `Module` rows, not 1. The
  real corruption path was in output-path derivation, not Module
  scope: two different packages' `emitModule` calls could still land
  on the same on-disk directory (`pkgDir`) when the module-root-
  relative path collapsed, and bare-basename keys then silently
  collided at write time — e.g. `pkg/cmd/gist/create/create.go` and
  `pkg/cmd/repo/create/create.go` both reducing to `"create.go"`. One
  file got the other's content merged in; its sibling was silently
  never re-emitted. A real trajectory editing `repo/create`'s
  `createRun` corrupted `gist/create`'s `createRun` with the wrong
  body and imports, a file the agent never touched or referenced.
  Likely a significant, previously invisible contributor to defn's
  measured "over-touching" on every real multi-package repo tested
  (common basenames like `create.go`, `delete.go`, `config.go` repeat
  across packages constantly) — the corruption looked like the agent
  wrote extra files, when emit did. Fixed by keying on the full
  cleaned relative path instead of the bare basename, which is a
  strictly safer invariant regardless of Module granularity. Any time
  a per-file operation is keyed by something derived from a path, ask
  whether two different real paths could produce the same key before
  trusting it as a file identity — and verify the "why" against the
  actual code, not just the reasonable-sounding story that fits the
  symptom, which is exactly what went wrong here.
- **A synthetic disambiguating name must be stable across every
  context it can be assigned in, or it isn't an identity at all.**
  Second-most-severe find. Go allows unlimited `func init()` per
  module (common: one per file, each registering that file's own
  subcommands), so ingest assigns each a synthetic name (`init`,
  `init_1`, `init_2`...) to avoid natural-key collisions. The counter
  was keyed by module alone, accumulating across every file in
  whatever order one specific ingest run happened to process them —
  so a full-module ingest and a single-file `sync` (which always
  starts its counter fresh) assigned *different* names to the exact
  same physical function. Since name is part of the upsert natural
  key, each mode switch forked a new row instead of updating the
  existing one, and the fast single-file path does no stale-row
  pruning at all. A real trajectory that mixed file-level and
  module-level `sync` calls while working on one `init()` ended up
  with six byte-identical copies of it in the emitted file — likely
  duplicate command/flag registration at runtime, not just DB noise.
  Fixed by keying the counter on (module, file) instead of module
  alone, so the Nth occurrence in a given file is always named the
  same regardless of which mode or which other files process
  alongside it. General form: before trusting any synthesized
  identifier as stable, check whether it can be computed two
  different ways (two ingest modes, two call orders) that would
  reasonably disagree.
- **Sibling handlers drift.** When one operation has multiple entry
  points — `edit` vs `handleFragmentEdit` vs `apply`'s "edit" case;
  `create` vs `apply`'s "create" case — a correctness fix to one does
  *not* propagate to the others. Hit five separate times: multi-decl-
  body rejection, receiver-based disambiguation, rollback-vs-success
  reporting, multi-decl `create` support, and rename-by-ID vs
  rename-by-upsert (this last one a trap `handleRename` already
  documented in its own code comment — the sibling still didn't get
  it). When fixing one op's handler, grep for every other entry point
  implementing the same concept and check each one explicitly.
- **An accepted parameter isn't necessarily a wired-through one.**
  Schema acceptance and actual effect are separate claims. `search`'s
  `file:` param was silently ignored; `test:"TestX"`'s `module:`/
  `file:` were never threaded to the handler at all. `delete`'s
  `dry_run:true` was the same gap at its most dangerous: `handleCode`'s
  dispatch built `handleDelete`'s param struct without copying
  `args.DryRun`, and that struct had no `DryRun` field to copy it into
  even if it had — so a caller asking for a safe preview got a real,
  silent, unpreviewed delete instead. `apply`'s own `dry_run` already
  previewed deletes correctly, which is exactly why this was invisible
  in testing that only exercised `apply`. Both looked like real
  options to a caller and both silently did something other than what
  was asked — worse than an error, because nothing signals the
  mistake. When adding a flag to a shared param shape, grep every
  dispatch site that constructs that struct, not just the handler that
  reads it.
- **`store.Module` is per Go package, not per `go.mod`** — and a
  belief to the contrary shipped into this very file, a fix commit
  message, and a regression test's doc comment for a full day before
  being caught and corrected (2026-08-09). `ingestPackage` calls
  `db.EnsureModule(pkg.PkgPath, ...)` once per `*packages.Package`, and
  the SQL upsert is keyed on that path (`ON CONFLICT(path)`) — so a
  single-module repo (go-zero, grpc-go, cli/cli) gets one `Module` row
  *per package*, not one for the whole repo. Confirmed by querying
  defn's own DB (`SELECT COUNT(*) FROM modules`) — 30 rows for one
  `go.mod`. `findModule`/`findModuleByFile` scoping to "package X"
  therefore does resolve to that one package's Module row correctly;
  that part of the original worry was unfounded. The `test:"TestX"`
  defaulting to a whole-repo `go test ./...` when no scope is given is
  a real, separately-verified finding — an unrelated, unbuildable
  sibling package elsewhere in a large repo poisoned every named-test
  run regardless of whether the actual target package was fine — but
  its cause is that unscoped `test` shells out to `./...` outright, not
  a Module-granularity mixup. Fixed by resolving the pattern itself
  against the DB when no explicit scope is given, since it's usually
  the literal test name. Lesson generalized: a plausible-sounding
  causal story that matches the symptom is not the same as reading the
  code that actually produces it — this file is not exempt from that
  check just because it's the place bugs get written up.
- **A feature exercised only in this repo's own dev loop is a feature
  that doesn't ship.** `hooks/defn-capture-question.sh` (grounds the
  `#203` starter bundle in the real user question) was wired only into
  this repo's own `.claude/settings.local.json` — `defn init` never
  installed it for consuming projects, so every real user got a
  silently weaker fallback with no way to know the stronger path
  existed. Before assuming a capability benefits users, check whether
  `defn init`'s actual output makes it reachable.
- **The measurement tool needs the same scrutiny as the thing it
  measures.** Found three times now in the bench's own
  `score_correctness.py`: assuming a name/receiver convention defn's
  schema never uses, rejecting a parenthesized-pointer-receiver form
  the schema sometimes does produce, and — the op with the widest
  gap — never handling `rename`'s `old_name`/`new_name` fields at all
  (it only ever checked `args.get("name")`, which is always empty for
  a rename, so every pure-rename fix silently scored as touching
  nothing). All three understated defn's real correctness. A
  surprising bench delta is at least as likely to be a scorer bug as a
  product regression — read the actual trajectory before trusting an
  aggregate number.
- **Ephemeral storage must outlive whatever depends on it, not just
  the process that created it.** `head-to-head-go`'s scoring pass
  reads `.defn/` workdirs an earlier agent run left under `/tmp`; an
  EC2 stop/start clears `/tmp` and silently destroyed 9 of 10 tasks'
  scoring data, which then looked exactly like a correctness
  regression (same cost, F1 dropped to 0) until traced to the wiped
  directory. Moved to `~/.cache`. General form: if step B depends on
  state step A left behind, that state needs to survive B's
  environment, not just A's own lifetime.
- **A pilot's job is to generate fresh trajectories, not just confirm
  the fix that motivated running it.** After one fix landed and two
  digs into *already-collected* data came up clean, the instinct was
  to stop. Correction: parity with files-mode is the floor, not the
  goal, and "nothing new in data already examined" isn't the same as
  "nothing left to find" — the next step is a fresh run, not a re-read
  of old data.
- **Not every remaining gap is a tool bug.** One example task
  (`grpc-go-2630`) survived four straight fixes, each independently
  live-verified doing exactly what it claimed, because the task itself
  is a genuinely hard lexical-disambiguation case — a doc comment
  elsewhere in the same package authentically discusses the same
  keyword the issue uses, giving a keyword-driven search real textual
  grounds for the wrong answer. Recognize when a gap has shifted from
  "defn tooling defect" to "model reasoning limit" and stop chasing
  the same example past that point — see the standing warning against
  benchmark-validity rabbit holes.
- **Circuit breakers should self-heal, not just refuse.** A block that
  only refuses assumes the caller immediately restructures its whole
  strategy after one denial. Measured: one real trajectory hit 11
  consecutive refused calls (26% of its entire tool budget) before
  adapting. Fixed by having a block auto-batch every name seen since
  the last reset into one `expand` call instead of just saying no.
- **When there's exactly one sensible interpretation of an unusual-but-
  natural input, resolve it instead of erroring.** `rename` already
  propagates every call site automatically (zero ambiguity — there's
  only one correct new name at each site). `add-import` infers `file:`
  when exactly one candidate exists. The circuit breaker auto-batches
  probe-style calls into `expand` instead of just refusing. Extended
  the same philosophy to Go's own `pkg.Symbol`/`pkg/path.Symbol`
  qualified-name convention: a bare-name lookup failure now retries
  that shape before giving up, since an agent reaching for it is
  reusing Go's own disambiguation, not guessing. The line to hold: this
  is for *resolving what was asked*, not *guessing what to change* — a
  build-failure rollback deliberately does NOT auto-patch a broken
  caller the same way, because the correct fix (what to pass for a new
  param, what to do with a new return value) depends on intent defn
  has no way to know. A mechanical placeholder that only makes the
  build pass (discarding a new return value with `_`) could silently
  ship something that compiles but drops the point of the edit — worse
  than an honest rollback, because it would look done.
- **The same fact, framed for the wrong risk, might as well be
  missing.** `impact` already reported a def's caller count before a
  real trajectory edited a return-arity-changing signature alone and
  hit a rollback — but the existing warning framed that caller as a
  test-coverage risk ("no test coverage — a change here may break code
  no test will catch"), never as a "you're about to break this call
  site" one. Same underlying data, wrong lens, so it didn't prevent
  what it could have. Check whether an existing signal is actually
  aimed at the risk a new fix cares about before assuming it already
  covers the gap.
- **A 45-hour-stale local MCP server produces very confusing edit
  failures that look like product bugs.** Mid-session, edits that
  reported "Updated X" and even ran without error stopped showing up
  on disk at all. Root cause: this repo's own dogfooding `defn serve`
  process was still running a binary built long before the session
  started, silently disconnected from every source change made since
  — `defn status` surfaces this directly ("Version skew: running serve
  is 0.26.5 but $(which defn) is 0.26.14, restart to pick up"), but
  only if you think to check it. When a local tool's edits stop
  persisting for no visible reason, check for version/process skew
  before assuming the tool's logic is broken.

**On comparing against files-mode**: a single n=10 sample showed defn
leading files-mode on correctness, hit-rate, and cost simultaneously —
the first time that had happened on this benchmark. A second
independent repeat sample did not confirm it: combined across both
runs, defn's correctness came back down to roughly tied with
files-mode's single run, while the cost advantage held up across both.
Individual tasks swing a lot run to run (one task went from F1 1.00 to
0.00 between runs) — real model non-determinism, not a regression from
anything shipped here. Lesson: don't trust a single n=10 sample's
correctness ranking, and don't rerun one arm without rerunning the
other for symmetry. Comparative benchmarking was deprioritized after
this in favor of just finding and fixing real defn bugs directly,
which is what surfaced the emit corruption bug above — a better use of
the same digging effort than chasing a stable comparison number.

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

## v8-v10 bench findings (2026-08-20/21) and the starter-bundle correction (2026-08-24)

v8 head-to-head (15-task Prometheus corpus) found defn 14.4% more expensive
per task than files-mode and triggering the Go toolchain 82% more often.
Four root causes, all fixed by 2026-08-21 (check the referenced test before
re-diagnosing any of these from scratch):
- `test` returned unfiltered `-v` PASS/RUN noise (66% pure noise) below its
  size cap — `truncateTestOutput` now strips `=== RUN`/`PAUSE`/`CONT`
  unconditionally.
- The starter bundle echoed the full captured question into its own header
  — now truncates via `truncateForHeader`/`truncateList`.
- `read` rejected `line_range` with no `name` and forced a retry (hit in
  40% of tasks) — now redirects to `op:"read-file"`.
- A build timeout was misreported as an indistinguishable empty
  "BUILD FAILED:" — `emitAndBuildAgainst`/`runBuildIn` now check
  `ctx.Err() == context.DeadlineExceeded` and report "BUILD TIMED OUT"
  (test: `TestEmitAndBuildAgainst_TimeoutReportsTimedOutNotEmptyBuildFailed`).

v10 re-run (same corpus, after those 4 fixes) found the remaining cost is
NOT primarily an edit-batching problem: of 567 calls, edit-class ops are
only 10.4%, `test` is 9.9%, defn read-class ops are 51.5%, non-defn
Read/Grep/Glob another 25.6% — ~77% combined read-shaped. `context`/
`expand` (built to consolidate reads) were used 4 times (0.7%). Both
existing in-band nudges show zero observed follow-through: `writeBatchNudge`
fired 7 times, 0/7 followed by an `apply` call; the circuit breaker's
auto-batch rescue fired 19 times, 0/19 followed by the model reaching for
`context`/`expand` on its own afterward. Conclusion: further apply-batching
investment caps out near ~7% of total call-count reduction; reactive
in-band text nudges are a dead lever regardless of threshold tuning.

**The correction (2026-08-24)**: v10 called "auto-injecting a
`context(question:<task>)`-shaped bundle as the first tool response of a
turn" an "untried, highest-estimated-leverage direction" — this was wrong.
That mechanism already existed, built 2026-07-23 (`f90b264`, #203) and
refined four times since (#302 widened it past search/overview to
read/outline/impact/expand; #303 fixed dedup-hash poisoning; #312
turn-scoped it instead of session-lifetime-once; #328, 2026-08-22 — one
day after the v10 entry). The v10 run had it live the whole time; its own
0.7% figure measures the model *choosing* to call context/expand itself,
which the bundle's passive auto-injection was never going to move. Caught
only because a later session was asked to "build" this as new, went to
implement it, and found it already shipped. Lesson: bench-finding prose
is not exempt from going stale — re-verify a claim like this (git log,
grep the mechanism by name) before either building on it or trusting it
as current state.

**Still open** at the time of the correction: has the bundle's actual
downstream call-count/cost effect ever been measured post-#312/#328 with
a controlled A/B, rather than just unit-tested for its own mechanics?
`DEFN_STRIP=starter-bundle` (see `#209` above) exists for exactly this.
Separately, a one-shot-per-turn design has a real structural ceiling: it
can only help the first few calls of a turn, not sustained efficiency
across a long one — which the 0.7% aggregate may partly reflect
regardless of the bundle's presence.

**The A/B (2026-08-24), and a real caveat about where it ran.** Answered
the "still open" question above with `DEFN_STRIP=starter-bundle` against 2
prometheus tasks (a 3rd, prometheus-19114, failed identically in both
conditions on a local ingest error unrelated to the bundle — dropped, not
counted). This should have run on the defn-bench EC2 box per the standing
instruction below — it was run locally instead (caught only when asked
directly "did you run that locally or on ec2?"), pegged an 8-core laptop
to load average 8+, and is exactly the 5th occurrence of that same
mistake. `hooks/defn-bench-guard.sh` now blocks local `agent_driver.py`/
`launch_arm(_parallel).sh` invocations at the harness level (escape hatch:
`DEFN_BENCH_LOCAL_OK=1`) since memory alone had failed to stick 4 times
running. Take the numbers below as a real but small, not-EC2-clean signal.

Result: NOT a clean "bundle helps" story, and not even consistently
directional.
- `prometheus-18712`: no meaningful bundle effect. Both conditions
  produced the same correctly-scoped fix (added the one flag/field the
  issue asked for); OFF was slightly noisier only because of an
  unrelated transient DB-lock retry.
- `prometheus-16766`: bundle ON caused real scope creep. The actual gold
  patch is a 1-line fix (one `time.Duration` missing `.String()` before
  a slog call). WITHOUT the bundle: 4 calls, $0.10, fixed exactly that
  line. WITH the bundle: 32 calls, $0.49, and the model used the
  bundle's broader "related definitions" context to go hunting for every
  similar `time.Since(...)` call across the whole `tsdb` package,
  finding 7 more occurrences elsewhere and fixing all 8 — arguably a more
  thorough change in isolation, but 8x the touched surface the actual
  issue called for, and worse on any correctness metric scored against
  the real gold patch's file/line scope.

**Correction to the correction, same day**: the "bundle causes scope
creep" reading above does not survive a check against data that was
already sitting in the repo. `bench/prometheus-repo/arm_defn/
prometheus__prometheus-16766.json` is a PRIOR recorded run of this exact
same task, bundle ON (the shipped default, no DEFN_STRIP set), from
earlier bench work — 6 calls, $0.13, 58s, fixed exactly the 1 gold-patch
line, no scope creep at all. That's nearly identical to THIS session's
fresh "bundle OFF" run, not its "bundle ON" run. Same task, same
bundle-ON condition, two runs 8x apart in call count (6 vs 32) — that is
ordinary Sonnet run-to-run variance (lessons-learned.md already has a
prior instance of this: "one task went from F1 1.00 to 0.00 between
runs"), not a reproducible effect of the bundle's presence. A single
before/after pair can't distinguish "the bundle caused this" from
"this task happens to have high run-to-run variance regardless of the
bundle" — and this one turned out to be the latter.

Separately, the EXISTING correctness_scores.json for all 15 prometheus
tasks (all bundle-ON, already scored, zero new cost to check) argues
against scope creep being a common effect in aggregate: 14 of 15 tasks
score precision=1.00 (every touched file was in the gold patch, no
extras); only one (`prometheus-12024`, 7 touched vs 5 gold, precision
0.71) shows the touched-files-exceed-gold pattern at all. If broad
scope creep were a frequent side effect of the bundle, this existing
data would very likely already show it more than 1/15 times.

Net: no reliable evidence either way from this session's A/B. Don't act
on it as a product signal in either direction (don't remove the bundle,
don't "fix" scope creep that may not be a real bundle effect). If this
question is worth resolving for real, it needs enough REPEATS per
task per condition (not one run each) to separate a genuine bundle
effect from Sonnet's own run-to-run variance, which this correction
shows can be large enough on its own to fully explain what looked like
a dramatic effect. That's a bigger, EC2-run ask than what ran here.

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

## Grammar-driven synthetic Go generation: lit review (2026-09-01), not yet built

Prompted by an "isn't there an almanac of valid Go patterns we could turn
into a test suite / synthetic sandbox" question. `internal/fuzzgen/hazards.go`
already IS the informal version of this: 15 hand-authored declaration-shape
hazards (colliding basenames, scattered `init()`, method named `init`, type
alias, etc.) composed via `math/rand/v2`. A sibling agent (`documents-568515`
over mcp-dispatch) ran 4 research passes on whether to go further —
mechanically generating synthetic Go source across the language's full valid
grammar — and reported back. Findings, kept here since no code shipped from
this yet and the research would otherwise be lost:

**Closest existing tool, and its gap:** GoSmith (github.com/dvyukov/gosmith)
is a real csmith-for-Go, found 50+ gc/gccgo/llgo/spec bugs in 10 days. Its
own docs admit no interfaces, no interface satisfaction, no type assertions,
no methods, no constants — exactly defn's critical surface, since every one
of the 15 existing hazards is about identity/kind confusion among those
constructs. Dormant project; golang/go#7985 proposed folding it into the
toolchain, closed unadopted. Reusable trick regardless: each var carries
`used bool`, and `leaveBlock()` synthesizes `_ = varname` for anything unused
(context.go:579-585) — solves Go's "declared and not used" / "imported and
not used" compile errors deterministically, no retry loop needed. Worth
lifting into hazards.go even if we never go full-grammar, since some
hand-written hazards already work around this by hand.

**Grammar base**, if this is ever built: antlr/grammars-v4/golang — verified
current (last commit 2026-06-20, GPG-signed, was a typeParameters fix) and
generics-complete. Safe to build on instead of hand-transcribing the spec
EBNF.

**Coverage criterion:** Purdom's algorithm (1972) gives 1x-per-production
coverage — too weak for Go's nested expr/type grammar. Havrikov & Zeller's
k-path coverage (ASE 2019) is the real formalization: every depth-k path
through the derivation tree. Fuzzing Book's `GrammarCoverageFuzzer`
(fuzzingbook.org) is a runnable reference impl, ports onto an EBNF dict
directly.

**Bounded deterministic enumeration** (closest structural match to
"mechanical/deterministic," the framing the question was actually asking
for): Skeletal Program Enumeration (Zhang/Sun/Su, PLDI 2017,
arXiv:1610.03148) — fix a small skeleton + var set, exhaustively enumerate
binding/usage patterns within it rather than random-sampling. 217 real
GCC/Clang bugs from small exhaustive sets beating large random ones. Maps
onto defn's declaration shapes almost directly.

**Type-checker bug-class match:** Hephaestus (Chaliasos et al., PLDI 2022) —
Java/Kotlin/Groovy type-checker fuzzer via two mutations (type-erasure,
type-overwriting) tuned for generics/inference bugs. Relevant because
`internal/resolve` sits on `go/types` and generics is its least
battle-tested corner — zero existing systematic Go-generics-fuzzing work
was found anywhere.

**Validity mechanism — the load-bearing correction from the research
pass:** the first pass proposed grammar-generate-then-go/types-reject-and-
retry. Wrong model. Csmith threads a live points-to/range analysis through
construction and locally backtracks on unsafe choices — never a full-program
external check-and-discard. Pałka/Claessen/Russo/Hughes (ICFP 2011, DOI
10.1145/2034773.2034801) did the same for well-typed lambda terms fuzzing
GHC's optimizer — type-directed construction, not generate-then-check. If
this is ever built: thread a live symbol/type/usage env through generation
(Csmith/GoSmith/Pałka-style), synthesize blank-identifier uses for anything
unused, run `go/types` ONCE at the end to confirm — not in a retry loop.

**Corpus-mining alternative:** LangFuzz (Holler/Herzig/Zeller, USENIX Sec
2012) — tags real-code subtrees by grammar nonterminal, recombines across
programs. This is a systematized version of what hazards.go already does by
hand; the natural extension is mining defn's own gin/hugo/chi bench corpora,
tagging by AST node kind, recombining by grammar-slot.

**Pressure-test finding that reshapes scope, and the actual recommendation:**
defn doesn't parse — `go/parser` and `go/types` do, and both are already
exhaustively fuzzed upstream by the Go project and by GoSmith. Full-grammar
k-path coverage burns most of its budget proving defn correctly ingests
`a + b * (c - d)`, which was never in question — every existing hazard is a
declaration-shape bug, not an expression-grammar bug. Recommended scope, if
this is picked up: skeleton enumeration + k-path coverage applied only to
declaration shapes × file/package arrangement × the AST-role boundaries the
`slice` op already names (signature/doc/body/error-branch/return/loop) —
reserve full grammar breadth for literal/composite-literal edge cases that
actually touch the emit/printer path, since that's the one place defn's own
code (not go/parser's) does real work.

**False friends, ruled out:** go-fuzz, `go test -fuzz`,
AdaLogics/go-fuzz-headers, OSS-Fuzz's Go integration — all mutation/
byte-to-value fuzzers for testing a function's input handling; none has ever
generated Go source itself.

**Theoretical footnote, not actionable alone:** Feat (Duregård/Jansson/Wang,
Haskell Symposium 2012) — bijective size-indexed enumeration closed under
sums/products, matches `go/ast` being a sum-of-products type. No Go port
exists; the recipe transfers, the artifact doesn't.

**Where this actually lands, for now:** not building the full grammar-driven
generator. The nearer, higher-ROI move (matches the "meaningfully cheaper
than not-using-defn" bar much better) is extending `hazards.go` with more
skeleton-enumerated declaration shapes instead of hand-authoring one-offs —
see the pending "add a deliberate (non-random) `TestMutationSequence_Hazards`
case for a scattered-`init()` rename" task, which is the direct, concrete
first step toward systematizing this. Revisit full-grammar generation only
if declaration-shape enumeration stops finding new bugs.

**Update (2026-09-01), the pareto horizon for one enumerated family, found
by reading the code instead of guessing:** built the scattered-`init()` task
above as `internal/fuzzgen`'s `crossCallHazard`/`collisionKind` family (the
#372 caller-misattribution bug). First cut enumerated 2 dimensions:
`collisionKind` (the Go-legal mechanism letting two declarations share a
bare name — multiple `func init()`, or a function/method name collision
across files) crossed with `refSite` (which AST-role boundary within the
calling declaration's body makes the call — body/error-branch/loop/return,
matching `code(op:"slice")`'s regions) — 7 hazards from one template.

`refSite` turned out to be dead weight. Reading `resolve.go` closely:
`lookupFuncDefID` computes `fromID` **once per `ast.FuncDecl`**
(~line 515), then `collectRefs` walks the *entire* body via a
position-agnostic `ast.Inspect` and attributes **every** reference it
finds inside — call, field access, constructor, anything — to that one
`fromID` (~line 540). `astRename` (internal/mcp/server.go) rewrites via
the same blanket `ast.Inspect`. So for any bug in this class, *where*
inside a function body something sits is provably irrelevant to whether
the bug reproduces — only *which declaration the misattribution assigns
the reference to* (`collisionKind`) can possibly matter. Confirmed
empirically too: stripping the #372 fix and re-running the fail-without
check on all 7 variants failed exactly the 3 `init_multi` ones and passed
all 4 `method_vs_function` ones regardless of the fix (method lookup is
keyed by receiver+name, a different index entirely, so #372 was never
reachable through that collision kind at all) — `refSite` never changed
the verdict for either collision kind. Collapsed back to 2 hazards (one
per `collisionKind`, no `refSite` axis at all); re-ran fail-without/
pass-with on the collapsed set and got identical discriminating power at
2/7 the runtime.

**The general rule this sets, for the next family:** before enumerating an
axis, check whether the code path under test actually branches on it — a
single `ast.Inspect`/`ast.Walk` over a whole subtree, or any other
whole-tree fold, means position/nesting-within-the-tree is *not* a real
axis for whatever bug lives at the fold's granularity (per-`FuncDecl`
here). The horizon sits wherever the target code stops branching on the
dimension — past that point every extra enumerated variant is pure
runtime with zero marginal detection power. Grep the actual resolve/
ingest/emit function first (as done here); don't assume more combinations
always buys more coverage. Dimensions that likely DO clear this bar for
other families, because the underlying code demonstrably branches on
them: which *kind* of reference is made (`collectRefs`'s own switch has
distinct branches for `CompositeLit`/constructor, `SelectorExpr`/
`field_ref`, and plain calls, each hitting a different lookup —
`objToDef` direct hit vs `crossPkgTypeFallback` vs `lookupFieldDefID`);
and the query-layer ambiguity #352's family lives in
(`GetDefinitionByName`), which is a wholly different mechanism from
`collectRefs`/`lookupFuncDefID` and would need its own from-scratch
branch-reading pass, not an assumption that the same axes transfer.

## Open finding (2026-09-01): rename may under-count callers on a real grpc-go file — not yet reproduced

Mining a fresh `grpc__grpc-go-2629` head-to-head-go trajectory (run
earlier the same day by this same session, `bench/head-to-head-go/
bench/small-slice-6/arm_defn/grpc__grpc-go-2629.json`): `rename(old_name:
"withContextDialer", new_name:"WithContextDialer")` reported "Renamed
withContextDialer → WithContextDialer / Updated 1 callers", then a
`test`/build call several messages later hit `dialoptions.go:332:31:
undefined: withContextDialer` — a stale reference to the OLD name that
should have been rewritten. Confirmed via `git show <base_commit>:
dialoptions.go` (not guesswork) that the pristine file has the renamed
function referenced from at least two same-file call sites: a bare-value
assignment inside `init()` (`internal.WithContextDialer =
withContextDialer` — grpc-go's internal "friend function" hook var
idiom) and a direct call inside a sibling function (`WithDialer -> return
withContextDialer(...)`). "Updated 1 callers" against 2+ real call sites
is consistent with rename missing one.

Built a minimal, faithful synthetic repro of this exact shape (same
pre-existing name collision with an unrelated `internal.WithContextDialer`
var of a different kind, same two same-file caller forms) —
`TestHandleRename_UpdatesBareFunctionValueAssignedToCrossPackageVar`
(internal/mcp/server_test.go). It does **not** reproduce the
undercounting: defn correctly reports "Updated 2 callers" and rewrites
both sites. So the real gap needs something the minimal repro doesn't
capture — most likely a third real caller elsewhere in grpc-go's much
larger real module (a test file, or another production file), or some
interaction specific to the file's actual size/complexity (492 lines,
many sibling declarations) that a 12-line fixture can't trigger.

**Resolved (same day, later): does not reproduce against the real repo.**
Cloned grpc-go fresh at the exact base commit
(`32559e2175a5c793c47df0b214775affde5ac35e`) and ran two independent
clean-room checks directly against it:

1. Isolated single `rename` call — correct: `GetCallers` on the real
   pre-rename `withContextDialer` definition shows exactly the 2 real
   callers (`WithDialer`, `init`, both in `dialoptions.go`); after
   rename, 0 dangling lowercase references remain on disk, and both
   real call sites are correctly rewritten.
2. A faithful full replay of the trajectory's exact preceding call
   sequence (`search` ×2, `read` ×5, `outline`, then `rename`, then
   both of the model's own follow-up `edit` calls, then the real
   `test(name:"WithDialer")`) through the actual dispatch entry point
   (`handleCode`, not a shortcut) — also fully correct, and the real
   `go test` run passed all 225 affected grpc-go tests clean.

Neither replay reproduces the live trajectory's "init (pickfirst.go:108)"
caller misattribution or the eventual "undefined: withContextDialer"
build failure — pickfirst.go's own pristine source (confirmed via `git
show`) doesn't even contain the string `withContextDialer` at this base
commit, so that caller listing was wrong regardless of the rename bug
theory. **Conclusion: this is not a live, reproducible defn correctness
bug in the current codebase.** Best remaining explanation, not fully
confirmed: `agent_driver.py` caches a `.defn` snapshot per `(instance_id,
defn_binary_hash)` and restores it on a rerun; since the EC2 box
rebuilt multiple times the same day always as a `-dirty` build (three
different base commits, per the small-slice-3/5/6 version strings), a
`-dirty` hash collision across two genuinely different commits could
silently reuse a stale cached snapshot for a task — a harness-level
caching gap, not a rename defect. Not chased further this session; the
harness's own snapshot-invalidation logic (`_defn_cache_path` /
`_defn_binary_hash` in `agent_driver.py`) would be the next place to
look if this recurs.

## Adoption gap (item #1): lit review + plan (2026-09-01), not yet built

Standing finding restated: ~77% of a session's calls are read-shaped;
`context`/`expand` (built to consolidate them) see ~0.7% spontaneous
adoption; both in-band nudges tried show 0/7 and 0/19 follow-through —
a confirmed dead lever. Ran two parallel lit-review passes (academic +
industry prior art) before proposing a fix, per this project's own
standing rule against building on unverified priors.

**Load-bearing finding, changes the plan's shape:** the premise
"additive tool adoption (using defn alongside Read/Bash instead of
replacing it) is itself the failure" doesn't survive the prior-art
check. Cursor's own published numbers (cursor.com/blog/semsearch, Nov
2025) show grep + semantic search coexisting permanently by design,
framed as a *strength* (+12.5% accuracy, +2.6% retention on large
codebases), not a problem needing a fix. A hybrid coarse+fine split
can legitimately beat forcing one path — so the target metric should
be total weighted session cost (already the CLAUDE.md bar), not
adoption percentage per se.

**Directly corroborates this project's own #209 lesson, independently:**
a 2026 paper on Codex-vs-Claude tool-batching divergence (arXiv
2607.10569) found that restricting an agent to a consolidated tool
surface backfires specifically for Claude on edit-heavy tasks (+14.4%
cost) when the substitute tool can't yet absorb the redirected traffic
with real batching — friction without consolidation benefit. Their
fix, matching #209's own resolution order (intent-capture had to land
before the circuit breaker stopped backfiring): **fix the consolidated
tool's bundling quality and cost first, gate only afterward, and build
the escape valve in from day one rather than reactively after a
blowup.**

Anthropic's own "Writing effective tools for AI agents" (2025)
prescribes the same direction from the design side: don't nudge,
redesign the surface so the consolidated path is the obvious one
(their examples collapse multiple granular tools into one purpose-built
tool, e.g. `search_logs` replacing `read_logs` narrowed to relevant
lines only) — validates narrowing/cheapening the granular ops
themselves as a first move, independent of whether substitution ever
improves. PreToolUse-style hooks are confirmed the only deterministic
(not probabilistic) lever, validating going structural over another
prompt-level nudge.

Ruled out / not actionable here: RLVR/GRPO tool-choice reward-shaping
(assumes fine-tuning the underlying model, not available to a
downstream MCP-server project); RAG-MCP-style tool-count filtering
(defn has one dispatch tool with op-params, not a tool-count problem);
"over-tooled agent" thresholds (inapplicable to defn's shape). No
external vendor has published a comparable hard-gate backfire to
defn's own #209 — flagged as under-published, not as evidence hard
gating is broadly safe.

**Plan, in order (nothing shipped yet):**
1. **Measurement precondition.** Lock a repeated-trial A/B protocol
   (≥15 repeats/condition, both arms, EC2) before touching anything —
   the same rigor gap that let two earlier "defn wins" claims fail to
   replicate at n=10 applies here. The starter-bundle's actual
   downstream call-count/cost effect (flagged "still open" above) is
   this same precondition, never actually answered.
2. **Cheapen the granular path, harden the consolidated one — no
   gating yet.** Fix the known circuit-breaker auto-batch payload
   bloat (dumps full bodies for unrequested defs — the confirmed
   dominant driver of the etcd-21620 2x cost gap) to outline-only by
   default; cap/narrow `impact`'s unbounded lists (the 45KB single-call
   case). Reduces the "additive tax" regardless of whether substitution
   ever improves.
3. **Server-side auto-consolidation, not model-side nudging.** Extend
   the existing self-healing circuit breaker so detected repeat/blast-
   radius access patterns route through a `context`/`expand`-shaped
   bundle transparently, regardless of which verb the model typed —
   sidesteps the confirmed-dead in-band-nudge lever entirely since it
   doesn't require the model to change behavior. Only after step 2
   makes the bundle itself cheap, per the Codex/Claude ordering
   requirement.
4. **Measure via step 1's protocol before considering hard gating.**
   Only if total cost is still uncompetitive after 2+3, consider actual
   op-availability gating on `read`/`outline`/`impact` — this time with
   the escape valve built in from the start, sized against Claude's
   real tool-batching limits (per the arXiv finding), not copied
   verbatim from the `.go`-guard pattern.

**Correction (same day, before implementing step 2): don't build what's
already built or already reverted.** Checking the actual codebase before
writing code (which should have been part of drafting the plan itself,
not a step after "go" — see the user's own direct feedback on this)
found step 2 was already tried and explicitly reverted: `#312`
(`TestHandleCode_CircuitBreakerAutoBatchesInsteadOfRefusing`) shipped
the circuit breaker's auto-batch hijack, measured it at 0/19
follow-through (the exact number this plan's lit review cited
independently), caught it bundling 4 unrelated names' full outlines
into an already-narrowly-scoped response, and reverted to
instrumentation-only — citing "lean on better primitives... rather
than reactively overriding the model's own tool choice," the same
conclusion Anthropic's own tool-design guidance and the Codex/Claude
arXiv paper both point to. Step 1's auto-batch body-size-threshold
fix was also already shipped (`TestHandleExpand_AutoBatchBodyOverrideRespectsSizeThreshold`).

**What actually shipped instead (2026-09-01, commit pending):** the one
real gap found — `rankedSearchResult` already computes caller/test
counts per hit for ranking (`RefCountsByTarget`, no extra query) but
never surfaced them in the response, so a dead-code hit (0 callers)
was indistinguishable from a load-bearing one until a separate
`impact` call. Added `callers`/`tests` int fields to the ranked
search JSON, populated from data already in memory — zero new
queries, negligible payload cost (a few bytes/hit, nothing like the
45KB `impact` case). This is squarely in the "make the default
response smarter" lane the research pointed to, not a re-attempt of
the reverted hijack: it enriches the same hit already returned rather
than redirecting into unrelated names or overriding tool choice.
Regression test: `TestRankedSearchResult_ResultsIncludeCallerAndTestCounts`.
Not yet measured against real trajectories — the next real step, per
this plan's own step 1 lens, is watching whether this measurably cuts
the search→outline/impact follow-up chain in a fresh mined trajectory,
not assuming it does.

**First real-trajectory check (2026-09-01), n=1 -- encouraging, not
proven.** Reran `cli__cli-3461` (defn arm, sonnet, EC2) with the fixed
binary and diffed against the pre-fix trajectory already sitting in
`bench/small-slice-6/arm_defn/` for the same task, same model, same
harness. Confirmed the mechanism fires live: `search(pattern:"GetJobs")`
now returns `{"name":"GetJobs",...,"callers":2,"tests":0}` inline instead
of a bare name/file/score. Result: 33→21 tool calls (-36%), $0.574→$0.339
(-41%), 116.7s→82.3s (-29%), both `rc=0`. The after-run's final fix is
also arguably more correct (parses `run.JobsURL` directly, handling
GitHub Enterprise's `/api/v3` prefix; the before-run's fix never touched
the enterprise-URL path the task is actually about) — a bonus, not
something this single sample can credit to the search fix specifically.

Per this project's own repeated lesson ([[DefnBeatsFilesmodeFirstTimeV02621]],
[[PrometheusBatchDefnLoses20]] v3/v4) that a single-sample "win" has
twice failed to replicate here — **do not treat this as a proven
result**. It's one real, mechanistically-explained positive data point
justifying keeping the fix and watching for more, not a benchmark
claim. Next real evidence would come from mining more of the
already-completed small-slice-3/4/5 trajectories the same way (cheap —
they're already run, just need a fresh post-fix rerun each), not from
launching a new bench batch.

## Reranked backlog (2026-09-02) + item 3 closed without code, item 1 widened

Re-asked "what's blocking superior-to-files-mode" and reranked the open
backlog against two live threads: the recurring correctness-bug class
(two same-shape bugs fixed same day — `#357` project_files, today's
emitModule def-driven path) and the still-unproven n=1 search-fix
result above. Agreed order: (1) proactively audit for more instances of
"one bad row hard-fails the whole call" before a third winze report
finds one; (2) mine the remaining already-completed small-slice-3/4/5
+ small-slice-6 trajectories against the search fix; (3) fix the two
payload-bloat drivers named in the original plan; watch (5) write-side
round-trip granularity for angles while doing 1-3. Saved to winze-memory
as `RankedBacklog20260902`.

**Item 3 verified already closed — no code changed.** Before touching
anything (the exact check skipped once already this session, corrected
after direct user feedback), read the actual current source instead of
trusting the 2026-08-19 etcd-21620 memory:
- The circuit-breaker "auto-batch dumps full bodies" mechanism the plan
  named is not live code to fix — `#312` already reverted the whole
  hijack to instrumentation-only. `circuitBreakerCheck`'s return value
  reaches only `mcpDebugf`; the response is never touched. Confirmed by
  reading `handleCode`'s actual call site, not the old memory.
- `impact`'s unbounded test-list is also already fixed: `impactJSONTestsCap
  = 20` (internal/mcp/server.go), shipped under `#279` specifically
  citing the etcd-21620 45KB incident by name, separate from and much
  lower than `impactJSONCap = 200` (callers/interface-dispatch stay
  higher since seeing many production callers is often the actual point
  of a blast-radius check; tests are rarely read individually).
Both fixes predate this session by two weeks. The plan was stale, not
the code — exactly the failure mode the user's own prior feedback this
session ("check whether things were done before you made a plan to do
them") warned about, caught this time by verifying first.

**Item 1 audit found 5 real fixes, one more severe than the rest.** All
guarded by one new shared helper, `pathEscapesProjectRoot` (checks
`filepath.Clean` + `IsAbs`/contains `".."`):
1. `handleDeleteFile`'s two `os.Remove` sites — reachable via a bare
   client-supplied `file:` value with no DB row match at all (falls
   into the zero-defs branch), not just a corrupted DB row.
2. `handleDelete`'s name-scoped `os.Remove` — via a corrupted
   `Definition.SourceFile`.
3. **`patchInsertHeaderOnDisk` (op:"insert-header") — the most severe.**
   Unlike every other write op, this one never requires a DB-matched
   definition (`moduleID` silently stays 0 when nothing matches) — `file`
   passes straight through with no gate at all. A bare
   `op:"insert-header", file:"../../anything"` call, zero DB corruption
   needed, would read then overwrite whatever that path resolved to
   outside the project root. Found by auditing every `filepath.Join(s.projectDir,`
   site in server.go for a write/remove with no escape check, not
   reported by anyone.
4. `patchImportOnDisk` (op:"add-import") — same shape, gated behind a
   DB match (lower severity than #3).
5. Per winze's live field report (moved out from behind the symlink,
   rebuilt, still got the identical stale error naming a path that no
   longer existed on disk): emit's project_files loop now **deletes**
   an escaping-path row on detection (`Backend.DeleteProjectFile`, new
   interface method) instead of skip-and-warn-forever. Skip-with-warning
   stays as-is for definitions/file_sources — deleting a Definition row
   would drop real code, not just an inert path string.
7 new/extended regression tests, all passing; full tree builds and
vets clean. Not yet committed — bundling with the earlier
symlink-root/emitModule/search-count fixes into one release per user
direction, given nothing is tagged yet.

**Item 2 (n=9 trajectory mining) result: does NOT replicate the n=1
win — inconclusive, not a regression.** Reran the defn arm (fixed
binary, `232491283fa4`) for every already-completed small-slice-3/4/5/6
task that had a genuine pre-fix baseline (9 tasks; 2 more in
small-slice-3 had no prior arm_defn run at all, so no paired
comparison exists for those). Recovered the exact pre-fix cost/duration
per task from the raw `.claude-stream.jsonl` streams already saved
locally in scratchpad from earlier this session (each stream's final
`result` event carries `costUSD`/`duration_ms`) — the on-box
`arm_defn/*.json` summaries were deleted before rerunning (needed to
clear agent_driver.py's skip-if-exists guard) without backing them up
first, which could have lost the comparison data entirely; caught in
time only because the raw streams happened to already be mirrored
locally. Worth a harder habit going forward: back up before deleting
comparison baselines, don't rely on a fallback copy existing by luck.

| task | cost before→after | wall before→after |
|---|---|---|
| grpc-go-2996 | $0.088→$0.089 (+2%) | 55.5s→43.4s (−22%) |
| etcd-21620 | $0.312→$0.546 (+75%) | 101.4s→521.5s (+414%) |
| etcd-20929 | $0.380→$0.393 (+3%) | 90.3s→89.3s (−1%) |
| etcd-20006 | $0.164→$0.061 (−63%) | 236.6s→39.5s (−83%) |
| cli-1069 | $0.287→$0.472 (+64%) | 79.5s→127.3s (+60%) |
| grpc-go-3476 | $0.244→$0.163 (−33%) | 92.6s→61.1s (−34%) |
| go-zero-1907 | $0.160→$0.228 (+43%) | 74.5s→69.9s (−6%) |
| grpc-go-2629 | $0.583→$0.158 (−73%) | 149.0s→52.5s (−65%) |
| go-zero-2787 | $0.084→$0.063 (−25%) | 79.3s→24.5s (−69%) |
| **total** | **$2.302→$2.173 (−5.6%)** | **958.7s→1029.0s (+7.3%)** |

5 tasks improved (2 substantially), 3 got worse (1 substantially:
etcd-21620), 1 flat. Net aggregate is a wash on cost and slightly
worse on wall time — not a clean win like cli-3461's n=1 result.
Correctness held steady: all 9 paired tasks succeeded (`rc=0`) both
before and after.

Dug into the etcd-21620 outlier specifically rather than waving it
off: pre-fix made 24 `mcp__defn__code` calls ($0.312, 101s); post-fix
made 33 + 3 `Grep` = 36 calls ($0.546, 521s) — *more* tool calls
post-fix, the opposite direction the search fix should push. This
isn't the fix backfiring mechanically; it's the model taking a
longer, different problem-solving path this run (both `rc=0`, both
genuinely solved the task) — ordinary LLM sampling variance at n=1
per task, which is exactly the kind of noise this project's own
repeated lesson ([[DefnBeatsFilesmodeFirstTimeV02621]],
[[PrometheusBatchDefnLoses20]]) warns swamps small real effects.

**Conclusion: keep the fix (it's free — a few bytes/hit, no extra
queries, and still mechanistically sound), but stop treating the
earlier n=1 result as evidence of a real session-cost win.** This is
the correct, if less satisfying, scientific outcome for item 2: it
correctly tempers the initial optimistic single sample rather than
confirming it. Real evidence, if it exists, needs a properly powered
repeat-trial protocol (the same rigor gap flagged and then explicitly
dropped as "overkill" for this specific investigation) — not more
one-off small-slice reruns.

## Handoff (2026-09-02): gap analysis written, next-session TODO

Items 1, 2, 3 of the reranked backlog above are closed (1: five
escaping-path fixes shipped in `eb3420c`; 2: n=9 rerun is a wash, fix
kept; 3: already fixed two weeks prior). Item 5 (write-side round-trip
granularity) produced no new angle — multi-decl `create` already covers
the 07-11 "13 creates vs 1 Write" shape on paper, but has never been
re-measured.

A separate, cheap analysis pass re-asked "what stands between defn and
superior-to-files-mode" from the pooled numbers alone and wrote the
answer to **`docs/gap-analysis-2026-09-02.md`** — read that first next
session; it is the work order. One-paragraph version: defn is
correctness-neutral and ~24% more expensive pooled; the gap is three
deterministic costs, not model behaviour — (A) the `code` tool's real
wire schema (13,915 B / 3,171 tokens, measured via an in-process
`tools/list` probe, not the description string alone) cache-read on
every call, **measured at ~87% of the pooled prom-opus gap and ~36% of
the etcd-multifile-v2 gap** (2026-09-02, see item 1 below — done), (B)
per-call enrichment the model doesn't consume, (C) tail events from
failed writes. Also: no current corpus exercises defn's actual
asymmetries (cross-package rename/move, def-scoped test) — a
refactor-shaped corpus is proposed alongside, not instead of, the
bug-fix ones.

**TODO for next session, in order — do not reorder without a
measurement reason:**
1. [x] **Done 2026-09-02.** Quantified the schema tax exactly: real
       `tools/list` wire JSON (in-process probe against the actual
       `newMCPServer`, not the description string in isolation) is
       13,915 B / 3,171 tokens. × real mean assistant-message count
       from `bench/prometheus-repo-opus/arm_defn/*.json` (50.2/task)
       × Opus cache-read ($1.50/M) = $0.239/task ≈ **87%** of the
       pooled $0.274/task gap. Cross-checked against
       `bench/etcd-multifile-v2/arm_defn/*.json` (27.3 calls/task,
       Sonnet cache-read $0.30/M) = $0.026/task ≈ **36%** of that
       corpus's $0.072/task gap. Two independent corpora agree: this
       is the dominant lever. Full numbers in
       `docs/gap-analysis-2026-09-02.md` §5 item 1.
2. [x] **Done 2026-09-02, same session.** Shipped the lean tool
       surface in `internal/mcp/tool_help.go`: 1,144 B description
       (down from 8,950 B) + an `opHelp` map (47 real ops, including 7
       never documented before — `context`, `test-coverage`,
       `batch-impact`, `file-defs`, `methods`, `insert-header`,
       `resummarize` — and correctly dropping the 5 dead Dolt-era ops
       the old description still advertised) served via
       `op:"help", topic:"<op>"`. Gated behind
       `stripped("verbose-tool-desc")` (existing `DEFN_STRIP` plumbing)
       — default unchanged until item 6's A/B confirms it end to end.
       Measured (in-process probe, not estimated): total wire JSON
       14,037 B → 6,121 B (56.4% smaller); description 8,950 B → 1,144 B
       (87.2% smaller); ~3,171 → ~1,454 tokens/call. Projected saving
       using item 1's real call counts: ≈$0.129/task on prom-opus
       (≈47% of the pooled gap), ≈$0.014/task on etcd-multifile-v2
       (≈20% of that gap) — projection, not yet a fresh bench
       confirmation; that's item 6. All 142 tests statically affected
       by `handleCode` pass. Cut from scope: did NOT add
       "auto-append help to the first error per op" — `handleCode`'s
       defer-based pipeline is 800+ lines and this needs its own,
       separate change, not a blind patch alongside everything else
       here. Full numbers: `docs/gap-analysis-2026-09-02.md` §5 item 2.
       Also filed `bug-report-2026-09-02-create-duplicates-shared-import-alias.md`
       — `code(op:"create")` into a file whose body imports an alias
       already used elsewhere in the package emits it twice
       (build-breaking); hit reproducibly twice while building this.
**Same day, later: found + partially fixed a second, unrelated bug.**
A sibling agent ("winze") reported over MCP Dispatch that editing one
var in a grouped `var (...)` block in a different project came back
as a 291-line diff — reordered every var alphabetically, deleted
section comments. Reproduced against a minimal scratch package:
confirmed two independent root causes. (1) `declOrderInSource`
(`internal/emit/emit.go`) keyed every spec in a grouped block by the
same enclosing-`GenDecl` offset instead of each spec's own position,
so `writeFile`'s regenerate-path sort had nothing to disambiguate on
and fell through to the DB's alphabetical fetch order — **fixed**,
each spec now keys by its own `s.Pos()`; regression test
`TestDeclOrderInSourceOrdersVarBlockMembersBySpecPosition`. (2) ingest
never captures a var/const block's inter-spec section comments into
any def's `Body` at all — invisible while the byte-splice AST-merge
path succeeds (untouched bytes pass through raw), but the instant ANY
def in the file forces the regenerate fallback, the whole block's
comments are gone, not just the one that triggered it — **not yet
fixed**, needs an ingest-side change; filed as
`bug-report-2026-09-02-edit-var-block-reorder-drops-section-comments.md`
with a narrower alternative (isolate one bad def's regenerate fallback
instead of regenerating the whole file) worth considering too.

**Reranked 2026-09-02, after items 1-2 landed** — the remaining items
were originally numbered in write-order, not dependency/risk order.
Two things earned a move up: (a) items 5 and 6 both spend real money on
EC2, and the open snapshot-cache-collision theory (old item 7) means
any EC2 result today is unverifiable — fixing that BEFORE the next EC2
spend, not after, is the only way item 6's "no more single-run reruns"
rule is actually enforceable; (b) the create-duplicates-import bug (old
item 9) is cheap, confirmed, reproducible, and directly undermines the
"write atomicity, not yet trusted" line in the gap-analysis scorecard —
worth closing before it silently bites the NEXT `create` call (possibly
inside item 3's own histogram tooling or anything else that authors a
new file). Items 3/4 stay $0-cost analysis-from-existing-data and keep
their relative order (3 explicitly gates item 6; 4 doesn't gate
anything, it just feeds the same bug-hunt queue item 9 came out of).
Item 8 is pure polish on an already-shipped feature with no downstream
dependency — lowest priority, do whenever.

3. [x] **Done 2026-09-02, same day, later session.** Harness:
       `agent_driver.py`'s `.defn` snapshot cache key now includes
       `_defn_source_tree_hash()` (the defn repo's own HEAD + hash of
       any uncommitted diff) alongside the existing `_defn_binary_hash()`
       — a second, independent invalidation signal for the open theory
       from the grpc-go-2629 rename finding (a stale `which defn` PATH
       resolution could make two different defn source states hash to
       the same cached binary). Verified: hash is deterministic/repeatable
       for the same tree state and changes when the tree is dirtied.
       Strictly additive to the cache key — can only cause more misses,
       never fewer, so no risk to existing correct cache hits.
4. [x] **Done 2026-09-02, same day, later session.** Fixed
       `bug-report-2026-09-02-create-duplicates-shared-import-alias.md`
       — root cause was NOT `internal/emit`'s import-merging pass, it
       was `handleCreate`'s single-decl path never stripping a leading
       `import (...)` block from the body (unlike the multi-decl path's
       `sliceDecls`, which already discards them). Reused `sliceDecls`
       for the strip, and made the existing `#367` aliased-import-patch
       mechanism run unconditionally (idempotent) since this path's
       build check is parse-only and never had a reliable failure
       signal to gate on. Confirmed via live repro against internal/mcp's
       own `sdkmcp` alias (reproduced the exact `redeclared` error
       pre-fix, clean single occurrence post-fix). Regression test:
       `TestHandleCreate_SingleDeclLeadingImportBlockNotDuplicatedInEmittedFile`.
       Along the way, found and worked around a separate operational
       issue: a build-breaking file left by the live repro made
       `internal/mcp`'s defs briefly invisible to `overview`/`read-file`
       queries (package failed to `go/packages.Load`) until a scoped
       `code(op:"sync", file:"internal/mcp/server.go")` — not a data-loss
       bug (source files were untouched, confirmed via `git status`),
       but worth knowing: a broken package can make defn's OWN read
       ops blind to it until synced.
5. [ ] Bytes-by-op histogram across every `arm_defn/*.json` on disk;
       budget or opt-in any op whose median exceeds ~470 B (files-mode
       baseline). Audit `read` Related footer, provenance tags, starter
       bundle, ranked-search JSON, outline caller lists. Gates item 7
       (below) same as before.
6. [ ] Tail-event detector script: flag any defn error/no-op followed by
       ≥5 calls before the next successful write; rank by calls burned.
       This becomes the bug-hunt queue (the same kind of queue item 4
       came out of, just automated instead of found by hand).
7. [ ] Refactor-shaped corpus (10 tasks, gold = upstream commit diff);
       Sonnet pilot on EC2 (~$10) to validate before any powered run.
       Only run this after item 5 lands.
8. [ ] ONE powered A/B after 5+7 land (and item 3 is fixed): ≥3
       repeats/task/arm, prom-15 + refactor-10, Opus, EC2 (~$300). Not
       before — this is the one run that has to count.
9. [ ] Auto-append `opHelp[op]` to the FIRST error result for that op
       per session (item 2's cut scope) — needs a per-session
       "already shown" set threaded through `handleCode`'s existing
       defer, right before the `if err != nil || result == nil ||
       result.IsError { return }` early-return (that guard is the
       reason nothing past it — dedup, freshness note, starter bundle —
       currently fires on an error result; the new logic must sit
       BEFORE it, not after). Do this as its own change, not bundled
       into another one. No dependency on anything else here — lowest
       priority, do whenever there's spare time.

Do not: add nudges, gate ops, build new discovery ops, rerun prom-opus
a third time as-is, or report any n=1 win as a result.

Housekeeping: the four untracked leftovers this note used to list
(`bug-report-2026-08-28-*.md`, `bug-report-2026-08-29-*.md`,
`internal/mcp/repro_scratch_test.go`, `internal/mcp/zzz_scratch_sibling_test.go`)
are gone from the working tree as of 2026-09-02 — already resolved,
nothing to do here.
