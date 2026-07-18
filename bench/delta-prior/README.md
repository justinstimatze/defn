# bench/delta-prior — D delta-from-prior byte measurement

Measures raw byte savings from the D projection: return a compact
provenance tag when a local library dep is unchanged from a tagged
upstream release, instead of the full body.

## What it does

For each target symbol, `measure` records five renderings and compares:

1. `files_bytes` — whole file containing the symbol (files-mode `Read` equivalent). Each file counted once.
2. `full_bytes` — defn's current `handleGetDefinition` body-in-fence rendering (`defn-natural`).
3. `compact_bytes` — `renderUpstreamMatch` as shipped (header + doc + sig + tag) (`defn-D as shipped`).
4. `tag_only_bytes` — minimal variant: header line + "unchanged upstream" hint, no doc, no sig.
5. `adaptive_bytes` — whichever of the three is smallest.

## Result 2026-07-17: chi v5.1.0 + gin v1.10.0

**Two runs before and after the render rework:**

| Corpus | files | defn-natural | D as first shipped (f533842) | D after rework | tag-only (minimum floor) |
|---|---:|---:|---:|---:|---:|
| chi (20 defs) | 27,095 | 4,080 | 10,430 (**−156%**) | **2,568 (+37%)** | 1,176 (+71%) |
| gin (19 defs) | 72,089 | 5,557 | 13,130 (**−136%**) | **2,525 (+55%)** | 1,210 (+78%) |

Percentages are byte reductions vs `defn-natural` (the current body-in-fence
rendering). Negative means D is *bigger* than defn-natural.

### The story

1. **defn-natural already dominates files-mode by ~85-92%** on
   library-symbol reads — the "narrow body vs whole file" story is
   real without any D involvement.

2. **D as first shipped (f533842) INFLATED bytes** by 140-155% because
   the doc + sig envelope exceeded the small library method bodies
   (chi/gin ranges 100-600 bytes; envelope 350-1300).

3. **The fix** (this commit): drop doc + sig from `renderUpstreamMatch`
   — keep just the header line and the `full: true` escape hatch.
   Yields +37% (chi) / +55% (gin) real byte savings on top of
   defn-natural. Doc and sig are still available on demand via
   `full: true`.

4. **A tag-only floor** (~60 bytes, header-line only) would save
   71-78% but loses the markdown structure other read-op outputs use.
   Kept as a measurement floor in the bench for comparison, not
   shipped.

Open questions the byte number cannot answer:

- Will the model actually trust the tag and skip a follow-up `full:
  true` call? Correctness measurement requires a real session bench,
  not this micro-bench.
- Does the tag give enough context that the model does not have to
  fall back to whole-file Read to disambiguate?

## How to reproduce

```bash
# Setup (in scratch dir):
git clone --depth 1 --branch v5.1.0 https://github.com/go-chi/chi.git chi
cd chi
defn init .
defn ingest-upstream . --module github.com/go-chi/chi/v5 --version v5.1.0
cd -

# Run
go run ./bench/delta-prior/measure \
  --db /path/to/chi \
  --module github.com/go-chi/chi/v5 --version v5.1.0 \
  --targets ./bench/delta-prior/targets-chi.txt \
  --out ./bench/delta-prior/YYYY-MM-DD-chi.csv
```

## Notes

- Uses the corpus seeded from the same source tree, so every read hits
  the compact form. This is the maximum-D scenario — a lower bound on
  files/D savings if D were rolled out to a real project.
- The measurement does **not** invoke `defn serve`; it reproduces the
  rendering templates from `internal/mcp/server.go` in-process. If
  those templates change, `renderFullBytes` / `renderCompactBytes` /
  `renderTagOnlyBytes` in `measure/main.go` must be updated.
