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
class of bug. Nineteen real bugs came out of it (in defn's MCP tool,
`internal/resolve`, and the bench's own scorer); full bug-by-bug
detail lives in `git log --oneline v0.26.13..v0.26.22` and each
commit's message, not repeated here. The durable, reusable lessons:

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
  `file:` were never threaded to the handler at all. Both looked like
  real scoping options to a caller and both silently ran unscoped —
  worse than an error, because nothing signals the mistake.
- **`store.Module` is per `go.mod`, not per package.** A single-module
  repo (the common case: go-zero, grpc-go, cli/cli) has exactly one
  `Module` row covering every package. Any fix that resolves a
  "scope to package X" argument through `findModule`/`findModuleByFile`
  without checking this silently scopes to "the whole repo" instead.
  Match against `source_file` substrings directly when the intent is
  package-level, not repo-level, scoping.
- **A feature exercised only in this repo's own dev loop is a feature
  that doesn't ship.** `hooks/defn-capture-question.sh` (grounds the
  `#203` starter bundle in the real user question) was wired only into
  this repo's own `.claude/settings.local.json` — `defn init` never
  installed it for consuming projects, so every real user got a
  silently weaker fallback with no way to know the stronger path
  existed. Before assuming a capability benefits users, check whether
  `defn init`'s actual output makes it reachable.
- **The measurement tool needs the same scrutiny as the thing it
  measures.** Found twice in the bench's own `score_correctness.py`:
  once assuming a name/receiver convention defn's schema never uses,
  once rejecting a parenthesized-pointer-receiver form the schema
  sometimes does produce. Both understated defn's real correctness. A
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

**Net result on `head-to-head-go`** (real GitHub issues, n=10, live
2-arm, same model, `v0.26.13` → `v0.26.22`):

| | defn | files-mode |
|---|---:|---:|
| mean F1 | 0.790 | 0.680 |
| F1≥0.5 hit-rate | 9/10 | 8/10 |
| total cost | $2.97 | $4.24 |

First time defn has led files-mode on correctness, hit-rate, and cost
simultaneously on this benchmark, not just approached parity on one
axis while trailing on another. n=10 on one model is not yet enough to
call the ranking stable — growing the sample (more tasks, repeat runs)
is the natural next step before treating this as settled.

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
