# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/justinstimatze/defn/security/advisories/new).

Do not open a public issue for security vulnerabilities.

## Scope

defn stores source code in a SQLite database (`modernc.org/sqlite`, pure Go)
and exposes it via MCP tools. The security-relevant surfaces are:

- **SQL injection via `code(op:"query")`**: Mitigated by prefix validation (only
  SELECT, WITH, EXPLAIN, and PRAGMA are allowed) and single-statement enforcement.
  Note: read-only queries can still access all tables including `bodies` (full
  source code). The database contains the same code as the repository — treat
  access as equivalent to repo access.
- **MCP tool access**: The MCP server runs locally on stdio. No network
  exposure. Access is limited to the Claude Code process that started it.
- **File emission**: `defn emit` writes files to a specified directory.
  Paths are derived from module paths in the database, not user input.
- **Go source parsing**: Source files are parsed by Go's standard `go/ast`
  and `go/types` packages. No code execution during parsing. `code(op:"sync")`
  re-parses the entire project in-process — same parsing, no execution.
- **SQLite database**: Stored in `.defn/defn.db` (WAL mode). No authentication —
  anyone with filesystem access can read/modify the database. `defn serve` binds
  to 127.0.0.1 only. The database contains full source code — treat it as
  sensitive as the repo itself.

## Supported Versions

Only the latest release is supported with security updates.
