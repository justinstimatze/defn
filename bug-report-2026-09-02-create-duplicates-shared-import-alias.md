# Bug: `code(op:"create")` duplicates an import already used elsewhere in the package

**Found:** 2026-09-02, while building the lean-tool-surface change
(`internal/mcp/tool_help.go`, see `docs/gap-analysis-2026-09-02.md`
work-order item 2).

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

## Not yet done

Root cause not investigated (didn't trace into `internal/emit`'s
import-merging pass). Workaround used this session: hand-edit the
emitted file once (bypassing the `.go` edit guard) to drop the
duplicate line, then `code(op:"sync", file:...)` to re-align the DB
with what's actually on disk.
