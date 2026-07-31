# defn — Claude Code Instructions

## Product rule — parity is the floor

**Every defn feature must be at least as efficient as not-using-defn** on total
weighted session cost (tokens + wall-clock). If a mutation path pays more
tokens or more wall than the equivalent Edit/Write/`gopls`/native shell
sequence, defn is a net loss for that workload and the feature is broken.
Parity is the floor; aim for materially better. Measure vs the *real* native
baseline (AST splice + `go build .`, not `go build ./...`), not vs
defn-on-defn benches.

Perf work is judged by real-workload numbers, not synthetic sweep curves.
When in doubt, ask winze (or another external user) to run their shape and
report the number before claiming a win.

## Code Navigation and Editing

**The database is authoritative. Files are an I/O projection.** For Go code, use the `code` MCP tool — not Edit/Write/Read. Edit/Write are for non-Go files (YAML, JSON, Markdown, shell scripts).

```
code(op: "read", name: "handleEdit")           -- full source by name
code(op: "read", name: "server.go:272")        -- or by file:line
code(op: "outline", name: "handleEdit")        -- compact projection: sig+doc+refs+flow, no body (v0.24.2)
code(op: "slice", name: "handleEdit", slice: "error-branch") -- verbatim AST-role slice (v0.24.2)
code(op: "insert-precondition", name: "F", condition: "x < 0", ret: "return err") -- byte-exact PUTGET; name optional if the DB has one non-test function (v0.25.0)
code(op: "replace-slice", name: "F", slice: "return", index: 1, new: "return nil") -- byte-exact PUTGET; name optional if the DB has one non-test function (v0.25.0)
code(op: "replace-hunk", name: "F", old: "x := 1\n", new: "x := 42\n", index: 0) -- byte-exact PUTGET; content-addressed hunk inside a def body; index required only when `old` occurs >1x; empty `new` deletes the hunk. Send zero anchor context when the hunk is def-unique — name does the file-level disambiguation (v0.26.0)
code(op: "wrap-in-defer", name: "F", stmt_index: 1, defer_body: "cleanup()") -- byte-exact PUTGET; name optional if the DB has one non-test function (v0.25.0)
code(op: "rename-param", name: "F", old_param: "x", new_param: "n") -- ≡_gofmt PUTGET; name optional if the DB has one non-test function (v0.25.0)
code(op: "add-import", import_path: "errors", file: "pkg/f.go", alias: "") -- goimports-canonical grouping; file optional if the DB has one non-test .go file (v0.25.0)
code(op: "impact", name: "Render")             -- blast radius + test coverage
code(op: "edit", name: "Foo", new_body: "...") -- edit, auto-emit + build
code(op: "search", pattern: "%Auth%")          -- name pattern (% wildcard)
code(op: "search", pattern: "authentication")  -- body text search
code(op: "test", name: "Render")               -- run affected tests only
code(op: "sync")                               -- re-ingest after file edits
code(op: "sync", file: "pkg/foo.go")           -- fast single-file sync (~10ms)
code(op: "emit", out: "/tmp/out")              -- emit the tree (works while serve holds the DB)
code(op: "apply", operations: [                -- BATCH multiple ops in one call, atomic, one emit+build
  {op: "rename-param", name: "F", old_param: "data", new_param: "payload"},
  {op: "wrap-in-defer", name: "F", defer_body: "cleanup()"},
  {op: "insert-precondition", name: "F", condition: "err != nil", ret: "return err"}])
```

**Batch when you can.** `apply` accepts create/edit/delete/rename PLUS all 6 projection ops (insert-precondition, replace-slice, replace-hunk, wrap-in-defer, rename-param, add-import). Prefer one `apply` over N sequential calls: same file gets emitted+built once instead of N times, and any error rolls back the whole batch atomically. Especially valuable for related edits to one def.

All ops: read, outline, slice, insert-precondition, replace-slice, replace-hunk, wrap-in-defer, rename-param, add-import, search, impact, explain, untested, edit, create, delete, rename, move, test, apply, diff, history, find, sync, emit, query, status, diff-defs, traverse, literals, pragmas, file-defs, overview, patch.

### Why defn for Go, not Edit/Write

- **`code(op:"edit")`** updates a single row and emits one file — no full-package reparse. `Edit` on a `.go` file triggers full resync.
- **`code(op:"rename")`** renames across every file that references the definition, in one call. Doing this with `Edit` is 20+ tool calls and fragile.
- **`code(op:"move")`** moves a definition between packages, updates all import sites, and deletes the old file if empty. Not expressible in Edit/Write.
- **`code(op:"apply")`** batches mutations atomically — all-or-nothing. Edit/Write can't do this.
- **Ref graph stays consistent.** Every `code` edit keeps the refs table current. Edit/Write leave it stale until sync.

Rule of thumb: if you're changing a function body, signature, or name, use `code`. If you're editing `go.mod`, a YAML file, a Markdown doc, or emitting a template, use Edit/Write.

## Build & Test

```bash
go build ./cmd/defn
go test ./... -count=1
go run ./cmd/defn-test    # integration tests against real Go projects (clones from GitHub)
```

Go 1.26+ required.

## Perf measurement

Two CLI subcommands time a single mutation against a live `.defn` without
spinning up serve + MCP. Use them to compare defn's write path against native
(`AST splice + go build .`) on a real repo.

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

## Emit scoping (`Opts.TouchedFiles`)

MCP mutation callers pass an `emit.Opts.TouchedFiles []string` of the
project-relative source_files they actually touched. Emit uses it to:

- Skip module files not in the set (write only the touched files).
- Skip mod.Doc auto-attach (doc lives where it already lives on disk).
- Skip the post-emit loc-index rebuild (only `defn lint` consumes it).
- Scope `goimports` to those files (via `Opts.GoimportsFiles`).

Project files (go.mod, go.sum) are ALWAYS written regardless of scope —
scoped-emit into a fresh tempdir would otherwise leave the tree unbuildable.

Companion: `autoEmitAndBuildWithOpts` also passes `TouchedFiles` to
`buildTargetsForFiles` so `go build` targets just the touched packages
(`go build ./cmd/x ./internal/y` instead of `./...`), avoiding cgo-heavy
subtree drag.

## Self-Hosting Round-Trip

```bash
defn ingest . && defn emit /tmp/out && cd /tmp/out && go build ./cmd/defn/
```

## Architecture

```
cmd/defn/           CLI. init, ingest, emit, serve, impact, untested, lint, query.
cmd/defn-test/      Integration tests against chi, mux, gin, toml.
cmd/defn-bench/     Token/tool-call benchmark (files vs defn).
internal/store/     SQLite storage (modernc.org/sqlite, pure Go). Definitions, bodies, references, imports, modules.
internal/ingest/    Parses Go via go/ast + go/types. Stores definitions, imports, embeds, tests.
internal/resolve/   Reference graph via go/types (test packages included, receiver-qualified lookups).
internal/emit/      Writes .go files from database (single file per package, goimports required).
internal/lint/      Emit → golangci-lint → remap diagnostics to definitions.
internal/goload/    Shared Go package loading utilities.
internal/mcp/       MCP server — single `code` tool with op dispatch (DCL pattern).
testdata/           Test fixtures.
```

## Storage: SQLite

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

**Writing queries:** SQLite is more forgiving than MySQL on reserved words,
but keep the habit of backticking identifiers that look English-ish
(``SELECT `kind` FROM definitions``) — it stays portable if the backend
ever changes again. Full-text search uses SQLite FTS5 (`MATCH` syntax).

Versioning: use git + worktrees. The DB has no branch/commit ops; treat
`.defn/defn.db` as a build artifact you rebuild with `defn sync`. For
concurrent-branch experiments, run one `defn serve` per worktree
(deterministic per-project port; auto-shared within a worktree).

## Key Design Decisions

- **SQLite for storage.** Migrated from Dolt in v0.27 (Phase 4 big-bang, 2026-07). Reasons: pure-Go build (no CGO/icu4c), ~10x smaller binary, faster ingest, lower steady-state RAM. Git-style branch/merge on definitions turned out to be a non-goal — users prefer git worktrees + `defn sync`.
- **Single tool, op dispatch.** One `code` tool with an `op` field instead of 17 separate tools. Dynamic Context Loading pattern — 46% fewer input tokens.
- **Name or file:line.** Name-based ops accept definition names OR `file:line` paths — bridging the gap between location-first and name-first workflows.
- **Disambiguation by blast radius.** When names are ambiguous (20+ "Render" in gin), picks the definition with the most non-test callers.
- **Resolve includes test packages.** `Tests: true` in packages.Load + receiver-qualified lookups for correct method resolution.
- **`extractSignature` from body.** When definitions are updated via MCP, signature is recomputed from the new body text.
- **Definitions are the atomic unit.** Files are a build artifact from `defn emit`.

## Conventions

- **All comments are preserved** on round-trip — doc comments, inline comments, and comments between statements. The database is a lossless representation of the source.
- All Go dependencies MIT, BSD-2, or Apache 2.0 licensed.
- `internal/store/schema_sqlite.sql` is the schema source of truth (embedded via `//go:embed`).
- `internal/store/` must not import other internal packages.
- `.defn/` directory gitignored by `defn init`.

<!-- defn:begin -->
## Go code: use defn, not Read/Bash/Grep/Edit

This project is indexed in defn (`.defn/`). For any `.go` file, use the `code` MCP tool — **not** Read, Bash, Grep, or Edit. Those built-ins are reserved for non-Go files (yaml, json, md, sh, `go.mod`, Dockerfile).

**This is enforced, not just requested.** `hooks/defn-go-guard.sh` (wired via `.claude/settings.local.json` `PreToolUse`) blocks `Read`/`Write`/`Edit`/`MultiEdit` on `.go` paths and Bash dumps (`cat`/`head`/`tail`/`awk`/`sed`/etc.) of `.go` files. It's an adoption nudge for habitual tool choice, not a security sandbox — regex-based command classification can't be made complete against deliberate obfuscation, and that's an accepted, documented limitation of the hook, not a gap to keep chasing.

**Escape hatch (rare, e.g. a known defn write-path bug):** `touch ~/.claude-allow-go-edit` before the one blocked call you need — the hook consumes (deletes) the sentinel automatically on that single use, so it does not need to be manually removed and does not stay armed as a standing bypass.

**#209: enforcement alone made things worse, not better.** A chi-explore bench with the guard live cost +154% vs. native files — not because tool *choice* failed (100% of calls correctly went to `code()`), but because removing the cheap-native-peek escape valve turned an existing bundling bug into a 44-call binge. Root cause: `#203`'s starter bundle used a hardcoded `"project structure"` placeholder question for a bare `overview` call, returning content unrelated to what was actually asked — the model correctly ignored it and did the work itself, one small call at a time, several of them exact repeats. Three fixes now in place:
- **Intent capture**: `hooks/defn-capture-question.sh` (`UserPromptSubmit`) stashes the raw prompt into `.defn/.last-question`; `appendStarter` prefers it over the op-specific fallback, so the one starter-bundle shot per session is actually targeted at the real question.
- **Repeat-call dedup floor lowered** (512→200 bytes): a repeated small response (e.g. the same auto-downgrade note served twice) used to slip past dedup entirely, giving a blindly-retrying model zero signal to stop. The stub now also hints at `full:true` when the repeated content was itself a downgrade note.
- **Per-turn circuit breaker**: after `DEFN_CIRCUIT_BREAKER` (default 8) individual `read`/`outline`/`search`/`impact`/`overview`/`methods` calls without a `context`/`expand`/`apply` in between, further singleton calls are refused with a nudge to batch. Turn boundaries are detected via a token the same hook bumps once per prompt.

If you're tuning any of `#205`'s enforcement further, re-run the chi-explore bench (`bench/session-cumulative/`) after — enforcement that isn't measured against the actual workload is how this regression happened in the first place.

**Do not `ls` and `Read` files by hand.** Start any Go task with `code(op:"overview")` to see the project shape, then drill in with `search` / `outline` / `impact`.

**Reach for `outline` before `read`.** `outline` returns the signature, doc, refs, and control-flow of a def — 5-10× smaller than the full body. It's enough to answer almost every "what does X do / how does Y work / where does Z fit" question. Only escalate to `read` (full body) when you're about to edit the def, or when outline was genuinely insufficient. A follow-up `read` costs nothing you haven't already committed to.

### By intent

- **Discover ("how does X work in this codebase")**: `code(op:"context", question:"...")` — server-side bundle: top-N relevant defs outlined + refs graph + Sonnet synthesis, all in ONE round-trip. Prefer this over 10-40 sequential search/read/impact calls when starting exploration. This is the single biggest lever for turn-1 discovery cost.
- **Explore individual defs**: `code(op:"overview")`, `code(op:"outline", name:"F")`, `code(op:"search", pattern:"...")`, `code(op:"impact", name:"F")`. Use when you know which def matters. For open-ended "how does X work" reach for context first.
- **Ask a specific question about a known def**: `code(op:"explain", name:"F", question:"how does F handle X")` — defn hands the source to a Sonnet co-processor and returns a synthesized paragraph answer with provenance. Accepts `names:["A","B"]` for multi-def scope. Requires `ANTHROPIC_API_KEY` on the serve.
- **Saturate context in one call**: `code(op:"expand", name:"F", include:["outline","callers"])` — one round-trip instead of read → impact → read. Prefer `expand` over multiple sequential `code` calls whenever you'd otherwise chain them.
- **Read the full body**: `code(op:"read", name:"F")` — returns the body **plus** a compact "Related" footer (summary + top-3 callers + top-3 callees + semantic neighbors). One call gives you what would otherwise take 3-4 sequential `impact`/`outline` calls. Add `full:true` to force the body when defn returns an upstream provenance tag.
- **Edit a def**: `code(op:"edit", name:"F", new_body:"...")`, `code(op:"rename", name:"F", new_name:"G")` — updates every reference across the repo atomically. `Edit` on a `.go` file leaves defn's graph stale.
- **New def / whole file**: `code(op:"create", name:"F", file:"pkg/x.go", body:"...")`.
- **Batch changes**: `code(op:"apply", operations:[...])` — atomic, one emit+build for the whole batch.
- **Test**: `code(op:"test", name:"F")` — runs only tests covering that def, not the whole suite.

### Rules of thumb

- **outline first, read only if you're editing** (or if outline genuinely wasn't enough — but check first). This is the single biggest lever for session cost.
- Run `code(op:"impact", name:"F")` before modifying an existing def; skip it for brand-new ones.
- If you must edit a `.go` file with a built-in tool, follow up with `code(op:"sync", file:"path")` so the graph stays correct.
<!-- defn:end -->
