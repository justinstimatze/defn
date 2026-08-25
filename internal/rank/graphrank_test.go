package rank

import (
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

// TestPersonalizedPageRank_ConnectedNodeOutranksIsolatedNodeGivenEqualSeeds
// is the core "lexical proposes, graph disposes" claim in miniature: node 1
// is isolated, node 2 is connected to two other positively-seeded nodes.
// Given EQUAL restart weight for nodes 1 and 2, node 2 must end up ranked
// higher -- it receives everything node 1 does (an identical share of the
// dangling-mass recycle, since dangling mass is split by restart weight and
// both share the same weight) PLUS real inflow from its neighbors, which
// node 1 structurally cannot receive. This holds regardless of iteration
// count or alpha, so it's safe to assert exactly rather than qualitatively.
func TestPersonalizedPageRank_ConnectedNodeOutranksIsolatedNodeGivenEqualSeeds(t *testing.T) {
	edges := [][2]int64{{2, 3}, {2, 4}}
	seeds := map[int64]float64{1: 10, 2: 10, 3: 4, 4: 4}
	pr := PersonalizedPageRank(edges, seeds, 0.25, 25)
	if pr[2] <= pr[1] {
		t.Fatalf("expected connected node 2 (score=%v) to outrank isolated node 1 (score=%v) despite equal seed weight", pr[2], pr[1])
	}
}

// TestPersonalizedPageRank_NoPositiveSeedsReturnsEmpty guards the early
// return: an all-zero or empty seed map has nothing to restart the walk
// from and must not panic on the seedTotal <= 0 division.
func TestPersonalizedPageRank_NoPositiveSeedsReturnsEmpty(t *testing.T) {
	pr := PersonalizedPageRank([][2]int64{{1, 2}}, map[int64]float64{1: 0}, 0.25, 25)
	if len(pr) != 0 {
		t.Fatalf("expected an empty result with no positive seeds, got %v", pr)
	}
}

// TestPersonalizedPageRank_TopScoreIsNormalizedToOne confirms the
// documented normalization: whichever node ends up with the most mass is
// scaled to exactly 1.0, so scores are always comparable regardless of how
// many seeds or iterations were used.
func TestPersonalizedPageRank_TopScoreIsNormalizedToOne(t *testing.T) {
	edges := [][2]int64{{1, 2}}
	pr := PersonalizedPageRank(edges, map[int64]float64{1: 1}, 0.25, 25)
	var max float64
	for _, v := range pr {
		if v > max {
			max = v
		}
	}
	if max != 1 {
		t.Fatalf("expected the top score to normalize to 1, got %v (full: %v)", max, pr)
	}
}

// TestGraphRerank_ConnectedCandidateGainsCentralityBonus is GraphRerank's
// own version of the isolated-vs-connected claim above, exercised through
// the public post-processing API a real caller (search's ranker) uses:
// given two candidates tied on lexical score, the one wired into a cluster
// of other positively-scored candidates ends up ranked above the isolated
// one once the graph bonus is applied.
func TestGraphRerank_ConnectedCandidateGainsCentralityBonus(t *testing.T) {
	scored := []ScoredRef{
		{Def: store.Definition{ID: 1, Name: "Isolated"}, Score: 10, Reasons: map[string]float64{}},
		{Def: store.Definition{ID: 2, Name: "Connected"}, Score: 10, Reasons: map[string]float64{}},
		{Def: store.Definition{ID: 3, Name: "Neighbor"}, Score: 4, Reasons: map[string]float64{}},
		{Def: store.Definition{ID: 4, Name: "Neighbor2"}, Score: 4, Reasons: map[string]float64{}},
	}
	edges := [][2]int64{{2, 3}, {2, 4}}
	out := GraphRerank(scored, edges, 5.0, 0.25, 25)

	byID := make(map[int64]ScoredRef, len(out))
	for _, sr := range out {
		byID[sr.Def.ID] = sr
	}
	isolated, connected := byID[1], byID[2]
	if connected.Reasons["graph_centrality"] <= isolated.Reasons["graph_centrality"] {
		t.Fatalf("expected connected candidate's graph_centrality (%v) to exceed isolated candidate's (%v)",
			connected.Reasons["graph_centrality"], isolated.Reasons["graph_centrality"])
	}
	if connected.Score <= isolated.Score {
		t.Fatalf("expected the connected candidate to outrank the isolated one after graph rerank given an equal lexical score: connected=%v isolated=%v",
			connected.Score, isolated.Score)
	}
}

func TestGraphRerank_EmptyEdgesIsNoOp(t *testing.T) {
	scored := []ScoredRef{{Def: store.Definition{ID: 1}, Score: 5, Reasons: map[string]float64{}}}
	out := GraphRerank(scored, nil, 5, 0.25, 25)
	if out[0].Score != 5 {
		t.Fatalf("expected score unchanged with no edges, got %v", out[0].Score)
	}
}

func TestGraphRerank_ZeroWeightIsNoOp(t *testing.T) {
	scored := []ScoredRef{{Def: store.Definition{ID: 1}, Score: 5, Reasons: map[string]float64{}}}
	out := GraphRerank(scored, [][2]int64{{1, 2}}, 0, 0.25, 25)
	if out[0].Score != 5 {
		t.Fatalf("expected score unchanged with graphWeight=0, got %v", out[0].Score)
	}
	if _, ok := out[0].Reasons["graph_centrality"]; ok {
		t.Fatal("expected no graph_centrality reason to be added when graphWeight=0")
	}
}

// TestGraphRerank_DoesNotMutateCallersOriginalReasonsMap is the regression
// for a real bug a code review caught: `copy(out, scored)` only copies the
// map HEADER, leaving out[i].Reasons aliased to the exact same underlying
// map as scored[i].Reasons -- so writing out[i].Reasons["graph_centrality"]
// silently mutated the caller's original slice too, despite GraphRerank
// looking like it returns an independent copy.
func TestGraphRerank_DoesNotMutateCallersOriginalReasonsMap(t *testing.T) {
	originalReasons := map[string]float64{"name_match": 1.0}
	scored := []ScoredRef{
		{Def: store.Definition{ID: 1}, Score: 10, Reasons: originalReasons},
		{Def: store.Definition{ID: 2}, Score: 10, Reasons: map[string]float64{"name_match": 1.0}},
	}
	edges := [][2]int64{{1, 2}}

	_ = GraphRerank(scored, edges, 5.0, 0.25, 25)

	if _, ok := originalReasons["graph_centrality"]; ok {
		t.Errorf("expected the caller's original Reasons map to be untouched, but graph_centrality leaked into it: %v", originalReasons)
	}
	if len(originalReasons) != 1 {
		t.Errorf("expected the caller's original Reasons map to keep its original single entry, got %v", originalReasons)
	}
}
