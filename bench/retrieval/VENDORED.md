# Vendored from blackwell-systems/knowing

Source: https://github.com/blackwell-systems/knowing
Commit: 21d7c25026e20d276fc0637469cedce52a521de5
License: MIT

Vendored verbatim (with import-path rewrites only):

- `benchtype/types.go` — adapter interface + result types
- `metrics/metrics.go`, `metrics/stats.go` — P@K, R@K, NDCG@10, MRR, F1, bootstrap CI
- `normalize/normalize.go` — symbol canonicalization across systems
- `corpus/tasks/caddy/**` — caddy retrieval tasks (60 across easy/medium/hard)
- `corpus/tasks/cross-cutting/**` — cross-package tasks
- `adapters/grep.go` — ripgrep baseline (paired comparison)

Written by us (not vendored):

- `adapters/defn.go` — defn adapter (CLI-shelled `defn query`)
- `adapters/registry.go` — slim registry, just defn + grep
- `harness_test.go` — slim harness with train/test split (SHA1 % 10 < 7 → train)

## What we deliberately did NOT vendor

- Their `adapters/knowing.go` and competitor adapters (codegraph, aider, gitnexus,
  gortex, cgc, codebase-memory). We measure paired delta vs the published
  knowing numbers for the same task IDs, not by re-running their adapter.
- Their `harness_test.go` — depends on `internal/context` for ranker knobs.
  Our harness is structurally simpler.
- Their `sweep_test.go`, `robustness_test.go`, `determinism_test.go`, etc. —
  belong to their tuning loop, not ours.

## To refresh

When their corpus or normalize logic moves, bump the pinned commit above and
re-copy. Keep `adapters/grep.go` byte-for-byte (we want their baseline,
unmodified) other than import-path rewrites.
