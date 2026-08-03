package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/metrics"
	"github.com/justinstimatze/defn/internal/store"
	"github.com/justinstimatze/defn/internal/summary"
	"gopkg.in/yaml.v2"
)

// TestSummaryModelQualityComparison answers the question raised after
// #186 shipped code(op:"plan"): does upgrading the async per-def
// summary backend from Haiku (DEFN_SUMMARY_MODEL's current default,
// set in fe0f092) to Sonnet measurably improve the candidate-ranking
// quality that code(op:"context") and code(op:"plan") both depend on
// (gatherContextCandidates's summaryHits*6 signal) -- enough to
// justify the ~3x background cost, given the user's framing: async
// LLM cache-warming is fine to lean on, but only changes justified by
// an actual measured effect, not by "we can afford it."
//
// This deliberately does NOT reuse bench/retrieval's existing
// DefnRanked adapter -- that adapter shells out to `defn query` and
// scores via internal/rank.Rank, a completely different ranker that
// never reads def_summaries at all (confirmed by reading its
// queryWithBody SQL). To measure the thing #197 actually built, this
// calls gatherContextCandidates in-process against a scratch copy of
// a real corpus repo's defn.db (bench/retrieval/corpus/repos), after
// populating def_summaries with each backend in turn -- the same real
// ground-truth tasks (bench/retrieval/corpus/tasks) and the same
// statistical comparison (metrics.CompareSystems: Wilcoxon signed-rank
// + Cohen's d + bootstrap CI) #228 used for the trajectory-format
// decision.
//
// Requires ANTHROPIC_API_KEY -- skips with a clear message otherwise,
// since there is no way to compare two real backends without calling
// them. The corpus DB is never mutated in place: each arm runs
// against its own fresh copy (see scratchSummarizedDB).
func TestSummaryModelQualityComparison(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set -- cannot compare two real summary backends without calling them. Set the key and re-run: go test ./internal/mcp/... -run TestSummaryModelQualityComparison -v -timeout 30m")
	}

	reposRoot := "../../bench/retrieval/corpus/repos"
	tasksRoot := "../../bench/retrieval/corpus/tasks"
	tasks := loadSummaryBenchTasks(t, reposRoot, tasksRoot)
	if len(tasks) == 0 {
		t.Skip("no corpus tasks matched a vendored repo with a defn.db -- nothing to measure")
	}

	reposSeen := map[string]bool{}
	for _, task := range tasks {
		reposSeen[task.Repo] = true
	}
	repoList := make([]string, 0, len(reposSeen))
	for r := range reposSeen {
		repoList = append(repoList, r)
	}
	t.Logf("repos in this pass: %v (%d tasks)", repoList, len(tasks))

	type arm struct {
		name  string
		model anthropic.Model
	}
	arms := []arm{
		{"haiku", summary.DefaultHaikuModel},
		{"sonnet", summary.DefaultExplainModel},
	}

	type dbKey struct{ repo, arm string }
	dbCache := map[dbKey]store.Backend{}
	defer func() {
		for _, b := range dbCache {
			b.Close()
		}
	}()
	for _, a := range arms {
		for repo := range reposSeen {
			backend, err := scratchSummarizedDB(t, reposRoot, repo, a.name, a.model, apiKey)
			if err != nil {
				t.Fatalf("%s/%s: %v", repo, a.name, err)
			}
			if defs, err := backend.FindDefinitions("%"); err == nil && len(defs) == 0 {
				t.Fatalf("corpus repo %q's checked-in defn.db has 0 definitions -- stale fixture, needs a real ingest before this comparison means anything. Not a harness bug; see bench/retrieval/corpus/repos/%s", repo, repo)
			}
			dbCache[dbKey{repo, a.name}] = backend
		}
	}

	var allMetrics []benchtype.MetricResult
	for _, a := range arms {
		for _, task := range tasks {
			backend := dbCache[dbKey{task.Repo, a.name}]
			s := &server{backend: backend}
			result := benchtype.RetrievalResult{System: a.name, TaskID: task.ID}
			scored, _, err := s.gatherContextCandidates(task.Description)
			if err != nil {
				t.Logf("%s/%s: gatherContextCandidates: %v (scored as a miss)", a.name, task.ID, err)
			} else {
				const topN = 20
				if len(scored) > topN {
					scored = scored[:topN]
				}
				symbols := make([]benchtype.RetrievedSymbol, len(scored))
				for i, c := range scored {
					qn := c.Def.Name
					if c.Def.Receiver != "" {
						qn = c.Def.Receiver + "." + c.Def.Name
					}
					symbols[i] = benchtype.RetrievedSymbol{QualifiedName: qn, Rank: i + 1}
				}
				result.Symbols = symbols
			}
			allMetrics = append(allMetrics, metrics.Compute(result, task.GroundTruth))
		}
	}

	for _, m := range []string{"precision_at_10", "recall_at_10", "ndcg_at_10", "mrr"} {
		cmp := metrics.CompareSystems(allMetrics, "sonnet", "haiku", m)
		t.Logf("%s: sonnet=%.3f haiku=%.3f diff=%+.3f wilcoxon-p=%.4f cohens-d=%.3f CI95=[%.3f,%.3f] n=%d significant=%v",
			m, cmp.MeanA, cmp.MeanB, cmp.Difference, cmp.WilcoxonP, cmp.CohensD, cmp.CI95Lower, cmp.CI95Upper, cmp.TaskCount, cmp.Significant)
	}
}

// loadSummaryBenchTasks walks tasksRoot and returns every task whose
// repo has a matching reposRoot/<repo>/.defn/defn.db -- only repos this
// checkout actually has vendored, so a partial corpus checkout still
// runs a partial (logged, not silently truncated) comparison instead
// of failing outright.
func loadSummaryBenchTasks(t *testing.T, reposRoot, tasksRoot string) []summaryBenchTask {
	t.Helper()
	var out []summaryBenchTask
	err := filepath.WalkDir(tasksRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var task summaryBenchTask
		if err := yaml.Unmarshal(raw, &task); err != nil {
			return err
		}
		if len(task.GroundTruth) == 0 {
			return nil
		}
		dbPath := filepath.Join(reposRoot, task.Repo, ".defn", "defn.db")
		if _, err := os.Stat(dbPath); err != nil {
			return nil // repo not vendored in this checkout -- skip, don't fail
		}
		out = append(out, task)
		return nil
	})
	if err != nil {
		t.Fatalf("walk tasks: %v", err)
	}
	return out
}

// scratchSummarizedDB copies repo's checked-in defn.db into a fresh
// t.TempDir() -- #146's rule applies here too: never mutate a shared
// corpus DB in place -- opens the copy, and populates EVERY
// definition's summary via the given model, synchronously. Bypasses
// the async summary.Worker on purpose: a benchmark needs a
// deterministic "fully populated" state before scoring, not a
// fire-and-forget queue that might still be draining mid-measurement.
//
// summary.NewHaiku is misleadingly named for the "sonnet" arm -- it's
// actually a generic single-model Anthropic backend (Model is a plain
// override field); reusing it here avoids adding a parallel
// NewSonnet constructor for what would be identical code.
//
// DEFN_BENCH_SUMMARY_MAX_DEFS caps how many defs get summarized per
// (repo, arm) when set, for a bounded-cost pilot run; unset means no
// cap -- the full corpus, logged either way so scope is never silent.
func scratchSummarizedDB(t *testing.T, reposRoot, repo, armName string, model anthropic.Model, apiKey string) (store.Backend, error) {
	t.Helper()
	srcDB := filepath.Join(reposRoot, repo, ".defn", "defn.db")
	src, err := os.ReadFile(srcDB)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", srcDB, err)
	}
	scratchDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratchDir, "defn.db"), src, 0644); err != nil {
		return nil, fmt.Errorf("write scratch db: %w", err)
	}
	backend, err := store.OpenBackend(scratchDir)
	if err != nil {
		return nil, fmt.Errorf("open scratch db: %w", err)
	}

	ids, err := backend.ListDefsMissingSummary()
	if err != nil {
		return nil, fmt.Errorf("list defs: %w", err)
	}
	if max := os.Getenv("DEFN_BENCH_SUMMARY_MAX_DEFS"); max != "" {
		if n, err := strconv.Atoi(max); err == nil && n < len(ids) {
			t.Logf("%s/%s: capping %d defs to %d via DEFN_BENCH_SUMMARY_MAX_DEFS", repo, armName, len(ids), n)
			ids = ids[:n]
		}
	}
	if len(ids) == 0 {
		t.Logf("%s/%s: no defs missing a summary -- corpus DB may already be summarized", repo, armName)
		return backend, nil
	}

	sumBackend := summary.NewHaiku(summary.HaikuOptions{APIKey: apiKey, Model: model, Parallelism: 8})
	var reqs []summary.Request
	for _, id := range ids {
		d, err := backend.GetDefinition(id)
		if err != nil || d == nil {
			continue
		}
		reqs = append(reqs, summary.Request{
			DefID: d.ID, Name: d.Name, Kind: d.Kind, Receiver: d.Receiver,
			Body: d.Body, BodyHash: store.HashBodyStructural(d.Body),
		})
	}
	t.Logf("%s/%s: generating %d summaries (model=%s)...", repo, armName, len(reqs), model)
	results := sumBackend.Generate(context.Background(), reqs)
	persisted, failed := 0, 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			continue
		}
		if err := backend.SetDefSummary(r.DefID, &store.DefSummary{OneLine: r.OneLine, BodyHash: r.BodyHash, Model: r.Model}); err != nil {
			failed++
			continue
		}
		persisted++
	}
	t.Logf("%s/%s: persisted %d, failed %d", repo, armName, persisted, failed)
	return backend, nil
}

// summaryBenchTask is the subset of bench/retrieval/corpus/tasks'
// YAML shape this bench needs -- see internal/planformat's
// corpus_test.go for the sibling reader over the same corpus (#228
// used it to compare trajectory formats; this reuses it to compare
// summary-backend quality).
type summaryBenchTask struct {
	ID          string   `yaml:"id"`
	Repo        string   `yaml:"repo"`
	Description string   `yaml:"description"`
	GroundTruth []string `yaml:"ground_truth"`
}

// TestSummaryModelQualityComparison_HarnessSmokeTest exercises the
// entire pipeline (corpus load, scratch-copy, summary population,
// gatherContextCandidates, metrics.Compute) with no ANTHROPIC_API_KEY
// -- summary.NewHaiku degrades to Stub{} on an empty key, so both
// "arms" get identical "TODO: <Name>" placeholders and can't show a
// real quality difference. That's expected and fine: this test's job
// is only to prove the harness itself runs end-to-end without a key,
// so a reviewer doesn't have to trust an untested 150-line bench file
// on faith before deciding whether to spend real API budget on
// TestSummaryModelQualityComparison.
func TestSummaryModelQualityComparison_HarnessSmokeTest(t *testing.T) {
	reposRoot := "../../bench/retrieval/corpus/repos"
	tasksRoot := "../../bench/retrieval/corpus/tasks"
	tasks := loadSummaryBenchTasks(t, reposRoot, tasksRoot)
	if len(tasks) == 0 {
		t.Skip("no corpus tasks matched a vendored repo with a defn.db")
	}
	task := tasks[0]

	backend, err := scratchSummarizedDB(t, reposRoot, task.Repo, "stub", summary.DefaultHaikuModel, "")
	if err != nil {
		t.Fatalf("scratchSummarizedDB: %v", err)
	}
	defer backend.Close()

	if defs, err := backend.FindDefinitions("%"); err == nil && len(defs) == 0 {
		t.Skipf("corpus repo %q's checked-in defn.db has 0 definitions -- stale fixture (never ingested, or ingested pre-migration and reset to an empty schema), not a harness bug. Needs a real ingest before this comparison can run against it.", task.Repo)
	}

	s := &server{backend: backend}
	scored, _, err := s.gatherContextCandidates(task.Description)
	if err != nil {
		t.Fatalf("gatherContextCandidates: %v", err)
	}
	if len(scored) == 0 {
		t.Fatalf("expected at least one candidate for task %s (%q)", task.ID, task.Description)
	}

	symbols := make([]benchtype.RetrievedSymbol, 0, len(scored))
	for i, c := range scored {
		qn := c.Def.Name
		if c.Def.Receiver != "" {
			qn = c.Def.Receiver + "." + c.Def.Name
		}
		symbols = append(symbols, benchtype.RetrievedSymbol{QualifiedName: qn, Rank: i + 1})
	}
	m := metrics.Compute(benchtype.RetrievalResult{System: "stub", TaskID: task.ID, Symbols: symbols}, task.GroundTruth)
	t.Logf("smoke test on %s/%s: precision@10=%.3f recall@10=%.3f (candidates=%d, ground_truth=%d)",
		task.Repo, task.ID, m.PrecisionAt10, m.RecallAt10, len(scored), len(task.GroundTruth))
}
