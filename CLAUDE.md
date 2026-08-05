# defn — Claude Code Instructions

## Product rule — parity is the floor

**Every defn feature must be at least as efficient as not-using-defn** on total
weighted session cost (tokens + wall-clock). Measure vs the *real* native
baseline (AST splice + `go build .`, not `go build ./...`), not vs
defn-on-defn benches. Perf work is judged by real-workload numbers, not
synthetic sweep curves — ask winze (or another external user) to run
their shape before claiming a win.

## Code Navigation and Editing

**The database is authoritative. Files are an I/O projection.** For Go code, use the `code` MCP tool — not Edit/Write/Read. Edit/Write are for non-Go files (YAML, JSON, Markdown, shell scripts).

```
code(op: "read", name: "handleEdit")           -- full source by name
code(op: "read", name: "server.go:272")        -- or by file:line
code(op: "outline", name: "handleEdit")        -- compact projection: sig+doc+refs+flow, no body
code(op: "slice", name: "handleEdit", slice: "error-branch") -- verbatim AST-role slice
code(op: "insert-precondition", name: "F", condition: "x < 0", ret: "return err") -- byte-exact PUTGET; name optional if the DB has one non-test function
code(op: "replace-slice", name: "F", slice: "return", index: 1, new: "return nil") -- byte-exact PUTGET
code(op: "replace-hunk", name: "F", old: "x := 1\n", new: "x := 42\n", index: 0) -- byte-exact PUTGET; content-addressed hunk; index required only when `old` occurs >1x
code(op: "wrap-in-defer", name: "F", stmt_index: 1, defer_body: "cleanup()") -- byte-exact PUTGET
code(op: "rename-param", name: "F", old_param: "x", new_param: "n") -- ≡_gofmt PUTGET
code(op: "add-import", import_path: "errors", file: "pkg/f.go", alias: "") -- goimports-canonical grouping
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

**Batch when you can.** `apply` accepts create/edit/delete/rename PLUS all 6 projection ops. Prefer one `apply` over N sequential calls: same file gets emitted+built once instead of N times, and any error rolls back the whole batch atomically.

All ops: read, outline, slice, insert-precondition, replace-slice, replace-hunk, wrap-in-defer, rename-param, add-import, search, impact, explain, untested, edit, create, delete, rename, move, test, apply, diff, history, find, sync, emit, query, status, diff-defs, traverse, literals, pragmas, file-defs, overview, patch.

**Why defn, not Edit/Write:** `edit` updates one row and emits one file (no full-package reparse); `rename` updates every reference in one call (Edit would be 20+ fragile calls); `move` moves a def between packages and updates all import sites; `apply` batches atomically; the refs table stays consistent on every `code` edit, but goes stale after Edit/Write until a sync. Rule of thumb: changing a function body/signature/name → `code`. Editing `go.mod`, YAML, Markdown, or a template → Edit/Write.

## Build & Test

```bash
go build ./cmd/defn
go test ./... -count=1
go run ./cmd/defn-test    # integration tests against real Go projects (clones from GitHub)
```

Go 1.26+ required. Perf-measurement CLI (`measure-rename`/`measure-edit`), emit-scoping internals, storage schema detail, and design-decision history all live in `docs/lessons-learned.md` — read on demand, not every session.

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

## Conventions

- **All comments are preserved** on round-trip — doc comments, inline comments, and comments between statements. The database is a lossless representation of the source.
- All Go dependencies MIT, BSD-2, or Apache 2.0 licensed.
- `internal/store/schema_sqlite.sql` is the schema source of truth (embedded via `//go:embed`).
- `internal/store/` must not import other internal packages.
- `.defn/` directory gitignored by `defn init`.

<!-- defn:begin -->
## Go code: use defn, not Read/Bash/Grep/Edit

This project is indexed in defn (`.defn/`). For any `.go` file, use the `code` MCP tool — **not** Read, Bash, Grep, or Edit. Those built-ins are reserved for non-Go files (yaml, json, md, sh, `go.mod`, Dockerfile).

**This is enforced, not just requested.** `hooks/defn-go-guard.sh` blocks `Read`/`Write`/`Edit`/`MultiEdit` on `.go` paths and Bash dumps (`cat`/`head`/`tail`/etc.) of `.go` files. Escape hatch (rare, e.g. a known defn write-path bug): `touch ~/.claude-allow-go-edit` before the one blocked call you need — self-consuming, no manual removal needed.

**Do not `ls` and `Read` files by hand.** Start any Go task with `code(op:"overview")` to see the project shape, then drill in with `search` / `outline` / `impact`.

**Reach for `outline` before `read`.** `outline` returns the signature, doc, refs, and control-flow of a def — 5-10× smaller than the full body. It's enough to answer almost every "what does X do / how does Y work / where does Z fit" question. Only escalate to `read` (full body) when you're about to edit the def, or when outline was genuinely insufficient. A follow-up `read` costs nothing you haven't already committed to.

### By intent

- **Discover ("how does X work in this codebase")**: `code(op:"context", question:"...")` — server-side bundle: top-N relevant defs outlined + refs graph + Sonnet synthesis, all in ONE round-trip. Prefer this over 10-40 sequential search/read/impact calls when starting exploration. This is the single biggest lever for turn-1 discovery cost.
- **Explore individual defs**: `code(op:"overview")`, `code(op:"outline", name:"F")`, `code(op:"search", pattern:"...")`, `code(op:"impact", name:"F")`. Use when you know which def matters. For open-ended "how does X work" reach for context first.
- **Ask a specific question about a known def**: `code(op:"explain", name:"F", question:"how does F handle X")` — defn hands the source to a Sonnet co-processor and returns a synthesized paragraph answer with provenance. Accepts `names:["A","B"]` for multi-def scope. Requires `ANTHROPIC_API_KEY` on the serve.
- **Saturate context in one call**: `code(op:"expand", name:"F", include:["outline","callers"])` — one round-trip instead of read → impact → read. Prefer `expand` over multiple sequential `code` calls whenever you'd otherwise chain them. **Have several targets in mind at once? Use `names:["A","B","C"]` instead of one `expand` per name** — round-trip *count* within a turn is the dominant session cost driver, not per-call size. Unresolvable names are skipped with a note rather than failing the whole call.
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
