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

## 2026-08-07/08: six real bugs from real trajectories, one self-healing design fix

A two-day digging session working strictly from real `head-to-head-go`
defn-arm trajectories (grpc-go, go-zero tasks on defn-bench) instead of
theorizing about cost drivers — the standing practice this project
follows because synthetic sweeps had previously missed exactly this
class of bug. Every fix below traces to a specific, read line-by-line
tool-call sequence, not a guess. Commits: `773eeed`, `008e271` (both
released as `v0.26.14`), `9506b09`, `1004ba9`, `60fb503`, `b272b6e`.

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

Process note: mid-session the standing practice shifted from "ship a
release per individual fix" (7 releases in one sitting the day before)
to "batch fixes, verify with a real head-to-head-go pilot arm before
shipping something meant to matter" — the unit-test-only gate satisfied
the letter of "verify before push" but missed whether a fix changed real
agent behavior. The `apply` multi-decl fix above is the first one shipped
under the new rule, and the pilot numbers are why it was worth the extra
step: unit tests alone would never have surfaced the 90%/84% deltas.

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
