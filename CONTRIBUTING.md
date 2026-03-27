# Contributing to defn

## Development Setup

```bash
git clone https://github.com/justinstimatze/defn.git
cd defn
CGO_ENABLED=1 go build ./cmd/defn
go test ./... -count=1
```

Requires Go 1.25+ and CGO (for Dolt embedded database).

## Running Checks

```bash
go test ./... -count=1                    # Unit tests
go run ./cmd/defn-test                    # Integration tests (clones real repos)
go vet ./...                              # Vet
golangci-lint run ./cmd/... ./internal/...  # Lint (optional)
```

## Self-Hosting

defn is developed using its own MCP tools:

```bash
defn init .                               # Index defn in itself
claude                                    # Use code(op:"impact"), code(op:"read"), etc.
```

## Architecture

```
cmd/defn/           CLI (init, ingest, emit, serve, impact, untested, ...)
cmd/defn-test/      Integration tests against chi, mux, gin, toml
cmd/defn-bench/     Token/tool-call benchmark
internal/store/     Dolt database layer
internal/ingest/    Go source parsing (go/ast + go/types)
internal/resolve/   Reference graph (go/types, test packages, receiver-qualified)
internal/emit/      Database → .go files
internal/goload/    Shared Go package loading utilities
internal/lint/      Emit → golangci-lint → remap to definitions
internal/mcp/       MCP server (single `code` tool, op dispatch)
```

## Conventions

- All dependencies must be MIT, BSD-2, or Apache 2.0 licensed
- `internal/store/` must not import other internal packages
- MCP tools go in `internal/mcp/`

## License

By contributing, you agree that your contributions will be licensed under Apache 2.0.
