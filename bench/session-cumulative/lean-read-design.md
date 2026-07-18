# Lean `read` output — design memo

*2026-07-17. Follow-up to chi-explore bench which localized the
defn-vs-files +13% gap to `cache_creation_1h` (+21-28%).*

## What's shipped today

`handleGetDefinition` returns:

    ## (r *Reader) Read (method)
    Module: github.com/example/pkg

    Read returns the next N bytes.

    ```go
    func (r *Reader) Read(p []byte) (int, error) { ... }
    ```

Bytes on a routeHTTP-sized def (~1200 char body): ~1245 total, of
which envelope is ~65 bytes = ~5-6% of output.

Across 10 read calls per session, envelope contributes ~600-800 bytes
to cache_creation_1h. Not huge alone, but the whole remaining
defn-vs-files gap is only ~50k tokens of cache_creation. Envelope is
one contributor; there may be others.

## The proposal

Strip the envelope on `read` when it's redundant with context the
model already has:

    // Read returns the next N bytes.
    func (r *Reader) Read(p []byte) (int, error) { ... }

Rationale for what to keep vs cut:

- `## Name (kind)` — the model made the tool call with `name`. It
  knows what it asked for. Cut.
- `Module: X` — SourceFile is already in `impact`/`expand` when
  needed; for `read`, the module is inferrable from context in
  almost all cases. Cut.
- Doc comment — model needs this for meaning. Keep, prepended
  inside the Go fence as `// doc` so it's not a separate paragraph.
- Body — the point of the call. Keep.

**Structural claim:** the envelope is mostly redundant with the
model's own tool_use context. Cut it entirely; the fewer bytes go
into cache_creation.

## When to KEEP the envelope

Ambiguity. If defn disambiguated (multiple defs share the name and
we picked one by blast-radius), the model needs to know WHICH one it
got. Keep header in that specific case.

Ambiguity detection at the store layer: `GetDefinitionByName` picks
one when multiple match. If the disambiguation kicked in, emit a
one-line disambig header:

    // picked: (r *Reader) Read (method, mux.go:63) among 3 matches

That's ~60 bytes when needed, 0 when not.

## Byte comparison (projected on chi)

    def          current  proposed  delta
    Timeout       1881    ~1810     -3.8%
    Middlewares    369     ~305    -17.3%
    Mux           1750    ~1685     -3.7%
    Router        1964    ~1900     -3.3%
    Route          614     ~550    -10.4%
    ServeHTTP     1502    ~1440     -4.1%
    routeHTTP     1245    ~1180     -5.2%
    Handle         495     ~430    -13.1%

Average per-call reduction: ~7%. Small defs get 10-17% reduction
(envelope is a bigger fraction of small outputs).

Cache_creation_1h is set by tool_result bytes going into first-use
cached content. If each read call is 7% smaller, and the model made
~7 reads in defn-natural turn 1, that's ~5% total cache_creation
reduction from lean read alone.

Not enough to close the +21% gap by itself — but it's a
correctness-preserving optimization we can compose with other work
(batched multi-def output, terser impact/expand format).

## Non-goals

- Don't change `expand` format. Expand's headers exist because the
  tool_result contains multiple sections (body, callers, ...).
  Cutting section headers would hurt readability.
- Don't change `impact` format. Impact's compact header ties body to
  caller list.
- Don't change error output. Error messages need to be self-describing.

## Preserve correctness

Rubric (from chi-explore-ground-truth.md): turn 2 answer must
identify the correct 2 direct callers, no phantoms. The lean read
returns the same body + doc; only the header changes. Correctness
should be unaffected.

Bench gate: chi-explore.txt on the lean-read arm must:
1. Answer all 10 turns correctly (spot-check turn 2 at minimum).
2. Reduce `cache_creation_1h` by ≥5% vs 2026-07-17 defn-natural
   baseline (275,979 tokens).
3. Total USD cost ≤ files baseline ($10.21) — the ultimate gate.

If (1) fails, revert. If (2) passes but (3) fails, we've closed some
but not enough of the gap — keep the change, keep iterating.
If (2) fails, envelope wasn't the driver; look elsewhere.

## Implementation sketch

1. Add a helper `renderReadCompact(d, modulePath, disambiguated bool)`.
2. In `handleGetDefinition`, replace the current envelope + fence
   with:

    ```go
    fmt.Fprintf(&sb, "```go\n")
    if disambiguated {
        fmt.Fprintf(&sb, "// picked: %s%s (%s, %s:%d) among matches\n",
            formatReceiver(d.Receiver), d.Name, d.Kind, d.SourceFile, d.StartLine)
    }
    if d.Doc != "" {
        // Prepend "// " to each doc line if not already commented.
        writeDocAsGoComment(&sb, d.Doc)
    }
    fmt.Fprintf(&sb, "%s\n```\n", d.Body)
    ```

3. Guard the D delta-from-prior branch (unchanged; it uses tag-only).

4. Add tests: `TestHandleRead_LeanFormat` verifies no ## header,
   no Module line; body and doc present.

5. Update the read-op preamble in commands.go if the schema changes.
   (It doesn't — this is a rendering change only.)

## Sequencing

1. Implement lean read + tests.
2. `go install ./cmd/defn`.
3. Rebuild chi-defn ingest (or just restart defn serve).
4. Fire one paid arm: `defn-natural-lean` on chi-explore.txt.
5. Analyze: compare to `files` and 2026-07-17 `defn-natural`.
6. If cost ≤ files baseline: ship. Else: keep the change (structural
   improvement even without full parity) and continue investigating.

## What this design deliberately does NOT claim

- Not claiming lean read alone closes the gap. It's ~5% headroom on
  a ~21% overhead — the structural fix, not a silver bullet.
- Not claiming envelope is the only overhead source. Impact/expand
  outputs still have their own envelope; may need similar treatment
  if this one gates positive.
- Not claiming this is done. If the paid arm shows lean read
  doesn't move cache_creation, the structural defect is deeper —
  we need to look at something else, not paper over it with more
  format tweaks.

## Postscript — 2026-07-17 trial result and revert

Implemented as described (strip doc paragraph from `read` and
`expand`; body already contains doc as //-lines). Fired paid arm on
chi-explore.txt.

Result:

    Arm                 tool_uses  cache_creation_1h  cache_read  cost
    files (baseline)     13         227,034          2,247,096   $10.21
    defn-natural         10         275,979          2,141,421   $11.50
    defn-natural-lean    16         261,922          2,738,217   $11.98

The dedup DID reduce `cache_creation_1h` by 5% (275k → 262k), as
predicted. But the model responded by making **60% more tool calls**
(10 → 16 across the session, 5 extra in turn 1 alone). Cache_read
blew up 28% and total cost rose to +17.3% vs files.

**Working hypothesis:** the doc-paragraph-before-fence was serving a
purpose the design memo underestimated — a prose TL;DR at the top of
the read output, OUTSIDE the code fence. The model apparently reads
prose-format doc more efficiently than the same content as
//-prefixed lines inside a Go fence. Stripping the paragraph made
the model reach for more calls to compensate.

**Reverted** in the same session. Both `handleGetDefinition` and
`handleExpand` are back to emitting the doc paragraph.

**Meta-lesson:** byte-count reduction can look correct structurally
(content really was duplicated) but harm observable model behavior.
Any future output-format optimization must gate on total session cost
in a paid bench, NOT on per-call byte savings in isolation. This is
exactly the "papering over structural defect with clever mitigation"
concern the user raised — the mitigation looked correct on paper and
failed in practice.

**Followup ideas for a later session:**
- Try the reverse — KEEP the paragraph but strip the doc's //-lines
  from the fenced body. Same dedup, preserves the prose TL;DR format.
  Requires parsing Go source to identify leading doc comments.
- Investigate WHY the model made more calls under lean output.
  Was it re-reading defs it already saw? Chasing side references?
  Turn-1 transcript analysis would tell us.
- Multi-trial to separate variance from real regression. This was
  a single trial. But direction was clear enough to revert.
