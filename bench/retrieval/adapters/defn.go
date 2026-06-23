// Package adapters implements benchtype.Adapter for code retrieval systems.
//
// defn.go shells out to the `defn` CLI in the target repo, querying the
// embedded Dolt database via `defn query`. P0 baseline: no ranking — results
// are returned in (exact > prefix > substring) order, shorter names first
// within each bucket. Ranking work belongs in a separate ranker package
// once the unranked baseline is measured.
package adapters

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/normalize"
)

type Defn struct {
	bin string
}

func NewDefn() *Defn {
	bin, err := exec.LookPath("defn")
	if err != nil {
		bin = "defn"
	}
	return &Defn{bin: bin}
}

func (a *Defn) Name() string { return "defn" }

// Index runs `defn ingest .` in the repo if the database doesn't exist yet.
func (a *Defn) Index(repoPath string) (int64, error) {
	start := time.Now()
	cmd := exec.Command(a.bin, "query", "SELECT 1")
	cmd.Dir = repoPath
	if err := cmd.Run(); err == nil {
		return time.Since(start).Milliseconds(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd = exec.CommandContext(ctx, a.bin, "ingest", ".")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return time.Since(start).Milliseconds(), fmt.Errorf("defn ingest: %v\n%s", err, out)
	}
	return time.Since(start).Milliseconds(), nil
}

func (a *Defn) Retrieve(repoPath string, task benchtype.Task, tokenBudget int) (benchtype.RetrievalResult, error) {
	start := time.Now()

	keywords := extractKeywords(task.Description)
	if len(keywords) == 0 {
		return benchtype.RetrievalResult{System: "defn", TaskID: task.ID}, nil
	}

	type cand struct {
		name     string
		receiver string
		kind     string
		file     string
		matchTy  int
		kwIdx    int
	}

	seen := make(map[string]*cand)

	for kwIdx, kw := range keywords {
		rows, err := a.queryName(repoPath, kw)
		if err != nil {
			continue
		}
		for _, r := range rows {
			matchTy := classifyMatch(r.name, kw)
			key := r.qualifiedName()
			if prev, ok := seen[key]; ok {
				if matchTy < prev.matchTy || (matchTy == prev.matchTy && kwIdx < prev.kwIdx) {
					prev.matchTy = matchTy
					prev.kwIdx = kwIdx
				}
				continue
			}
			seen[key] = &cand{name: r.name, receiver: r.receiver, kind: r.kind, file: r.file, matchTy: matchTy, kwIdx: kwIdx}
		}
	}

	candidates := make([]*cand, 0, len(seen))
	for _, c := range seen {
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		ai, bj := candidates[i], candidates[j]
		if ai.matchTy != bj.matchTy {
			return ai.matchTy < bj.matchTy
		}
		if ai.kwIdx != bj.kwIdx {
			return ai.kwIdx < bj.kwIdx
		}
		return len(ai.name) < len(bj.name)
	})

	limit := 20
	if len(candidates) < limit {
		limit = len(candidates)
	}
	symbols := make([]benchtype.RetrievedSymbol, 0, limit)
	for i := 0; i < limit; i++ {
		c := candidates[i]
		qn := c.name
		if c.receiver != "" {
			qn = c.receiver + "." + c.name
		}
		symbols = append(symbols, benchtype.RetrievedSymbol{
			QualifiedName: qn,
			Normalized:    normalize.Symbol(qn),
			Rank:          i + 1,
			FilePath:      c.file,
			Kind:          c.kind,
		})
	}

	return benchtype.RetrievalResult{
		System:     "defn",
		TaskID:     task.ID,
		Symbols:    symbols,
		TokensUsed: len(symbols) * 15,
		LatencyMs:  time.Since(start).Milliseconds(),
	}, nil
}

func (a *Defn) SupportsLearning() bool                                      { return false }
func (a *Defn) RecordFeedback(_ string, _ benchtype.Task, _ []string) error { return nil }
func (a *Defn) Reset(_ string) error                                        { return nil }

type defnRow struct {
	name     string
	receiver string
	kind     string
	file     string
}

func (r defnRow) qualifiedName() string {
	if r.receiver != "" {
		return r.receiver + "." + r.name
	}
	return r.name
}

func (a *Defn) queryName(repoPath, keyword string) ([]defnRow, error) {
	kw := strings.ReplaceAll(keyword, "'", "")
	sql := fmt.Sprintf(
		"SELECT name, IFNULL(receiver,'') AS receiver, `kind`, IFNULL(source_file,'') AS source_file "+
			"FROM definitions WHERE test = FALSE AND name LIKE '%%%s%%' LIMIT 50",
		kw,
	)
	cmd := exec.Command(a.bin, "query", sql)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseDefnJSON(out)
}

func parseDefnJSON(out []byte) ([]defnRow, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var raw []map[string]any
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return nil, err
		}
		return rowsFromMaps(raw), nil
	}
	if trimmed[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return nil, err
		}
		if rows, ok := obj["rows"].([]any); ok {
			maps := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					maps = append(maps, m)
				}
			}
			return rowsFromMaps(maps), nil
		}
		return rowsFromMaps([]map[string]any{obj}), nil
	}
	var maps []map[string]any
	sc := bufio.NewScanner(strings.NewReader(trimmed))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			maps = append(maps, m)
		}
	}
	return rowsFromMaps(maps), nil
}

func rowsFromMaps(maps []map[string]any) []defnRow {
	out := make([]defnRow, 0, len(maps))
	for _, m := range maps {
		out = append(out, defnRow{
			name:     asStr(m["name"]),
			receiver: asStr(m["receiver"]),
			kind:     asStr(m["kind"]),
			file:     asStr(m["source_file"]),
		})
	}
	return out
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

func classifyMatch(name, kw string) int {
	nl := strings.ToLower(name)
	kl := strings.ToLower(kw)
	switch {
	case nl == kl:
		return 0
	case strings.HasPrefix(nl, kl):
		return 1
	default:
		return 2
	}
}
