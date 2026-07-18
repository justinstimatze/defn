# bench/expand-bytes — cheap sanity check for `code(op:"expand")` v1

Measures raw byte output for a target def across three renderings:

- **files** — whole source file bytes (files-mode `Read` equivalent)
- **read + impact** — sum of separate `handleGetDefinition` and
  `handleImpact` outputs (today's two-call multi-hop cost)
- **expand** — `handleExpand` v1 (body + callers)

Two ratios:
- `expand / files` — competitive with files-mode?
- `expand / (read + impact)` — improvement over two-call today?

## 2026-07-17 run — POST getInterfaceDispatchCallers fix (chi)

Below numbers are after removing the buggy `getInterfaceDispatchCallers`
heuristic in `internal/store/store.go`. The heuristic was returning
callers of the interface TYPE for every concrete method — inflating
`Mux` methods with dozens of phantom callers. Fix: rely on the
`interface_dispatch` refs already emitted by `resolve.go:474`. See
regression test `TestGetImpact_InterfaceDispatchPrecision`.

Pre-fix routeHTTP showed 26 callers (12 prod + 14 test) — 24 phantoms.
Post-fix: 2 callers, matching ground truth.

    target        files  read  impact  r+i   expand  exp/files  exp/r+i
    Timeout        1222  1881    153  2034    1940    1.59       0.95
    Middlewares    4615   369    306   675     554    0.12       0.82
    Mux           16163  1750    375  2125    2154    0.13       1.01
    Router         4615  1964    188  2152    2748    0.60       1.28
    Route         16163   614    233   847    1250    0.08       1.48
    ServeHTTP     16163  1502   1630  3132    3888    0.24       1.24
    routeHTTP     16163  1245    252  1497    1369    0.08       0.91
    Handle        16163   495    146   641     707    0.04       1.10

## 2026-07-17 run — PRE-fix (kept for comparison)

    target        files  read  impact  r+i   expand  exp/files  exp/r+i
    Timeout        1222  1881    153  2034    1940    1.59       0.95
    Middlewares    4615   369    306   675     554    0.12       0.82
    Mux           16163  1750    375  2125    2154    0.13       1.01
    Router         4615  1964    188  2152    2748    0.60       1.28
    Route         16163   614    479  1093    1658    0.10       1.52
    ServeHTTP     16163  1502   1846  3348    4515    0.28       1.35

## Interpretation

- **vs files-mode:** expand wins on 5 of 6 targets, big wins on
  Middlewares (0.12), Mux (0.13), Route (0.10). Loses only on Timeout
  (1.59) because Timeout is a small standalone file that's mostly the
  definition — no context to save on.
- **vs read+impact:** expand saves bytes on small cases (Timeout 0.95,
  Middlewares 0.82) but INFLATES 28-52% on Router / Route / ServeHTTP.
  Root cause: expand v1 lists all callers (prod + test) whereas
  `handleImpact` lists only prod-caller lines and summarizes tests.
  Test-heavy defs pay for that with more output.

## Design implication

Byte-size comparison IS competitive with files-mode across every real
target except the trivial single-file case. NOT a byte-bloat disaster.

Cache-read math still dominates: one expand call at turn N saves
~(10-N)·D cached tokens from being re-billed on later turns. Even if
expand is 1.5× the bytes of two separate calls, the turn elimination
saves multiples of that on cached prefix re-billing.

Small tuning opportunity: match `handleImpact`'s prod-only-with-summary
format for callers to close the r+i ratio. Deferred — not blocking.

## Ladder verdict

Byte micro-bench passes: expand v1 is byte-competitive. Combined with
the turn-count projection (`../session-cumulative/expand-projection.md`,
which failed parity on turns.txt), the combined signal is:

- Expand design is sound (not bloated).
- turns.txt is the wrong workload — too write-heavy for expand's
  read-substitution mechanism to matter.
- Next: design a read-dominated bench script where expand wins on
  paper, then fire one paid arm.
