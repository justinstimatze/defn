// Package retrieval_test is the cross-system retrieval benchmark harness.
//
// Tasks live under corpus/tasks/<repo>/<tier>/*.yaml. Repos are expected at
// corpus/repos/<repo>/ (cloned via scripts/clone-repos.sh). Each adapter is
// invoked per-task; metrics are computed against ground_truth.
//
// Usage:
//
//	go test ./bench/retrieval/ -run TestRetrieval -v -timeout 30m
//	BENCH_REPOS=caddy go test ./bench/retrieval/ -run TestRetrieval -v
//	BENCH_ADAPTERS=defn go test ./bench/retrieval/ -run TestRetrieval -v
//
// Train/test split: SHA1(task-id) % 10 < 7 → train. The harness logs both
// but only the test partition counts toward externally cited numbers.
package retrieval_test

import (
	"crypto/sha1"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/justinstimatze/defn/bench/retrieval/adapters"
	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/metrics"
)

const defaultTokenBudget = 5000

func TestRetrieval(t *testing.T) {
	if testing.Short() {
		t.Skip("retrieval benchmark requires cloned corpus")
	}

	tasks := loadTasks(t, "corpus/tasks")
	if len(tasks) == 0 {
		t.Fatal("no task fixtures found in corpus/tasks/")
	}

	repoFilter := envSet("BENCH_REPOS")
	adapterFilter := envSet("BENCH_ADAPTERS")

	available := adapters.Available()
	t.Logf("Available adapters: %v", names(available))

	type rowKey struct{ adapter, split, tier string }
	bucket := make(map[rowKey][]benchtype.MetricResult)

	for _, ad := range available {
		if len(adapterFilter) > 0 && !adapterFilter[ad.Name()] {
			continue
		}
		repos := groupByRepo(tasks, repoFilter)
		for repo := range repos {
			repoPath := filepath.Join("corpus", "repos", repo)
			if _, err := os.Stat(repoPath); err != nil {
				t.Logf("  [%s] repo %s not cloned at %s — skipping", ad.Name(), repo, repoPath)
				delete(repos, repo)
				continue
			}
			if dt, err := ad.Index(repoPath); err != nil {
				t.Logf("  [%s] %s: index error: %v", ad.Name(), repo, err)
				delete(repos, repo)
			} else {
				t.Logf("  [%s] indexed %s in %dms", ad.Name(), repo, dt)
			}
		}
		for repo, rtasks := range repos {
			repoPath := filepath.Join("corpus", "repos", repo)
			for _, task := range rtasks {
				res, err := ad.Retrieve(repoPath, task, defaultTokenBudget)
				if err != nil {
					t.Logf("  [%s] %s: %v", ad.Name(), task.ID, err)
					continue
				}
				m := metrics.Compute(res, task.GroundTruth)
				split := splitFor(task.ID)
				bucket[rowKey{ad.Name(), split, task.Tier}] = append(bucket[rowKey{ad.Name(), split, task.Tier}], m)
				t.Logf("  [%s][%s][%s] %s P@10=%.2f R@10=%.2f NDCG@10=%.2f MRR=%.2f n=%d gt=%d",
					ad.Name(), split, task.Tier, task.ID,
					m.PrecisionAt10, m.RecallAt10, m.NDCGAt10, m.MRR,
					len(res.Symbols), len(task.GroundTruth))
			}
		}
	}

	// Aggregate. Print one row per (adapter, split, tier) and an ALL row.
	keys := make([]rowKey, 0, len(bucket))
	for k := range bucket {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].adapter != keys[j].adapter {
			return keys[i].adapter < keys[j].adapter
		}
		if keys[i].split != keys[j].split {
			return keys[i].split < keys[j].split
		}
		return keys[i].tier < keys[j].tier
	})
	t.Logf("")
	t.Logf("AGGREGATE (mean per partition)")
	t.Logf("%-10s %-6s %-7s %5s %7s %7s %7s %7s", "adapter", "split", "tier", "n", "P@10", "R@10", "NDCG@10", "MRR")
	for _, k := range keys {
		ms := bucket[k]
		t.Logf("%-10s %-6s %-7s %5d %7.3f %7.3f %7.3f %7.3f",
			k.adapter, k.split, k.tier, len(ms),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.PrecisionAt10 }),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.RecallAt10 }),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.NDCGAt10 }),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.MRR }))
	}
	// Cross-tier per adapter+split rollup.
	type as struct{ adapter, split string }
	rollup := make(map[as][]benchtype.MetricResult)
	for k, ms := range bucket {
		rollup[as{k.adapter, k.split}] = append(rollup[as{k.adapter, k.split}], ms...)
	}
	t.Logf("")
	t.Logf("ROLLUP (mean across tiers)")
	t.Logf("%-10s %-6s %5s %7s %7s %7s %7s", "adapter", "split", "n", "P@10", "R@10", "NDCG@10", "MRR")
	asKeys := make([]as, 0, len(rollup))
	for k := range rollup {
		asKeys = append(asKeys, k)
	}
	sort.Slice(asKeys, func(i, j int) bool {
		if asKeys[i].adapter != asKeys[j].adapter {
			return asKeys[i].adapter < asKeys[j].adapter
		}
		return asKeys[i].split < asKeys[j].split
	})
	for _, k := range asKeys {
		ms := rollup[k]
		t.Logf("%-10s %-6s %5d %7.3f %7.3f %7.3f %7.3f",
			k.adapter, k.split, len(ms),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.PrecisionAt10 }),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.RecallAt10 }),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.NDCGAt10 }),
			mean(ms, func(m benchtype.MetricResult) float64 { return m.MRR }))
	}
}

func loadTasks(t *testing.T, root string) []benchtype.Task {
	var tasks []benchtype.Task
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var task benchtype.Task
		if err := yaml.Unmarshal(data, &task); err != nil {
			t.Logf("  yaml parse error %s: %v", path, err)
			return nil
		}
		if task.ID == "" {
			return nil
		}
		tasks = append(tasks, task)
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	return tasks
}

func groupByRepo(tasks []benchtype.Task, filter map[string]bool) map[string][]benchtype.Task {
	out := make(map[string][]benchtype.Task)
	for _, t := range tasks {
		if len(filter) > 0 && !filter[t.Repo] {
			continue
		}
		out[t.Repo] = append(out[t.Repo], t)
	}
	return out
}

func splitFor(taskID string) string {
	h := sha1.Sum([]byte(taskID))
	bucket := int(h[0]) % 10
	if bucket < 7 {
		return "train"
	}
	return "test"
}

func envSet(key string) map[string]bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, x := range strings.Split(v, ",") {
		x = strings.TrimSpace(x)
		if x != "" {
			out[x] = true
		}
	}
	return out
}

func names(as []benchtype.Adapter) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name()
	}
	return out
}

func mean(ms []benchtype.MetricResult, f func(benchtype.MetricResult) float64) float64 {
	if len(ms) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, m := range ms {
		s += f(m)
	}
	return s / float64(len(ms))
}
