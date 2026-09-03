# Bug: `code(op:"create")` duplicates an import already used elsewhere in the package

**Found:** 2026-09-02, while building the lean-tool-surface change
(`internal/mcp/tool_help.go`, see `docs/gap-analysis-2026-09-02.md`
work-order item 2).

**Fixed:** 2026-09-02, same day, later session (work-order item 4).

## Repro

1. Package already has another file importing an aliased package, e.g.
   `internal/mcp/tool_help.go` and `internal/mcp/server.go` both import
   `sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"`.
2. `code(op:"create", file:"internal/mcp/some_new_test.go", body:"...")`
   where `body`'s own `import (...)` block ALSO imports
   `sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"` (needed because
   the new file uses `sdkmcp.X` symbols directly, e.g. a
   `sdkmcp.NewInMemoryTransports()`-based test harness).
3. The emitted file on disk ends up with the import listed **twice**
   inside the same `import (...)` block (confirmed via `grep -n
   sdkmcp <file>` showing two occurrences, blank line between the two
   groups):
   ```go
   import (
       ...
       "github.com/justinstimatze/defn/internal/store"
       sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

       sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
   )
   ```
4. `go build`/`go test` fails: `sdkmcp redeclared in this block`.
5. **The stored definition body in the DB is correct (single import)** —
   confirmed via `code(op:"replace-hunk", ...)` on the duplicate-import
   text returning `"old not found, but replacement already present in
   body"`. So this is an **emit-time** bug (the goimports-canonical
   grouping pass, per the `applyOp` doc comment in `server.go`), not a
   create/ingest-time one.
6. Reproduced twice independently (two separate fresh `create` calls,
   two different filenames, same package, same alias) — not a fluke.
7. `code(op:"sync")` (bare, and `sync file:"<path>"`) does **not** fix
   an already-bad emitted file; only re-emitting the file (e.g. via a
   subsequent edit to the same def) does.

## Impact

Any `create` (or likely `apply` with a `create` op) into an existing
package where the new file's own body imports a package already
imported elsewhere in that package, under the same alias, produces a
file that fails to build. Silent until the next build/test — the
`create` call itself reports success.

## Root cause (found)

Not `internal/emit`'s import-merging pass — the bug is upstream of
emit, in `handleCreate`'s **single-decl** path
(`internal/mcp/server.go`). The `#357` fix already strips a leading
`package X` clause from a single-decl `create` body via
`stripLeadingPackageDecl`, but never stripped a leading `import (...)`
block the same way the **multi-decl** path's `sliceDecls` already does
(`sliceDecls`'s own doc comment: "Import blocks are silently
skipped — goimports re-adds them at emit time from usage"). So a
single-decl body that (like the multi-decl case) naturally opens with
its own import block landed **verbatim** in the stored def's `Body`.
At emit time this literal text gets written a SECOND time in addition
to the canonical per-module import block (`relevantImportsForFile`),
producing `X redeclared in this block` whenever the alias is already
registered elsewhere in the module — silent at `create` time because
`commitOrRollbackOnEmit` only runs a parse-level check for this path
(a duplicate import is syntactically valid Go; only failed with a real
`go build`/`go test`, matching the "silent until the next build/test"
note above).

Confirmed directly: creating a scratch test file in `internal/mcp`
itself (which already aliases `sdkmcp` in `server.go`) reproduced the
exact `sdkmcp redeclared in this block` error pre-fix, and produced a
clean single-occurrence import post-fix.

**Fix** (`internal/mcp/server.go`, `handleCreate`): reuse
`sliceDecls(args.Body)` to extract the def's body with any leading
import block stripped (falling back to the plain package-strip if
`sliceDecls` somehow doesn't apply), then unconditionally call
`extractAliasedImports` + `patchImportOnDisk` after a successful
commit — `patchImportOnDisk` is idempotent (a no-op when the alias is
already present via the canonical per-module union), so this is safe
to run every time rather than gated on a build-failure signal this
path never had in the first place. This mirrors the `#367` mechanism
`handleCreateMultiDecl` already uses, adapted for the single-decl
path's weaker (parse-only) build check.

Regression test:
`TestHandleCreate_SingleDeclLeadingImportBlockNotDuplicatedInEmittedFile`
(`internal/mcp/server_test.go`).
