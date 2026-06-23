// Command defn-bench-tune grid-searches rank.Weights against the
// retrieval benchmark's train split and prints the best NDCG@10.
//
// Usage:
//
//	cd bench/retrieval && go run ../../cmd/defn-bench-tune -repo caddy
//
// The candidate set per task is fetched once (via `defn query`), so the
// grid loop is pure in-process re-ranking. A 5^5 = 3125 sweep finishes
// in a few seconds.
//
// Output is human-readable plus a final line of the form
//
//	BEST WEIGHTS name=1.0 caller=2.0 test=0.5 body=1.0 receiver=0.25 ndcg=0.181
//
// suitable for grepping in a Makefile target.
package main

import (
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/metrics"
	"github.com/justinstimatze/defn/bench/retrieval/normalize"
	"github.com/justinstimatze/defn/internal/rank"
	"github.com/justinstimatze/defn/internal/store"
)

var (
	flagRepo   = flag.String("repo", "caddy", "repo name under corpus/repos/ and corpus/tasks/")
	flagCorpus = flag.String("corpus", "corpus", "path to corpus root (relative to cwd)")
	flagBin    = flag.String("defn", "defn", "defn binary on PATH or absolute path")
	flagLevels = flag.String("levels", "0.25,0.5,1,2,4", "comma-separated weight levels for the grid")
)

func main() {
	flag.Parse()
	repo := *flagRepo
	repoPath := filepath.Join(*flagCorpus, "repos", repo)
	tasksDir := filepath.Join(*flagCorpus, "tasks", repo)

	tasks, err := loadTasks(tasksDir)
	must(err)
	train := filterTrain(tasks)
	fmt.Fprintf(os.Stderr, "loaded %d tasks (%d train) for %s\n", len(tasks), len(train), repo)
	if len(train) == 0 {
		fatal("no train tasks")
	}

	idf, err := primeIDF(*flagBin, repoPath)
	must(err)
	fmt.Fprintln(os.Stderr, "primed IDF")

	cache := make(map[string][]rank.Candidate, len(train))
	for _, t := range train {
		cands, err := fetchCandidates(*flagBin, repoPath, t.Description)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  fetch %s: %v\n", t.ID, err)
			continue
		}
		cache[t.ID] = cands
	}
	fmt.Fprintf(os.Stderr, "cached candidates for %d tasks\n", len(cache))

	levels, err := parseLevels(*flagLevels)
	must(err)
	fmt.Fprintf(os.Stderr, "grid: %d^4 = %d combos (NameMatch anchored at 1.0)\n", len(levels), pow4(len(levels)))

	all := make([]tuneResult, 0, 1296)
	count := 0
	// NameMatch is anchored at 1.0 — the other weights are tuned
	// relative to it. Linear scoring is scale-invariant for ordering,
	// so without an anchor "all 0.25" ties "all 1.0" and the grid
	// degenerates. Anchoring fixes the feature-importance question to
	// "how much does each other feature contribute relative to name
	// match?"
	const nameWeight = 1.0
	for _, c := range levels {
		for _, te := range levels {
			for _, b := range levels {
				for _, rcv := range levels {
					{
						w := rank.Weights{
							NameMatch:     nameWeight,
							CallerCount:   c,
							TestCount:     te,
							BodyOverlap:   b,
							ReceiverMatch: rcv,
						}
						ndcg, p10, mrr := evaluate(train, cache, idf, w)
						count++
						all = append(all, tuneResult{w: w, ndcg: ndcg, p10: p10, mrr: mrr})
					}
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "evaluated %d combos\n", count)

	// Sort descending by NDCG@10.
	sortDesc(all)

	// Baseline — DefaultWeights (1.0 across all features).
	baseW := rank.Weights{NameMatch: 1, CallerCount: 1, TestCount: 1, BodyOverlap: 1, ReceiverMatch: 1}
	bNDCG, bP10, bMRR := evaluate(train, cache, idf, baseW)
	fmt.Printf("BASELINE (all=1.0) train n=%d: ndcg=%.4f p@10=%.4f mrr=%.4f\n", len(train), bNDCG, bP10, bMRR)

	fmt.Printf("TOP 5 tuned (train n=%d):\n", len(train))
	for i := 0; i < 5 && i < len(all); i++ {
		r := all[i]
		fmt.Printf("  #%d ndcg=%.4f p@10=%.4f mrr=%.4f  name=%g caller=%g test=%g body=%g receiver=%g\n",
			i+1, r.ndcg, r.p10, r.mrr,
			r.w.NameMatch, r.w.CallerCount, r.w.TestCount, r.w.BodyOverlap, r.w.ReceiverMatch)
	}
	best := all[0]
	delta := best.ndcg - bNDCG
	fmt.Printf("BEST WEIGHTS name=%g caller=%g test=%g body=%g receiver=%g ndcg=%.4f p@10=%.4f mrr=%.4f (Δndcg vs baseline = %+.4f)\n",
		best.w.NameMatch, best.w.CallerCount, best.w.TestCount, best.w.BodyOverlap, best.w.ReceiverMatch,
		best.ndcg, best.p10, best.mrr, delta)
}

type tuneResult struct {
	w    rank.Weights
	ndcg float64
	p10  float64
	mrr  float64
}

func sortDesc(rs []tuneResult) {
	// Insertion-friendly stable sort. Small enough to be fine.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].ndcg > rs[j-1].ndcg; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func evaluate(tasks []benchtype.Task, cache map[string][]rank.Candidate, idf rank.IDF, w rank.Weights) (ndcg, p10, mrr float64) {
	var sumN, sumP, sumM float64
	var n int
	for _, t := range tasks {
		cands, ok := cache[t.ID]
		if !ok {
			continue
		}
		scored := rank.Rank(t.Description, cands, idf, w)
		limit := 20
		if len(scored) < limit {
			limit = len(scored)
		}
		symbols := make([]benchtype.RetrievedSymbol, 0, limit)
		for i := 0; i < limit; i++ {
			d := scored[i].Def
			qn := d.Name
			if d.Receiver != "" {
				qn = d.Receiver + "." + d.Name
			}
			symbols = append(symbols, benchtype.RetrievedSymbol{
				QualifiedName: qn,
				Normalized:    normalize.Symbol(qn),
				Rank:          i + 1,
				FilePath:      d.SourceFile,
				Kind:          d.Kind,
			})
		}
		m := metrics.Compute(benchtype.RetrievalResult{
			System:  "tune",
			TaskID:  t.ID,
			Symbols: symbols,
		}, t.GroundTruth)
		sumN += m.NDCGAt10
		sumP += m.PrecisionAt10
		sumM += m.MRR
		n++
	}
	if n == 0 {
		return 0, 0, 0
	}
	return sumN / float64(n), sumP / float64(n), sumM / float64(n)
}

func loadTasks(root string) ([]benchtype.Task, error) {
	var out []benchtype.Task
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var task benchtype.Task
		if err := yaml.Unmarshal(data, &task); err != nil {
			return nil
		}
		if task.ID != "" {
			out = append(out, task)
		}
		return nil
	})
	return out, err
}

func filterTrain(tasks []benchtype.Task) []benchtype.Task {
	out := make([]benchtype.Task, 0, len(tasks))
	for _, t := range tasks {
		h := sha1.Sum([]byte(t.ID))
		if int(h[0])%10 < 7 {
			out = append(out, t)
		}
	}
	return out
}

// fetchCandidates mirrors what bench/retrieval/adapters/defn_ranked.go
// does but inline: extract keywords, query each, dedupe by qualified name.
func fetchCandidates(bin, repoPath, description string) ([]rank.Candidate, error) {
	keywords := extractKeywords(description)
	seen := make(map[string]rank.Candidate)
	for _, kw := range keywords {
		kw = strings.ReplaceAll(kw, "'", "")
		sql := fmt.Sprintf(
			"SELECT d.name, IFNULL(d.receiver,'') AS receiver, d.`kind` AS kind, "+
				"IFNULL(d.source_file,'') AS source_file, IFNULL(b.body,'') AS body, "+
				"(SELECT COUNT(*) FROM refs r JOIN definitions c ON c.id = r.from_def "+
				"WHERE c.test = FALSE AND r.to_def = d.id) AS caller_count, "+
				"(SELECT COUNT(*) FROM refs r JOIN definitions c ON c.id = r.from_def "+
				"WHERE c.test = TRUE AND r.to_def = d.id) AS test_count "+
				"FROM definitions d LEFT JOIN bodies b ON b.def_id = d.id "+
				"WHERE d.test = FALSE AND d.name LIKE '%%%s%%' LIMIT 200",
			kw,
		)
		cmd := exec.Command(bin, "query", sql)
		cmd.Dir = repoPath
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		maps, err := parseRows(out)
		if err != nil {
			continue
		}
		for _, m := range maps {
			d := store.Definition{
				Name:       asStr(m["name"]),
				Receiver:   asStr(m["receiver"]),
				Kind:       asStr(m["kind"]),
				SourceFile: asStr(m["source_file"]),
				Body:       asStr(m["body"]),
			}
			key := d.Receiver + "." + d.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = rank.Candidate{
				Def:         d,
				CallerCount: asInt(m["caller_count"]),
				TestCount:   asInt(m["test_count"]),
			}
		}
	}
	out := make([]rank.Candidate, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	return out, nil
}

func primeIDF(bin, repoPath string) (rank.IDF, error) {
	sql := "SELECT IFNULL(b.body,'') AS body FROM definitions d " +
		"JOIN bodies b ON b.def_id = d.id WHERE d.test = FALSE " +
		"ORDER BY d.hash LIMIT 5000"
	cmd := exec.Command(bin, "query", sql)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return rank.PlaceholderIDF, err
	}
	maps, err := parseRows(out)
	if err != nil {
		return rank.PlaceholderIDF, err
	}
	df := make(map[string]int)
	for _, m := range maps {
		body := asStr(m["body"])
		seen := make(map[string]bool)
		for _, tok := range tokenize(body) {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}
	n := len(maps)
	scores := make(map[string]float64, len(df))
	var maxIDF float64
	for tok, count := range df {
		s := math.Log(float64(n+1) / float64(count+1))
		scores[tok] = s
		if s > maxIDF {
			maxIDF = s
		}
	}
	return tuneIDF{scores: scores, max: maxIDF}, nil
}

type tuneIDF struct {
	scores map[string]float64
	max    float64
}

func (i tuneIDF) Score(token string) float64 {
	if v, ok := i.scores[token]; ok {
		return v
	}
	return i.max
}

func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// extractKeywords mirrors the bench adapter's keyword extraction. Copied
// rather than imported to keep cmd/defn-bench-tune independent of the
// adapters package.
func extractKeywords(description string) []string {
	stop := map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
		"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
		"with": true, "by": true, "from": true, "as": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
		"how": true, "what": true, "when": true, "where": true, "why": true, "which": true,
		"who": true, "whom": true, "this": true, "that": true, "these": true, "those": true,
		"i": true, "you": true, "he": true, "she": true, "it": true, "we": true, "they": true,
		"my": true, "your": true, "his": true, "her": true, "its": true, "our": true,
		"add": true, "use": true, "get": true, "set": true, "make": true, "find": true,
	}
	var out []string
	for _, tok := range tokenize(description) {
		if len(tok) < 3 || stop[tok] {
			continue
		}
		out = append(out, tok)
	}
	return out
}

func parseRows(out []byte) ([]map[string]any, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var raw []map[string]any
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	if trimmed[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return nil, err
		}
		if rows, ok := obj["rows"].([]any); ok {
			ms := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					ms = append(ms, m)
				}
			}
			return ms, nil
		}
		return []map[string]any{obj}, nil
	}
	var maps []map[string]any
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			maps = append(maps, m)
		}
	}
	return maps, nil
}

func asStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func asInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		var n int
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func parseLevels(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var f float64
		if _, err := fmt.Sscanf(p, "%f", &f); err != nil {
			return nil, fmt.Errorf("bad level %q: %w", p, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no levels in %q", s)
	}
	return out, nil
}

func pow4(n int) int { return n * n * n * n }

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(v any) {
	fmt.Fprintln(os.Stderr, v)
	os.Exit(1)
}
