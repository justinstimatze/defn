# defn — code in high definition

[![CI](https://github.com/justinstimatze/defn/actions/workflows/ci.yml/badge.svg)](https://github.com/justinstimatze/defn/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/justinstimatze/defn)](https://goreportcard.com/report/github.com/justinstimatze/defn)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Release](https://img.shields.io/github/v/release/justinstimatze/defn)](https://github.com/justinstimatze/defn/releases)

**Your Go code lives in two places: files and a graph.** Both are complete, lossless representations of the same code — kept in sync automatically. Files work with every tool in the Go ecosystem. The graph adds structure: every function, method, type, and constant is a node; every call, reference, and interface implementation is an edge.

Edit through defn or edit files directly. Either side auto-syncs to the other. Either can recover the other.

defn ships as an MCP tool: point Claude Code, Codex, or any MCP-capable agent at a Go repo and it queries the graph instead of grepping files.

**"What breaks if I change this function?"** In [gin-gonic/gin](https://github.com/gin-gonic/gin), `(*responseWriter).WriteHeader` is called through the `http.ResponseWriter` interface, not directly — the kind of indirection grep can't follow:

| | Tool calls | Wire cost <sup>[1](#fn-cost)</sup> | Wall time | Callers found | Transitive | Tests |
|---|---|---|---|---|---|---|
| grep + read | 5 | 2,415 tokens | 1.5s | 3 of 11 | — | — |
| `code(op:"impact", name:"WriteHeader")` | 1 | 2,014 tokens | 0.02s | **11 of 11** | **306** | **226** |

grep can't tell `WriteHeader` from `WriteHeaderNow`, or a real call site from a same-named method on an unrelated test type (`mockWriter`, `interceptedWriter`) — sorting that out took 4 more reads and still missed the 8 callers that only exist through interface dispatch. Transitive callers and test coverage aren't things grep computes at all; they need a call graph.

<a name="fn-cost"></a><sub>1. This is the cost of one call, not a full session — an agent that uses defn *alongside* file tools rather than in place of them won't see this ratio hold across a whole session. Session-level cost is a separate, harder measurement; see [methodology](#methodology) below.</sub>

## What makes this different

**Files don't know about each other.** grep finds text matches, not callers. Reading `context.go` doesn't tell you that `responseWriter` satisfies `ResponseWriter`, or that changing `WriteHeader` breaks 226 tests through interface dispatch. That information exists in the type system but dies when you close your editor.

**defn makes it permanent.** It parses your code with `go/types` (the same type checker gopls uses) and stores the result in SQLite (`modernc.org/sqlite`, pure Go, no CGO). The reference graph persists across sessions, includes interface satisfaction, and is queryable:

```sql
-- Who calls Render and has no tests?
SELECT d.name FROM definitions d
JOIN refs r ON r.from_def = d.id
WHERE r.to_def = (SELECT id FROM definitions WHERE name = 'Render' AND receiver = '*Context')
AND d.test = FALSE
AND NOT EXISTS (
  SELECT 1 FROM refs r2
  JOIN definitions t ON t.id = r2.from_def AND t.test = TRUE
  WHERE r2.to_def = d.id
)
```

## Setup

```bash
go install github.com/justinstimatze/defn/cmd/defn@latest
go install golang.org/x/tools/cmd/goimports@latest

cd your-go-project
defn init .
```

Then start Claude Code or Codex and ask:

```
"What's the blast radius of changing the Render function?"
"Which functions have no test coverage?"
"Find all functions that handle authentication"
```

To remove everything: `rm -rf .defn`

Requires Go 1.26+ and `goimports`. Pure-Go build — no CGO, no icu4c, no external database. The `.defn/defn.db` SQLite file is a rebuildable artifact of `defn ingest`.

Upgrading the binary (`go install .../defn@latest`) doesn't affect an already-running `defn serve` — it keeps executing the old image until restarted. Run `defn status` to check for version skew, or `defn restart` to pick up the new binary.

## How it works

One MCP tool — `code` — with an `op` field. Your AI agent calls it naturally:

| What you ask | What defn does |
|---|---|
| "Show me Render" | `code(op:"read", name:"Render")` — full source, disambiguated by blast radius |
| "What depends on this?" | `code(op:"impact", name:"X")` — callers, transitives, test coverage, interface dispatch |
| "Change 3 lines in this function" | `code(op:"edit", name:"X", old_fragment:"...", new_fragment:"...")` — no need to provide the whole body |
| "Rewrite this function" | `code(op:"edit", name:"X", new_body:"...")` — full replacement, auto-emits + builds |
| "What's in this file?" | `code(op:"overview", file:"server.go")` — all definitions with caller/callee counts |
| "Rename across the codebase" | `code(op:"rename", old_name:"X", new_name:"Y")` — updates definition + all callers |
| "Run only affected tests" | `code(op:"test", name:"X")` — via reference graph, not `go test ./...` |
| "Simulate a change" | `code(op:"simulate", mutations:[...])` — throwaway branch, ripple report, discard |

<details>
<summary>All operations</summary>

| Op | What it does | Key params |
|---|---|---|
| `read` | Full source of a definition | `name` or `file:line` |
| `outline` | Compact projection: signature + doc + caller/callee summary + top-level flow. Falls back to `read` for bodies under 300 bytes (v0.24.2). | `name` |
| `slice` | Verbatim AST-role slice of a def: `signature`, `doc`, `body`, `error-branch`, `return`, `loop` (v0.24.2). Byte-exact against the source. | `name`, `slice` |
| `insert-precondition` | Insert `if <condition> { <ret> }` at function entry. Byte-exact PUTGET. `name` inferred when the DB has one non-test function (v0.25.0). | `condition`, `ret`, `name?` |
| `replace-slice` | Replace the Nth match of an AST-role slice with verbatim bytes. Byte-exact PUTGET. `name` inferred when the DB has one non-test function (v0.25.0). | `slice`, `index`, `new`, `name?` |
| `wrap-in-defer` | Insert `defer <body>` before the Nth top-level statement. Byte-exact PUTGET. `name` inferred when the DB has one non-test function (v0.25.0). | `stmt_index`, `defer_body`, `name?` |
| `rename-param` | Rename a value param or receiver via ast.Object scoping; shadowing is respected. ≡_gofmt PUTGET. `name` inferred when the DB has one non-test function (v0.25.0). | `old_param`, `new_param`, `name?` |
| `add-import` | Add an import path (with optional alias) to a file's module. Byte-exact goimports-canonical grouping. `file` inferred when the DB has one non-test .go file (v0.25.0). | `import_path`, `file?`, `alias?` |
| `search` | Find by name pattern (%) or body text | `pattern` |
| `impact` | Blast radius, callers, test coverage | `name` |
| `explain` | Signature + callers + callees + tests | `name` |
| `overview` | All definitions in a file with relationships | `file` |
| `similar` | Find definitions with similar signatures | `name` |
| `untested` | Definitions without test coverage | — |
| `edit` | Full body replace, OR fragment replace via `old_fragment`+`new_fragment` | `name` |
| `insert` | Insert code after an anchor string | `name`, `after`, `body` |
| `create` | Create def(s) from body — infers name/kind; with `file:` set, `body` may hold multiple declarations to author a whole new file in one call | `body`, optional `module`, `file?` |
| `delete` | Remove + clean up references | `name` |
| `rename` | Rename + update callers (AST-based, preserves comments) | `old_name`, `new_name` |
| `move` | Move to another module | `name`, `module` |
| `test` | Run only affected tests | `name` |
| `simulate` | Throwaway branch, apply mutations, ripple report | `mutations` |
| `apply` | Batch operations (transactional) | `operations` |
| `diff` | Uncommitted changes | — |
| `history` | Commit history for a definition | `name` |
| `find` | Definition at a file:line | `file`, `line` |
| `sync` | Re-ingest after file edits | — |
| `emit` | Emit the tree to a directory (works while serve holds the DB) | `out` |
| `query` | Read-only SQL | `sql` |
| `test-coverage` | Test names covering a definition | `name` |
| `batch-impact` | Combined blast radius for multiple definitions | `names` |
| `file-defs` | Map file path to definitions | `file` |
| `traverse` | BFS over the ref graph, direction + kind filtered | `name`, `direction` |
| `literals` | Composite literal fields (type/field/value) | `pattern`, `name`, `body` |
| `pragmas` | Comment pragmas (`//go:generate`, `//winze:...`) | `pattern` |
| `status` | Backend stats + uncommitted-file summary | — |
| `diff-defs` | Structural diff between two source snapshots | `from`, optional `to` |

</details>

Name-based ops accept `file:line` paths and `Receiver.Method` syntax (`Context.Render`, `(*Router).Handle`).

## Concurrency

Treat `.defn/defn.db` as a build artifact of the working tree. For branch experiments, use `git worktree` and run one `defn serve` per worktree — each gets a deterministic per-project port (FNV-hashed into 9420-9999) and auto-shares within a worktree.

Only one `defn serve` process can own a `.defn/` at a time — enforced via `syscall.Flock` on `.defn/serve.pid`. A concurrent CLI gets an actionable error, and `defn status` shows who's holding the lock along with any version skew between the running serve and the on-disk binary.

## Scale

Measured fresh against defn v0.26.6 (Aug 2026):

| Project | Lines | Defs | Refs | Init time |
|---------|-------|------|------|-----------|
| chi | 11,690 | 494 | 965 | 2.6s |
| gin | 24,099 | 1,877 | 3,815 | 32.4s |
| hugo | 218K | 10,221 | 22,209 | 7min *(not re-measured this pass)* |

Init is a one-time cost. Incremental resolve after edits is much faster.

## Limitations

- **Go only.** The type-checked reference graph requires `go/types`.
- **`rename` uses AST-based identifier replacement** — preserves comments and string literals. Local variables that shadow the definition name are detected and preserved (with a warning).

## License

Apache 2.0. Built on [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) (BSD-3) and [adit-code](https://github.com/justinstimatze/adit-code) research. See [INFLUENCES.md](INFLUENCES.md).
