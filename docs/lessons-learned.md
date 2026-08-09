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
