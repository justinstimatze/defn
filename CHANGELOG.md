# Changelog

All notable changes to defn will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/).

## [0.1.0] - 2026-03-22

### Added
- Core: definitions stored in Dolt (SQL database with git semantics)
- Single `code` MCP tool with op dispatch (DCL architecture — 46% fewer input tokens vs file tools)
- 18 operations: read, search, impact, explain, similar, untested, edit, create, delete, rename, move, test, apply, diff, history, query, find, sync
- All name params accept file:line syntax (e.g. "internal/mcp/server.go:272") for location-first workflows
- Go language support via go/ast + go/types (type-checked references)
- Impact analysis: blast radius, transitive callers, test coverage per definition
- Smart disambiguation: ambiguous names resolved by most non-test callers
- Reference resolution: includes test packages, receiver-qualified method lookups
- Emit: database → compilable .go files (round-trip verified on chi, mux, gin, toml)
- Auto-emit on edit: edit op updates DB and files simultaneously
- Incremental resolve: edit op and create op only re-resolve the changed module
- In-process resolve: no DB lock conflicts, no dependency on defn binary in PATH
- Greenfield authoring: create op infers name/kind/test from body, apply op batches operations
- Atomic rename: rename op updates definition and all callers across codebase
- Definition-level test targeting: test op runs only affected tests via reference graph
- Dolt versioning: branch, checkout, merge, commit, diff, log — native on definitions
- defn init: auto-configures .mcp.json, CLAUDE.md, .gitignore
- defn watch: auto-re-ingest on file changes (legacy mode)
- Claude Code hooks: auto-init on session start, auto-reingest on file edit
- Integration tests against 4 real Go projects (chi, mux, gin, toml)
- 8 workflow tests covering SWE-bench operation patterns
- Self-hosted: defn is developed using its own MCP tools

### Experimental (not production-ready)
- rename op (string replacement, not AST transformation)
- move op (basic module reassignment)

### Benchmarked (single runs, not averaged)
- claude -p on gin: defn finds 21/21 callers + 134 transitives + 111 tests; file tools find 19/21 with no transitives or test count
- Round-trip: ingest → emit → go build verified on chi, mux, gin, toml
- Scale: chi (10K lines, 11s), gin (24K, 48s), hugo (218K, 7min)

### Known Limitations
- Go only (type-checked references require go/types)
- Binary ~140MB due to embedded Dolt (requires CGO for zstd)
- rename op, delete op, apply op use full resolve (touches multiple modules)
- rename op is string replacement, may corrupt comments/strings
