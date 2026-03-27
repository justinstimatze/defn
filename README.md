# defn — code in high definition

[![CI](https://github.com/justinstimatze/defn/actions/workflows/ci.yml/badge.svg)](https://github.com/justinstimatze/defn/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/justinstimatze/defn)](https://goreportcard.com/report/github.com/justinstimatze/defn)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Release](https://img.shields.io/github/v/release/justinstimatze/defn)](https://github.com/justinstimatze/defn/releases)

**defn replaces files with a graph.** Every Go function, method, type, and constant becomes a node. Every call, reference, and interface implementation becomes an edge. AI agents query the graph instead of grepping through files.

**The round-trip is lossless.** defn and your files stay perfectly in sync — all comments, file structure, and definitions are preserved. Edit through defn or edit files directly; the database auto-detects changes and re-ingests. Either can recover the other.

**"What breaks if I change Render?"**

> **Without defn:** grep → read 5 files → scroll → guess
> 9 calls | 144K tokens | 45s | found 19 of 21 callers | no transitives | no test count

> **With defn:** `code(op:"impact", name:"Render")`
> **2 calls | 51K tokens | 12s | 33 callers | 341 transitive | 238 tests** — including interface dispatch

## What makes this different

**Files don't know about each other.** grep finds text matches, not callers. Reading `context.go` doesn't tell you that `responseWriter` satisfies `ResponseWriter`, or that changing `WriteHeader` breaks 238 tests through interface dispatch. That information exists in the type system but dies when you close your editor.

**defn makes it permanent.** It parses your code with `go/types` (the same type checker gopls uses) and stores the result in [Dolt](https://www.dolthub.com/) — a SQL database with git semantics. The reference graph persists across sessions, includes interface satisfaction, and is queryable:

```sql
-- Who calls Render and has no tests?
SELECT d.name FROM definitions d
JOIN `references` r ON r.from_def = d.id
WHERE r.to_def = (SELECT id FROM definitions WHERE name = 'Render' AND receiver = '*Context')
AND d.test = FALSE
AND NOT EXISTS (
  SELECT 1 FROM `references` r2
  JOIN definitions t ON t.id = r2.from_def AND t.test = TRUE
  WHERE r2.to_def = d.id
)
```

## Setup

```bash
go install github.com/justinstimatze/defn/cmd/defn@latest
go install golang.org/x/tools/cmd/goimports@latest

cd your-go-project
defn init . --server    # recommended: starts Dolt server for multi-agent
defn init .             # or: embedded mode, no server needed
```

Then start Claude Code or Codex and ask:

```
"What's the blast radius of changing the Render function?"
"Which functions have no test coverage?"
"Find all functions that handle authentication"
```

To remove everything: `defn clean`

Requires Go 1.26+, CGO, and `goimports`. Binary is ~140MB due to embedded Dolt engine.

**macOS:** `brew install icu4c` then:
```bash
export CGO_CFLAGS="-I$(brew --prefix icu4c)/include"
export CGO_LDFLAGS="-L$(brew --prefix icu4c)/lib"
go install github.com/justinstimatze/defn/cmd/defn@latest
```

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
| `search` | Find by name pattern (%) or body text | `pattern` |
| `impact` | Blast radius, callers, test coverage | `name` |
| `explain` | Signature + callers + callees + tests | `name` |
| `overview` | All definitions in a file with relationships | `file` |
| `similar` | Find definitions with similar signatures | `name` |
| `untested` | Definitions without test coverage | — |
| `edit` | Full body replace, OR fragment replace via `old_fragment`+`new_fragment` | `name` |
| `insert` | Insert code after an anchor string | `name`, `after`, `body` |
| `create` | Create (infers name/kind from body) | `body`, optional `module` |
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
| `query` | Read-only SQL | `sql` |
| `test-coverage` | Test names covering a definition | `name` |
| `batch-impact` | Combined blast radius for multiple definitions | `names` |
| `file-defs` | Map file path to definitions | `file` |

</details>

Name-based ops accept `file:line` paths and `Receiver.Method` syntax (`Context.Render`, `(*Router).Handle`).

## Versioning

Dolt gives git semantics on SQL data. Branch, merge, diff, commit — natively, on definitions.

```bash
defn branch feature && defn checkout feature
# make changes
defn commit "add auth middleware"
defn checkout main && defn merge feature
```

In server mode, multiple agents branch and merge concurrently on the same Dolt server.

## Scale

| Project | Lines | Defs | Refs | Init time |
|---------|-------|------|------|-----------|
| chi | 10K | 370 | 704 | 11s |
| gin | 24K | 1,580 | 3,829 | 48s |
| hugo | 218K | 10,221 | 22,209 | 7min |

Init is a one-time cost. Incremental resolve after edits is much faster.

## Limitations

- **Go only.** The type-checked reference graph requires `go/types`.
- **`rename` uses AST-based identifier replacement** — preserves comments and string literals. Local variables that shadow the definition name are detected and preserved (with a warning).

## License

Apache 2.0. Built on [Dolt](https://github.com/dolthub/dolt) (Apache 2.0) and [adit-code](https://github.com/justinstimatze/adit-code) research. See [INFLUENCES.md](INFLUENCES.md).
