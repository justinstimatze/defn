# defn — code in high definition

[![CI](https://github.com/justinstimatze/defn/actions/workflows/ci.yml/badge.svg)](https://github.com/justinstimatze/defn/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/justinstimatze/defn)](https://goreportcard.com/report/github.com/justinstimatze/defn)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Release](https://img.shields.io/github/v/release/justinstimatze/defn)](https://github.com/justinstimatze/defn/releases)

AI-native code database for Go. A **defn** is a definition — the atomic unit of code. Navigate, edit, and understand Go code by structure instead of by file.

## Why

AI agents spend most of their time *finding* code, not *changing* it. On gin-gonic/gin:

**Reading a function — without defn:**
```
grep "Render" → 269 results across dozens of files
Read context.go (1,489 lines) → scroll to find the right Render
There are 20+ definitions named "Render" — which one?
4+ tool calls, ~8K tokens of file content.
```

**Reading a function — with defn:**
```
code(op: "read", name: "Render")
```
One call. defn disambiguates by blast radius — returns `(*Context).Render`, the one with the most callers.

**Editing — without defn:**
```
Read context.go → find function → craft str_replace with enough
surrounding context to be unique → hope it matches exactly one location
```

**Editing — with defn:**
```
code(op: "edit", name: "Render", new_body: "func (c *Context) Render...")
```

This extends to harder queries. We asked Claude "blast radius of Render" on gin via `claude -p`:

<details>
<summary><b>Without defn</b> — 9 tool calls, 144K input tokens, 45s</summary>

Claude used 9 Grep/Read calls to find callers of Render in context.go. Found 19 of 21 callers. Couldn't compute transitive callers or test coverage — would require reading every test file and tracing call chains manually.
</details>

<details>
<summary><b>With defn</b> — 2 tool calls, 51K input tokens, 12s</summary>

Claude called `code(op:"impact", name:"Context.Render")`. defn parsed the receiver, found `(*Context).Render`, and returned all 21 callers, 134 transitive callers, and 111 covering tests in one response.
</details>

**65% fewer tokens, 75% faster, more complete answer.** Measured via `claude -p` on gin-gonic/gin. Results vary by run.

## Setup

```bash
go install github.com/justinstimatze/defn/cmd/defn@latest
go install golang.org/x/tools/cmd/goimports@latest

cd your-go-project
defn init . --server    # recommended: starts Dolt server for multi-agent
defn init .             # or: embedded mode, no server needed
```

To remove everything: `defn clean`

Requires Go 1.25+, CGO, and `goimports`. Binary is ~140MB due to embedded Dolt engine.

**macOS:** install ICU first: `brew install icu4c` then build with:
```bash
export CGO_CFLAGS="-I$(brew --prefix icu4c)/include"
export CGO_LDFLAGS="-L$(brew --prefix icu4c)/lib"
go install github.com/justinstimatze/defn/cmd/defn@latest
```

`defn init` parses your Go source, stores definitions and references in [Dolt](https://www.dolthub.com/), and configures MCP for Claude Code and OpenAI Codex.

The database and files are **kept in sync**. Edits via defn update the database and emit files. File edits are auto-detected and re-ingested. Either can recover the other: `defn init` rebuilds the database from files; `defn emit` recreates files from the database. The `.defn/` directory is gitignored — rebuild on clone with `defn init`.

### Embedded mode

`defn init .` creates a `.defn/` directory with an embedded database. No server needed. Good for single-user workflows.

### Server mode (recommended)

`defn init . --server` starts a Dolt server automatically:
- Installs to `.defn-server/`, binds to `127.0.0.1:3307`
- Creates a `defn` user with a cryptographically random password (no root access)
- Configures MCP with the authenticated DSN
- Requires [Dolt](https://github.com/dolthub/dolt/releases) installed

Manage the server: `defn server start`, `defn server stop`, `defn server status`.

**Why server mode:**
- Multiple agents work concurrently (each session has its own branch)
- Standard MySQL clients for debugging (`mysql -h 127.0.0.1 -P 3307`)
- `defn push` / `defn pull` to [DoltHub](https://www.dolthub.com/) for cloud hosting

### Try it

After `defn init`, start Claude Code or Codex and ask:

```
"What's the blast radius of changing the Render function?"
"Which functions have no test coverage?"
"Find all functions that handle authentication"
```

## How agents actually spend their time

Analysis of [SWE-bench agent trajectories](https://huggingface.co/datasets/nebius/SWE-rebench-openhands-trajectories) (via [adit-code](https://github.com/justinstimatze/adit-code)) across 1,840 files and 49 repos found that file size is the strongest predictor of agent cost. The common operations defn replaces:

| Operation | With files | With defn |
|---|---|---|
| **Find a function** | grep → read file → scroll | `code(op:"read", name:"X")` |
| **Edit a function** | read file → find it → str_replace | `code(op:"edit", name:"X", body:...)` |
| **Understand callers** | grep → disambiguate → read each | `code(op:"impact", name:"X")` |
| **Run affected tests** | `go test ./...` (everything) | `code(op:"test", name:"X")` |
| **Rename across codebase** | grep → edit each file | `code(op:"rename", old_name:"X", new_name:"Y")` |

Name-based ops accept `file:line` paths and `Receiver.Method` syntax.

## Tool

One MCP tool — `code` — with an `op` field. Single tool schema in context instead of 17 separate tool definitions, following the [Dynamic Context Loading](https://cefboud.com/posts/dynamic-context-loading-llm-mcp/) pattern.

```
code(op: "read", name: "Render")
code(op: "impact", name: "Render")
code(op: "edit", name: "Render", new_body: "func ...")
code(op: "search", pattern: "%Auth%")
```

**Operations:**

| Op | What it does | Key params |
|---|---|---|
| `read` | Full source of a definition | `name` or `file:line` |
| `search` | Find by name pattern (%) or body text | `pattern` |
| `impact` | Blast radius, callers, test coverage | `name` |
| `explain` | Signature + callers + callees + tests | `name` |
| `similar` | Find definitions with similar signatures | `name` |
| `untested` | Definitions without test coverage | — |
| `edit` | Replace body, auto-emit + build | `name`, `new_body` |
| `create` | Create (infers name/kind from body) | `body`, optional `module` |
| `delete` | Remove + clean up references | `name` |
| `rename` | Rename + update callers (string replacement) | `old_name`, `new_name` |
| `move` | Move to another module | `name`, `module` |
| `test` | Run only affected tests | `name` |
| `apply` | Batch operations | `operations` |
| `diff` | Uncommitted changes | — |
| `history` | Commit history for a definition | `name` |
| `find` | Definition at a file:line | `file`, `line` |
| `sync` | Re-ingest after file edits | — |
| `query` | Read-only SQL | `sql` |

Write operations auto-emit files and auto-resolve references. The MCP server watches for external file changes and auto-reingests, so editing via file tools stays in sync. Set `DEFN_LEGACY=1` to disable auto-emit.

Configuration via `defn.toml` (optional):
```toml
db = ".defn"        # or a MySQL DSN for server mode
port = "3307"       # server port
build-timeout = "30s"
test-timeout = "60s"
```

## What it stores

Each function, method, type, and constant is a row. References between them are rows. Test coverage is derived from the reference graph.

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

## Versioning

Dolt gives git semantics on SQL data. Branch, merge, diff, commit — natively, on definitions.

```bash
defn branch feature
defn checkout feature
# make changes
defn commit "add auth middleware"
defn checkout main
defn merge feature
```

**Multi-agent workflow (server mode):**

```bash
# Each agent gets its own branch on the shared server:
defn branch agent-1 && defn checkout agent-1
# ... agent 1 works ...
defn commit "add auth middleware"

defn branch agent-2 && defn checkout agent-2
# ... agent 2 works ...
defn commit "refactor handlers"

# Merge both back
defn checkout main
defn merge agent-1
defn merge agent-2
```

In server mode, each MySQL session has its own branch context via `CALL DOLT_CHECKOUT` — multiple agents work concurrently on the same Dolt server without interference. In embedded mode, `defn worktree <name>` copies the database to a separate directory.

For remote collaboration (e.g. pushing to [DoltHub](https://www.dolthub.com/)): `defn push origin main`.

Dolt handles row-level merge. Semantic conflicts (e.g. one agent renames a function, another adds a caller) require manual resolution.

## Limitations

- **Inline comments between statements are lost** on round-trip. Doc comments on functions/types and package doc comments are preserved. Comments within expressions are preserved. But standalone comments like `// Step 2: do X` are dropped by `go/ast` re-printing. Use doc comments instead.
- **Go only.** The type-checked reference graph requires `go/types`. Other languages would need their own type checkers.
- **`rename` op is string replacement**, not AST transformation. May affect comments/strings containing the name.
- **Emit produces one file per package.** Multi-file package layouts (foo.go, bar.go) are collapsed to a single file. Original file structure is not preserved.
- **`apply` op is not atomic.** Partial failures leave the database in a mixed state. Check the response for per-operation errors.

## Self-hosting

defn can ingest, emit, and rebuild itself:

```bash
defn ingest . && defn emit /tmp/out
cd /tmp/out && go build ./cmd/defn/
```

Verified on go-chi/chi, gorilla/mux, gin-gonic/gin, BurntSushi/toml.

## Scale

| Project | Lines | Defs | Refs | Init time | DB size |
|---------|-------|------|------|-----------|---------|
| chi | 10K | 370 | 704 | 11s | 15M |
| gin | 24K | 1,580 | 3,829 | 48s | 81M |
| hugo | 218K | 10,221 | 22,209 | 7min | 714M |

Tested up to ~200K lines. Projects with cgo imports (moby) or Go 1.26+ (kubernetes) are not yet supported. Init is a one-time cost — incremental resolve after edits is much faster.

## Why not gopls?

gopls answers "who calls X?" too. But it can't compose callers + transitives + test coverage into one answer. It doesn't persist across sessions. It doesn't version definitions. It needs `file:line:col` — defn just needs a name. And it can't edit, rename, or delete.

## License

MIT. Built on [Dolt](https://github.com/dolthub/dolt) (Apache 2.0) and [adit-code](https://github.com/justinstimatze/adit-code) research. See [INFLUENCES.md](INFLUENCES.md).
