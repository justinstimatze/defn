// defn_ranked.go is the bench adapter that scores candidates with the
// internal/rank linear scorer. The plain `defn` adapter does deterministic
// bucket sort (exact > prefix > substring); this one extends the SQL to
// pull body + caller_count, builds a per-repo IDF table from a body
// sample, and runs rank.Rank with the task description as the query.
//
// Lives alongside the unranked adapter so the harness reports both side
// by side. The unranked baseline is the floor we must beat.
package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/normalize"
	"github.com/justinstimatze/defn/internal/rank"
	"github.com/justinstimatze/defn/internal/store"
)

type DefnRanked struct {
	bin string

	mu  sync.Mutex
	idf map[string]*adapterIDF // keyed by repoPath
}

func NewDefnRanked() *DefnRanked {
	bin, err := exec.LookPath("defn")
	if err != nil {
		bin = "defn"
	}
	return &DefnRanked{bin: bin, idf: make(map[string]*adapterIDF)}
}

func (a *DefnRanked) Name() string { return "defn-ranked" }

// benchVerbose reports whether to log adapter-level diagnostics (query
// errors, empty results, ingest details). Set BENCH_VERBOSE=1 in the
// environment to enable. Defaults off so the harness's aggregate
// output stays clean.
func benchVerbose() bool { return os.Getenv("BENCH_VERBOSE") == "1" }

// Index ensures the project is ingested and primes the IDF table for the
// repo. The plain adapter's `defn query SELECT 1` is reused as the "is the
// DB ready" probe.
func (a *DefnRanked) Index(repoPath string) (int64, error) {
	start := time.Now()
	cmd := exec.Command(a.bin, "query", "SELECT 1")
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ing := exec.CommandContext(ctx, a.bin, "ingest", ".")
		ing.Dir = repoPath
		out, err := ing.CombinedOutput()
		if err != nil {
			return time.Since(start).Milliseconds(), fmt.Errorf("defn ingest: %v\n%s", err, out)
		}
	}
	if err := a.primeIDF(repoPath); err != nil {
		return time.Since(start).Milliseconds(), fmt.Errorf("prime idf: %w", err)
	}
	if benchVerbose() {
		if idf, ok := a.idf[repoPath]; ok && idf != nil {
			fmt.Fprintf(os.Stderr, "[defn-ranked] %s indexed: IDF over %d sampled bodies, %d unique tokens, maxIDF=%.2f\n",
				repoPath, idf.docs, len(idf.scores), idf.maxIDF)
		}
	}
	return time.Since(start).Milliseconds(), nil
}

func (a *DefnRanked) Retrieve(repoPath string, task benchtype.Task, tokenBudget int) (benchtype.RetrievalResult, error) {
	start := time.Now()

	keywords := extractKeywords(task.Description)
	if len(keywords) == 0 {
		return benchtype.RetrievalResult{System: a.Name(), TaskID: task.ID}, nil
	}

	seen := make(map[string]rank.Candidate)
	for _, kw := range keywords {
		rows, err := a.queryWithBody(repoPath, kw)
		if err != nil {
			if benchVerbose() {
				fmt.Fprintf(os.Stderr, "[defn-ranked] task=%s keyword=%q query error: %v\n",
					task.ID, kw, err)
			}
			continue
		}
		if benchVerbose() && len(rows) == 0 {
			fmt.Fprintf(os.Stderr, "[defn-ranked] task=%s keyword=%q returned 0 rows\n",
				task.ID, kw)
		}
		for _, r := range rows {
			key := r.def.Receiver + "." + r.def.Name
			if _, ok := seen[key]; !ok {
				seen[key] = rank.Candidate{
					Def:         r.def,
					CallerCount: r.callers,
					TestCount:   r.tests,
				}
			}
		}
	}

	cands := make([]rank.Candidate, 0, len(seen))
	for _, c := range seen {
		cands = append(cands, c)
	}

	idf := a.idfFor(repoPath)
	scored := rank.Rank(task.Description, cands, idf, rank.DefaultWeights)

	// Graph re-rank ("lexical proposes, graph disposes" -- see
	// internal/rank/graphrank.go): the same rank.GraphRerank internal/mcp
	// wires into search/context, applied here so this harness's P@10/R@10/
	// NDCG/MRR numbers actually measure it instead of only exercising the
	// lexical Rank path. BENCH_GRAPH_RERANK=0 disables it for a paired
	// before/after comparison.
	if w := benchGraphRerankWeight(); w > 0 && len(cands) >= 2 {
		ids := make([]int64, len(cands))
		for i, c := range cands {
			ids[i] = c.Def.ID
		}
		if edges, err := a.edgesAmong(repoPath, ids); err == nil {
			scored = rank.GraphRerank(scored, edges, w, 0, 0)
		} else if benchVerbose() {
			fmt.Fprintf(os.Stderr, "[defn-ranked] task=%s edgesAmong error: %v\n", task.ID, err)
		}
	}

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

	return benchtype.RetrievalResult{
		System:     a.Name(),
		TaskID:     task.ID,
		Symbols:    symbols,
		TokensUsed: len(symbols) * 15,
		LatencyMs:  time.Since(start).Milliseconds(),
	}, nil
}

func (a *DefnRanked) SupportsLearning() bool                                      { return false }
func (a *DefnRanked) RecordFeedback(_ string, _ benchtype.Task, _ []string) error { return nil }
func (a *DefnRanked) Reset(_ string) error                                        { return nil }

// queryWithBody fetches id/name/receiver/kind/source_file plus body and
// non-test caller count for every def whose name contains kw. Stays in
// the CLI shell-out style of the unranked adapter so we measure the
// same path; the only difference is the SELECT shape. id is required
// for the graph-rerank measurement (GraphRerank keys everything by
// Definition.ID) -- earlier versions of this adapter never selected it,
// which would have silently collapsed every candidate onto ID 0.
func (a *DefnRanked) queryWithBody(repoPath, keyword string) ([]struct {
	def     store.Definition
	callers int
	tests   int
}, error) {
	kw := strings.ReplaceAll(keyword, "'", "")
	// 200 cap (was 50) — the earlier limit was clipping the candidate
	// pool before the ranker ever saw the relevant items, so high-recall
	// ground truths were unreachable no matter what the weights did.
	// Two correlated subqueries fill caller_count (non-test) and
	// test_count (test) so the ranker's graph-signal features have data.
	sql := fmt.Sprintf(
		"SELECT d.id AS id, d.name, IFNULL(d.receiver,'') AS receiver, d.`kind` AS kind, "+
			"IFNULL(d.source_file,'') AS source_file, IFNULL(b.body,'') AS body, "+
			"(SELECT COUNT(*) FROM refs r JOIN definitions c ON c.id = r.from_def "+
			"WHERE c.test = FALSE AND r.to_def = d.id) AS caller_count, "+
			"(SELECT COUNT(*) FROM refs r JOIN definitions c ON c.id = r.from_def "+
			"WHERE c.test = TRUE AND r.to_def = d.id) AS test_count "+
			"FROM definitions d LEFT JOIN bodies b ON b.def_id = d.id "+
			"WHERE d.test = FALSE AND d.name LIKE '%%%s%%' LIMIT 200",
		kw,
	)
	cmd := exec.Command(a.bin, "query", sql)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	maps, err := parseJSONRows(out)
	if err != nil {
		return nil, err
	}
	rows := make([]struct {
		def     store.Definition
		callers int
		tests   int
	}, 0, len(maps))
	for _, m := range maps {
		rows = append(rows, struct {
			def     store.Definition
			callers int
			tests   int
		}{
			def: store.Definition{
				ID:         int64(asInt(m["id"])),
				Name:       asStr(m["name"]),
				Receiver:   asStr(m["receiver"]),
				Kind:       asStr(m["kind"]),
				SourceFile: asStr(m["source_file"]),
				Body:       asStr(m["body"]),
			},
			callers: asInt(m["caller_count"]),
			tests:   asInt(m["test_count"]),
		})
	}
	return rows, nil
}

func parseJSONRows(out []byte) ([]map[string]any, error) {
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
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out, nil
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

// adapterIDF is a per-repo IDF source built once at Index time from a
// body sample. Implements rank.IDF.
type adapterIDF struct {
	scores map[string]float64
	maxIDF float64
	docs   int
}

func (i *adapterIDF) Score(token string) float64 {
	if v, ok := i.scores[token]; ok {
		return v
	}
	return i.maxIDF
}

func (a *DefnRanked) idfFor(repoPath string) rank.IDF {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i, ok := a.idf[repoPath]; ok {
		return i
	}
	return rank.PlaceholderIDF
}

func (a *DefnRanked) primeIDF(repoPath string) error {
	// Sample 5000 non-test bodies, ordered by hash so the sample is
	// stable across reruns. Same recipe as store.SampleBodies; using
	// the CLI shell-out preserves the "everything goes through the
	// public surface" property of the bench.
	sql := "SELECT IFNULL(b.body,'') AS body FROM definitions d " +
		"JOIN bodies b ON b.def_id = d.id WHERE d.test = FALSE " +
		"ORDER BY d.hash LIMIT 5000"
	cmd := exec.Command(a.bin, "query", sql)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	maps, err := parseJSONRows(out)
	if err != nil {
		return err
	}
	bodies := make([]string, 0, len(maps))
	for _, m := range maps {
		bodies = append(bodies, asStr(m["body"]))
	}
	idf := buildAdapterIDF(bodies)
	a.mu.Lock()
	a.idf[repoPath] = idf
	a.mu.Unlock()
	return nil
}

// buildAdapterIDF computes log(N/df) for each token in the sample. N is
// the sample size; tokens not present score at the rarest-observed level
// (so unique query terms still get full signal).
func buildAdapterIDF(bodies []string) *adapterIDF {
	df := make(map[string]int)
	for _, body := range bodies {
		seen := make(map[string]bool)
		for _, tok := range tokenizeForIDF(body) {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}
	n := len(bodies)
	if n == 0 {
		return &adapterIDF{scores: nil, maxIDF: 0, docs: 0}
	}
	scores := make(map[string]float64, len(df))
	var maxIDF float64
	for tok, count := range df {
		s := logRatio(n, count)
		scores[tok] = s
		if s > maxIDF {
			maxIDF = s
		}
	}
	return &adapterIDF{scores: scores, maxIDF: maxIDF, docs: n}
}

func tokenizeForIDF(s string) []string {
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

// logRatio returns log((n+1)/(df+1)) — smoothed to avoid log(0) edge
// cases and to keep tokens that appear everywhere from going negative.
func logRatio(n, df int) float64 {
	num := float64(n + 1)
	den := float64(df + 1)
	return math.Log(num / den)
}

// edgesAmong fetches every refs edge (from_def, to_def) where BOTH
// endpoints are in ids, shelled through the same `defn query` path
// queryWithBody uses -- mirrors internal/store's SQLiteDB.EdgesAmong, but
// this adapter has no store.Backend handle of its own (it only ever talks
// to the project through the CLI), so it can't call that method directly.
func (a *DefnRanked) edgesAmong(repoPath string, ids []int64) ([][2]int64, error) {
	if len(ids) < 2 {
		return nil, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = fmt.Sprintf("%d", id)
	}
	in := "(" + strings.Join(strs, ",") + ")"
	sql := "SELECT from_def, to_def FROM refs WHERE from_def IN " + in + " AND to_def IN " + in
	cmd := exec.Command(a.bin, "query", sql)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	maps, err := parseJSONRows(out)
	if err != nil {
		return nil, err
	}
	edges := make([][2]int64, 0, len(maps))
	for _, m := range maps {
		edges = append(edges, [2]int64{int64(asInt(m["from_def"])), int64(asInt(m["to_def"]))})
	}
	return edges, nil
}

// benchGraphRerankWeight lets a before/after comparison run be driven from
// the environment instead of a code edit: BENCH_GRAPH_RERANK=0 disables the
// rerank entirely (GraphRerank's own graphWeight<=0 early-return), matching
// internal/mcp's graphRerankWeight (1.0) otherwise -- same equal-footing,
// "pending tune" rationale as DefaultWeights' other entries.
func benchGraphRerankWeight() float64 {
	if os.Getenv("BENCH_GRAPH_RERANK") == "0" {
		return 0
	}
	return 1.0
}
