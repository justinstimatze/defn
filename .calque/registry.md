# calque registry — defn

Externalized "these must agree" memory. Each entry records an adjudicated
suspect calque surfaced. Re-`calque check` drops any collapsed pair
automatically; entries stay until the underlying code shape changes.

Boundary conventions used so far:
- `internal/projection/**/*.go` × `internal/mcp/**/*.go` — canonical Phase C
  boundary: engine functions in `projection`, MCP handlers in `mcp`. The
  handlers are thin dispatchers, so nearly every projection function has
  exactly one MCP counterpart. Those pairs are recorded as
  `handler-engine-dispatch` below.

## Baseline: 2026-07-07 (post-Phase-C v0.25.0 + extract-slice consolidation cddd51f)

Ran `calque scan --left "internal/projection/**/*.go" --right "internal/mcp/**/*.go"`.
Result: 13 pairs, 6 clusters. Adjudicated: **0 drift**, 5 contracted-twin-ok,
8 false-alarm.

## Adjudicated pairs

- pair: internal/projection/insert_precondition.go::InsertPrecondition | internal/mcp/server.go::server.handleInsertPrecondition
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: handler wraps engine; canonical is projection

- pair: internal/projection/wrap_in_defer.go::WrapInDefer | internal/mcp/server.go::server.handleWrapInDefer
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: handler wraps engine; canonical is projection

- pair: internal/projection/replace_slice.go::ReplaceSlice | internal/mcp/server.go::server.handleReplaceSlice
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: handler wraps engine; canonical is projection

- pair: internal/projection/add_import.go::AddImport | internal/mcp/server.go::server.handleAddImport
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: handler wraps engine; canonical is projection

- pair: internal/projection/rename_param.go::RenameParam | internal/mcp/server.go::server.handleRenameParam
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: handler wraps engine; canonical is projection

- pair: internal/projection/rename_param.go::RenameParam | internal/mcp/server.go::astRename
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: different scopes. RenameParam rewrites one param throughout a single
    function via ast.Object binding. astRename renames a definition across a
    whole file via name+local-decl heuristic (used by `code(op:"rename")`).
    Shared calls are the go/ast idiom, not a shared contract.

- pair: internal/projection/rename_param.go::RenameParam | internal/mcp/server.go::server.handleRename
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: `rename-param` renames a param inside a body; `rename` renames a
    top-level definition across the module.

- pair: internal/projection/replace_slice.go::interiorComments | internal/mcp/server.go::topLevelFlow
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: both parse a body with `"package p\n"` prefix but extract different
    things (interior comments in an offset range vs top-level statement kinds).
    Mild refactor opportunity for a `parseBodyWithPrefix` helper, but no
    contract drift.

- pair: internal/projection/replace_slice.go::replaceSliceRange | internal/mcp/server.go::server.handleReplaceSlice
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: internal helper of `ReplaceSlice`; the handler is unaware of it.

- pair: internal/projection/replace_slice.go::ReplaceSliceForce | internal/mcp/server.go::server.handleReplaceSlice
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: the handler dispatches to ReplaceSlice OR ReplaceSliceForce
    depending on args.Force — a design choice, not drift.

- pair: internal/projection/replace_slice.go::ReplaceSlice | internal/mcp/server.go::server.handleSlice
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: handleSlice is the READ counterpart (extract-and-return the AST
    slice); ReplaceSlice is the WRITE counterpart. Same vocabulary, not twins.

- pair: internal/projection/wrap_in_defer_test.go::TestWrapInDefer_ErrorCases | internal/mcp/server.go::server.handleWrapInDefer
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: unit test exercising the engine; shares vocabulary via the engine
    name only.

- pair: internal/projection/wrap_in_defer_test.go::TestWrapInDefer_ByteExactPUTGET | internal/mcp/server.go::server.handleWrapInDefer
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: unit test exercising the engine.

## Adjudicated clusters

- cluster: internal/mcp/server.go::server.handleCode | internal/mcp/server.go::server.handleApply | internal/mcp/server_test.go::TestHandleCodeValidation
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: op-dispatch table. handleCode is the main entry, handleApply routes
    batched mutations, and the test enumerates the same op set for input
    validation. If a new op is added, all three must be updated — this is the
    canonical shared contract, not drift. Consider extracting the op-name
    list to a package-level `validOps` map used by all three.

- cluster: internal/mcp/server.go::server.impactJSON | internal/mcp/server.go::server.handleBatchImpact | internal/mcp/server.go::server.handleTestCoverage
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: three projections of one `store.Impact`. impactJSON is the canonical
    single-def report (typed impactDefRef). handleBatchImpact emits a summary
    shape for N defs. handleTestCoverage emits a tests-only shape. The
    upstream `store.Impact` shape is the shared contract; if it changes, all
    three call sites update naturally because they read from the same struct.

- cluster: internal/mcp/server.go::server.inferFromBody | internal/mcp/server_test.go::TestExtractSignature | internal/mcp/server_test.go::TestInferFromBody
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: definition-kind vocabulary ("function", "method", "type",
    "interface", "const", "var"). Also duplicated in `internal/ingest/ingest.go`.
    Any new kind added by a future Go language version would need updating in
    ingest + mcp inference. Consider extracting to a canonical enum in
    `internal/store/kind.go` with parsing helpers.

- cluster: internal/mcp/server.go::server.handleApply | internal/mcp/server.go::server.handleCreate | internal/mcp/tools_extra.go::server.handleMove
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-07
  - note: shared handler helpers (`countTopLevelDecls`, `findModuleByFile`,
    `findModule`). Same layer, one canonical helper set — not a twin.

- cluster: internal/mcp/server.go::stmtKind | internal/projection/slices.go::Slices | internal/mcp/server_test.go::TestHandleOutline_LargeBodyReturnsOutline | internal/mcp/server_test.go::TestHandleSlice_MissingArgs | internal/mcp/server_test.go::TestHandleSlice_ReturnStmt | internal/projection/insert_precondition_test.go::TestInsertPrecondition_ErrorCases | internal/projection/replace_slice_test.go::TestReplaceSlice_ErrorCases
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: shared "assign"/"select"/"return" strings are the Go ast statement
    vocabulary — a standard-library-scale enum, not a private seam. stmtKind
    projects statements for the outline view; Slices matches AST subtrees for
    edit ops. Both consult `ast.ReturnStmt` because that IS Go, not because
    they share a hidden contract.

- cluster: internal/mcp/server.go::server.handleCode | internal/mcp/tools_extra.go::server.handleCodeDiff | internal/mcp/tools_extra.go::server.handleStatus
  - verdict: false-alarm
  - reviewed: 2026-07-07
  - note: shared "table" and "status" strings are common English; the three
    handlers do unrelated work.

## Added 2026-07-17 (read-file op)

- pair: internal/mcp/server.go::server.handleReadFile | internal/mcp/server.go::server.handleFileDefs
  - verdict: contracted-twin-ok (with known handleFileDefs bug flagged)
  - reviewed: 2026-07-17
  - note: both handlers call `s.db.FindDefinitionsByFile(dir, file, 0)` — two projections
    of one file's def set. `handleFileDefs` returns metadata-only JSON summary;
    `handleReadFile` returns doc + sig + body markdown per def in source order.
    Single-source data layer: `FindDefinitionsByFile` + `GetBodiesByDefIDs` (new).
    KNOWN DRIFT: dir-derivation for BARE filenames (no `/`) differs. handleFileDefs
    strips the `.go`/`_test.go` extension into a dir hint (e.g. "main.go" → "main"),
    which fails when the module path doesn't contain that stem (e.g. module
    "testproj" + file "main.go" → LIKE '%main%' misses). handleReadFile uses
    empty-dir (correct — permissive LIKE + exact source_file filter narrows).
    Followup: fix handleFileDefs to match handleReadFile's dir="" pattern.

## Added 2026-07-17 (delta-from-prior read op)

- cluster: internal/mcp/server.go::server.handleGetDefinition | internal/mcp/server.go::server.renderUpstreamMatch | internal/mcp/server.go::server.renderDivergedFromUpstream
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-17 (updated post-bench)
  - note: three renderers for the same read op, split by upstream-fingerprint state.
    `renderUpstreamMatch` fires when local structural hash equals a known upstream
    row → tag-only provenance form (header line + full:true hint, no doc/sig/body).
    The doc+sig version was reverted after bench/delta-prior/ showed the envelope
    inflated bytes 140-155% over the tiny library method bodies it was meant to
    replace. Doc/sig are freely available via `full: true`.
    `renderDivergedFromUpstream` fires when the def name exists upstream but no
    version's hash matches → full body + divergence note.
    `handleGetDefinition` tail fires otherwise (module unknown to corpus, or
    caller passed `full: true`) → the original body-in-fence form.
    Contract: all three must set `usageStats{Op: "read", ...}` and route through
    `withUsage(textResult(...), ...)` so structured output stays uniform.

- pair: internal/store/hash.go::HashBodyStructural | internal/store/hash.go::HashBody
  - verdict: contracted-twin-ok
  - reviewed: 2026-07-17
  - note: raw SHA256 vs AST-structural SHA256 of a body string. Twins by design —
    `HashBody` is used for exact-match cache keys (ingest freshness); `HashBodyStructural`
    is whitespace/comment-invariant, used to detect that local dep body matches a
    tagged upstream release even when comments/formatting drifted.

## Follow-up refactors (deferred, not drift)

None of these are gate-worthy — mild ergonomics wins surfaced by the scan:

1. `parseBodyWithPrefix(body) (*ast.File, *token.FileSet, error)` — used by
   nearly every projection edit op with the `"package p\n"` idiom.
2. Canonical `Kind` enum shared by ingest + mcp inference (would violate the
   "store must not import other internal packages" rule from CLAUDE.md, but
   a shared upstream package is possible).
3. Op-name list extracted from `handleCode` switch to a `map[string]struct{}`
   so `TestHandleCodeValidation` and `handleApply` read the same source.
