# defn — Claude Code Instructions

## Code Navigation and Editing

This project is self-hosted in defn. Use the `code` MCP tool for **Go code**:

```
code(op: "read", name: "handleEdit")           -- full source by name
code(op: "read", name: "server.go:272")        -- or by file:line
code(op: "impact", name: "Render")             -- blast radius + test coverage
code(op: "edit", name: "Foo", new_body: "...") -- edit, auto-emit + build
code(op: "search", pattern: "%Auth%")          -- name pattern (% wildcard)
code(op: "search", pattern: "authentication")  -- body text search
code(op: "test", name: "Render")               -- run affected tests only
code(op: "sync")                               -- re-ingest after file edits
```

All ops: read, search, impact, explain, untested, edit, create, delete, rename, move, test, apply, diff, history, find, sync, query.

**Both editing paths work.** `code(op:"edit")` updates the database, emits files, and rebuilds references automatically. File tools (Read, Edit) work too — call `code(op:"sync")` after editing Go files.

Prefer defn for Go code (fewer steps, auto-build verification). Use Read/Edit/Grep for non-Go files.

## Build & Test

```bash
go build ./cmd/defn
go test ./... -count=1
go run ./cmd/defn-test    # integration tests against real Go projects (clones from GitHub)
```

Go 1.25+ required.

## Self-Hosting Round-Trip

```bash
defn ingest . && defn emit /tmp/out && cd /tmp/out && go build ./cmd/defn/
```

## Architecture

```
cmd/defn/           CLI. init, ingest, emit, serve, impact, untested, lint, branch, checkout, merge, commit, diff, log, query.
cmd/defn-test/      Integration tests against chi, mux, gin, toml.
cmd/defn-bench/     Token/tool-call benchmark (files vs defn).
internal/store/     Dolt database layer. Definitions, bodies, references, imports, modules.
internal/ingest/    Parses Go via go/ast + go/types. Stores definitions, imports, embeds, tests.
internal/resolve/   Reference graph via go/types (test packages included, receiver-qualified lookups).
internal/emit/      Writes .go files from database (single file per package, goimports required).
internal/lint/      Emit → golangci-lint → remap diagnostics to definitions.
internal/goload/    Shared Go package loading utilities.
internal/mcp/       MCP server — single `code` tool with op dispatch (DCL pattern).
testdata/           Test fixtures.
```

## Storage: Dolt

Dolt = SQL database with native git semantics. Branch, merge, diff, commit on structured data.

Database stored in `.defn/` directory. Key tables:
- `definitions` — name, kind, exported, test, receiver, signature, hash
- `bodies` — source text (separate for fast metadata queries)
- `modules` — Go packages
- `` `references` `` — which definitions call/reference which (backtick-quoted: MySQL reserved word)
- `imports` — per-module import paths
- `project_files` — go.mod, go.sum, embedded files

Versioning via `CALL DOLT_COMMIT`, `DOLT_BRANCH`, `DOLT_MERGE`, `dolt_log`, `dolt_status`.

Dolt system tables (queryable via `code(op:"query")`):
- `dolt_log` — commit history
- `dolt_status` — uncommitted changes
- `dolt_branches` — branch list
- `dolt_diff_definitions` — definition changes between commits
- `dolt_diff_<table>` — per-table diffs for any table

## Key Design Decisions

- **Dolt for storage.** SQL database with native git semantics — branch, merge, diff, commit on definitions.
- **Single tool, op dispatch.** One `code` tool with an `op` field instead of 17 separate tools. Dynamic Context Loading pattern — 46% fewer input tokens.
- **Name or file:line.** Name-based ops accept definition names OR `file:line` paths — bridging the gap between location-first and name-first workflows.
- **Disambiguation by blast radius.** When names are ambiguous (20+ "Render" in gin), picks the definition with the most non-test callers.
- **Resolve includes test packages.** `Tests: true` in packages.Load + receiver-qualified lookups for correct method resolution.
- **`extractSignature` from body.** When definitions are updated via MCP, signature is recomputed from the new body text.
- **Definitions are the atomic unit.** Files are a build artifact from `defn emit`.

## Conventions

- **All comments are preserved** on round-trip — doc comments, inline comments, and comments between statements. The database is a lossless representation of the source.
- All Go dependencies MIT, BSD-2, or Apache 2.0 licensed.
- `internal/store/schema.sql` is the schema source of truth (embedded via `//go:embed`).
- `internal/store/` must not import other internal packages.
- `.defn/` directory gitignored by `defn init`.
