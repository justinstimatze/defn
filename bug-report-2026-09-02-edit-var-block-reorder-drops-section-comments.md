# Bug: editing a var(...) block can reorder members alphabetically and drop section comments

**Found:** 2026-09-02. Reported by a sibling agent ("winze") over MCP
Dispatch (`msg-244468d7`) while editing `corpus/bootstrap.go` in a
different project via `code(op:"edit")`: one var-block edit came back
as a 291-line diff that reordered every var in the block alphabetically
and deleted section comments, for what should have been a small,
targeted change. Reverted and redid all 5 edits with plain string
replacement instead.

## Root causes (two, confirmed independently by direct repro against a
minimal scratch package + `defn ingest`/`defn emit`)

### 1. Ordering — FIXED same day (`internal/emit/emit.go`, `declOrderInSource`)

`writeFile`'s regenerate fallback re-sorts DB defs into on-disk order
using `declOrderInSource`, which mapped a byte offset to each
identifier. For members of a grouped `var (...)` / `const (...)` /
`type (...)` block, that map keyed **every spec** in the block to the
**same offset** — the enclosing `GenDecl`'s position, computed once
outside the per-spec loop — instead of each spec's own position.
`sort.SliceStable`'s tie-break (equal `iPos`/`jPos`) then falls through
to the defs' incoming order, which is alphabetical (the DB fetches
defs ordered by `source_file, kind, name`). Net effect: the instant a
file's regenerate path runs, every member of a var/const/type block
that ISN'T disambiguated by its own position gets silently resorted
alphabetically, discarding whatever grouping/order the author wrote.

Confirmed via direct repro: a 4-member var block
(`zebraHost, appleHost, mikeTimeout, beetTimeout`) forced through
`writeFile`'s regenerate path (by making one member's DB body
deliberately unparseable, so `mergeDeclsIntoSource` bails with
"spliced result doesn't parse") emitted as
`appleHost, beetTimeout, mikeTimeout, zebraHost` — pure alphabetical.

**Fix:** `declOrderInSource` now keys each `TypeSpec`/`ValueSpec` by
its own `s.Pos()` rather than the enclosing decl's `d.Pos()`. Verified
the same forced-regenerate repro now preserves original source order.
Regression test:
`TestDeclOrderInSourceOrdersVarBlockMembersBySpecPosition`
(`internal/emit/emit_test.go`).

### 2. Comment loss — NOT YET FIXED (ingest-side)

Ingest never captures a var/const block's inter-spec "section"
comments into **any** definition's `Body` at all. Confirmed via direct
SQL inspection of a freshly-ingested scratch DB: none of 4 var defs'
stored bodies contain their preceding section comment
(`// Section 1: ...`, `// Section 2: ...`), even though the comments
are legitimately present in the source and immediately precede their
following spec (i.e. they'd normally attach as that spec's Go doc
comment).

This is invisible as long as `mergeDeclsIntoSource` (the byte-splice
AST-merge path) succeeds: it only touches the exact byte range of a
replaced spec, so surrounding comments pass through as untouched raw
bytes regardless of whether they were ever captured into the DB. But
the moment ANY def in the file forces the regenerate fallback (a
malformed edit, an unmatched def, an invalid byte range — see
`writeFile`'s `ok=false` branches in `mergeDeclsIntoSource`), the
**entire file** gets rebuilt by concatenating stored bodies alone, and
since none of them contain the section comments, they are silently and
permanently gone — for every var in the block, not just whichever one
triggered the fallback.

This means a SINGLE bad/malformed edit anywhere in a file can
destructively drop comments on OTHER, correctly-edited, unrelated
parts of the same file. Confirmed via the same forced-regenerate
repro as above: the regenerated output has zero occurrences of either
section comment.

## Impact

Any edit to a member of a non-trivial `var (...)` / `const (...)` /
`type (...)` block, in a file where regenerate ends up running for any
reason, permanently drops that block's inter-spec comments. The
ordering half of this (item 1) is now fixed; comment capture (item 2)
is not — this is violates the CLAUDE.md guarantee "the database is a
lossless representation of the source" for this specific case
(floating comments between grouped-block specs).

## Not yet done

Fixing item 2 properly requires ingest to capture inter-spec floating
comments (either attach them into the following spec's stored `Body`,
or store them separately and have the regenerate path re-splice them
back in) — a bigger, riskier ingest-side change than item 1's,
deserving its own dedicated investigation rather than a same-session
bolt-on. A narrower, lower-risk alternative worth considering instead
of (or in addition to) fixing ingest capture: make the regenerate path
itself safer by isolating a single bad def's fallback to just that
def (falling back to per-def regeneration/failure) rather than
regenerating the WHOLE file and losing every other def's comments
collaterally.
