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

| Corpus | files | full | compact (shipped) | tag-only |
|---|---:|---:|---:|---:|
| chi (20 defs) | 27,095 | 4,080 | 10,430 (**−156%** vs full) | 1,176 (**+71%** vs full) |
| gin (19 defs) | 72,089 | 5,557 | 13,130 (**−136%** vs full) | 1,210 (**+78%** vs full) |

Percentages are byte reductions vs `full` (defn-natural). Negative means
D is *bigger* than the current body-in-fence rendering.

### Interpretation

**Two conclusions land at once:**

1. **defn-natural already dominates files-mode by ~85-92%** on
   library-symbol reads — the "narrow body vs whole file" story is
   real without any D involvement. This confirms the well-worn
   read-side thesis on a fresh corpus.

2. **D as currently implemented does NOT help.** The compact envelope
   (doc + sig + provenance tag) exceeds the body of a typical chi/gin
   method (5-30 LOC), so it *inflates* byte output by ~140%.

3. **A skinnier D form CAN help.** Dropping the doc and sig leaves a
   ~60-byte tag line, which beats defn-natural by ~72-78% on the same
   corpus. This form gives the model just the pointer ("this is
   Name @ version, unchanged from upstream — look it up if you need
   more"), delegating body knowledge to the model's prior.

### Design implication

Ship a rework of `renderUpstreamMatch` that emits only the tag line by
default, not doc + sig. The doc and sig are freely available via
`full: true` (which already returns the body anyway).

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
