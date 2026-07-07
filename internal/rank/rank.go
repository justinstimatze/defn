// Package rank scores candidate definitions for a natural-language query.
//
// The package is pure: callers fetch candidates and pre-compute graph stats
// (caller count, test coverage), then Rank produces a sorted list. Rank does
// not touch the store directly so internal/store/ stays independent.
package rank

import (
	"sort"

	"github.com/justinstimatze/defn/internal/store"
)

// Candidate is a definition plus the graph statistics needed for ranking.
// The handler is responsible for batch-filling CallerCount/TestCount before
// calling Rank — doing it per-candidate inside Rank would be N round-trips.
type Candidate struct {
	Def         store.Definition
	CallerCount int // non-test incoming refs
	TestCount   int // distinct test definitions that reach this def
}

// ScoredRef is a candidate with its final score and per-feature contributions.
// Reasons is keyed by feature name (the keys in DefaultWeights) so callers
// can render an explainer alongside the result.
type ScoredRef struct {
	Def     store.Definition
	Score   float64
	Reasons map[string]float64
}

// IDF reports inverse document frequency for a token. Token argument is the
// already-normalized form (lowercase, no separators). Tokens absent from the
// corpus return a high IDF (treated as rare) so unique query terms still
// contribute signal.
type IDF interface {
	Score(token string) float64
}

// constIDF returns the same score for every token. Used as a placeholder
// while the real lazy-sampled IDF is wired in; lets the linear scorer ship
// end-to-end before the corpus-statistics machinery exists.
type constIDF float64

func (c constIDF) Score(string) float64 { return float64(c) }

// PlaceholderIDF is a no-op IDF source. Replace once idf.go lands.
var PlaceholderIDF IDF = constIDF(1.0)

// Weights controls the linear combiner. Tuned on the train split of the
// retrieval benchmark; never against test. See cmd/defn-bench-tune (TBD).
type Weights struct {
	NameMatch     float64
	CallerCount   float64
	TestCount     float64
	BodyOverlap   float64
	ReceiverMatch float64
}

// DefaultWeights are placeholders pending tune. Equal weight across features
// keeps the initial bench number honest — a feature that doesn't help at
// equal weight isn't going to help at tuned weight either.
var DefaultWeights = Weights{
	NameMatch:     1.0,
	CallerCount:   1.0,
	TestCount:     1.0,
	BodyOverlap:   1.0,
	ReceiverMatch: 1.0,
}

// Rank scores every candidate and returns them sorted by descending score.
// Pure function: same inputs → same outputs, no I/O.
func Rank(query string, candidates []Candidate, idf IDF, w Weights) []ScoredRef {
	qTokens := tokenize(query)
	out := make([]ScoredRef, 0, len(candidates))
	for _, c := range candidates {
		reasons := map[string]float64{
			"name_match":     nameMatch(qTokens, c.Def.Name),
			"caller_count":   callerCountScore(c.CallerCount),
			"test_count":     testCountScore(c.TestCount),
			"body_overlap":   bodyOverlap(qTokens, c.Def.Body, idf),
			"receiver_match": receiverMatch(qTokens, c.Def.Receiver),
		}
		score := reasons["name_match"]*w.NameMatch +
			reasons["caller_count"]*w.CallerCount +
			reasons["test_count"]*w.TestCount +
			reasons["body_overlap"]*w.BodyOverlap +
			reasons["receiver_match"]*w.ReceiverMatch
		out = append(out, ScoredRef{Def: c.Def, Score: score, Reasons: reasons})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}
