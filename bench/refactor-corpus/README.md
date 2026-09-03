# refactor-corpus (gap-analysis 2026-09-02, work-order item 7)

10 real upstream commits, hand-picked via `gh search commits` across the
same 5 repos already used elsewhere in this project's benches
(prometheus, etcd, go-zero, cli, grpc-go), chosen because none of the
existing bug-fix corpora exercise defn's actual structural asymmetries
(cross-package rename/move, def-scoped test, guaranteed-consistent ref
graph) — see `docs/gap-analysis-2026-09-02.md` §4.

Each task's `base_commit_sha` is the real commit's own parent; the agent
is asked (via `problem_statement`, a paraphrase of the real commit's
rationale, not a copy of its message) to make the same change from a
clean pre-refactor checkout. `fix_patch` is the real commit's full diff,
used by `score_gitdiff.py`'s `gold_files()` for file-level
precision/recall — same scoring mechanism as every other corpus here.

## Shape breakdown

- **rename-exported-cross-package** (2): prometheus `min`/`max` duration
  functions → `least`/`greatest` (11 files, incl. the generated PromQL
  parser — a deliberately-kept risk case, see caveat below); etcd
  `ApplyWait` rename (2 files, small/clean).
- **move-def-between-packages** (3): etcd's `Event` helpers → `pb`
  package (4 files); go-zero's JSON header vars → an `internal` package
  (14 files); go-zero's auth interceptor → `serverinterceptors` (2
  files, intentionally tiny for difficulty diversity).
- **extract-helper-used-in-k-files** (3): etcd's `EtcdState` field
  accessors (5 files, genuinely cross-file); cli's token-type-constant
  consolidation (2 files); grpc-go's `loopyWriter` shared-state helper
  (1 file — this one's K=1, the model dedupes two paths *within* the
  same file, not across files; kept anyway for grpc-go repo
  representation, noted honestly rather than mislabeled).
- **signature-change-n-callers** (2): cli's `GetComment`
  (`ghrepo.Interface` → host string, 8 files/callers); prometheus's
  `recode` (return `*HistogramAppender` directly, 1 file/2 callers —
  smallest example in the set).

## Known risk deliberately kept in

The prometheus rename task touches
`promql/parser/generated_parser.y.go` — the exact generated-parser file
that caused a "cannot safety-check ... generated content doesn't parse"
emit failure in a real prom-19184 trajectory (see
`docs/lessons-learned.md`'s tail-event-detector write-up). Left in
rather than swapped for an easier example: it's a genuine, realistic
case of defn's actual weak spot, and the pilot's whole point is to find
out whether the corpus (and defn) can handle it, not to dodge it.

## Running

```
python3 bench/head-to-head-go/agent_driver.py --corpus-dir bench/refactor-corpus ...
```
See `bench/head-to-head-go/agent_driver.py --help` for arm/concurrency/
cost-cap flags — same driver as every other corpus, just pointed at a
different `tasks.jsonl`. Score with:
`python3 bench/prometheus-repo-opus/score_gitdiff.py <repo_dir> bench/refactor-corpus/tasks.jsonl <arm_defn_dir> <arm_files_dir>`
(same `gold_files()`-based file-level precision/recall as every other
corpus — see that script for the exact invocation shape).

Per [[Bench artifacts not committed]]: this task list (and this readme)
are committed as reference/reusable material, same bar as
`bench/prometheus-repo`'s original 15-task set. Trajectory JSON output
and correctness_scores.json from actually running the pilot are NOT
committed unless a later authoritative run is meant to back published
numbers.
