package rank

import "sort"

// PersonalizedPageRank computes random-walk-with-restart scores over an
// undirected adjacency built from edges (pairs of definition IDs), seeded
// by seeds (def ID -> restart weight; only positive weights matter).
//
// This is "lexical proposes, graph disposes": pure term-overlap ranking
// treats every candidate independently, so a definition that merely shares
// a word with the query (an unrelated "overlay" widget) can outrank the
// definition the query is actually about, purely on word count. Seeding a
// personalized PageRank walk with the lexical scores and re-ranking by the
// result lets structural centrality within the matched cluster break that
// tie: a definition tightly wired into the code the query touches gains
// mass from its neighbors; a lexically-matched but structurally isolated
// definition keeps only its own restart mass and sinks.
//
// alpha is the restart probability (0.25 is the standard value -- higher
// keeps the walk closer to the seeds). iters is the power-iteration count
// (25 converges comfortably at the candidate-set scale this is used at).
// Returns a score per def ID reached by the walk, normalized so the top
// score is 1; defs the walk never reaches are absent from the result.
//
// Deterministic, $0, no embeddings, no network -- same lexical-seed ->
// graph-rank pipeline used by NanoNets/Graft (MIT-licensed;
// github.com/NanoNets/Graft), adapted here to def IDs over defn's own refs
// table instead of Graft's markdown concept-node graph.
func PersonalizedPageRank(edges [][2]int64, seeds map[int64]float64, alpha float64, iters int) map[int64]float64 {
	if alpha <= 0 {
		alpha = 0.25
	}
	if iters <= 0 {
		iters = 25
	}

	adj := make(map[int64][]int64, len(edges)*2)
	link := func(a, b int64) {
		adj[a] = append(adj[a], b)
	}
	for _, e := range edges {
		a, b := e[0], e[1]
		if a == b {
			continue
		}
		link(a, b)
		link(b, a)
	}

	var seedTotal float64
	for _, w := range seeds {
		if w > 0 {
			seedTotal += w
		}
	}
	if seedTotal <= 0 {
		return map[int64]float64{}
	}
	restart := make(map[int64]float64, len(seeds))
	for id, w := range seeds {
		if w > 0 {
			restart[id] = w / seedTotal
		}
	}

	rank := make(map[int64]float64, len(restart))
	for id, w := range restart {
		rank[id] = w
	}
	for i := 0; i < iters; i++ {
		next := make(map[int64]float64, len(rank))
		for id, r := range restart {
			next[id] = alpha * r
		}
		var dangling float64
		for id, mass := range rank {
			nbrs := adj[id]
			if len(nbrs) == 0 {
				dangling += mass
				continue
			}
			share := (1 - alpha) * mass / float64(len(nbrs))
			for _, nb := range nbrs {
				next[nb] += share
			}
		}
		if dangling > 0 {
			dm := (1 - alpha) * dangling
			for id, r := range restart {
				next[id] += dm * r
			}
		}
		rank = next
	}

	var max float64
	for _, v := range rank {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return map[int64]float64{}
	}
	out := make(map[int64]float64, len(rank))
	for id, v := range rank {
		out[id] = v / max
	}
	return out
}

// GraphRerank adjusts already-computed scores (typically Rank's own output)
// by structural centrality within the candidate set's own reference
// subgraph -- see PersonalizedPageRank's doc for why. edges should be pairs
// of def IDs restricted to members of scored (a bounded, cheap subgraph;
// this is not meant to walk the whole project's refs table). graphWeight
// controls how much centrality can move the final order; 0 or a nil/empty
// edges list leaves scored unchanged. alpha/iters are forwarded to
// PersonalizedPageRank (0 picks its defaults).
//
// Each result's Reasons gains a "graph_centrality" entry (0 for a def the
// walk didn't reach) alongside the lexical/graph-signal features Rank
// already recorded, so the breakdown stays fully explainable. Returns a
// genuinely independent copy -- each Reasons map is cloned, not aliased --
// so mutating the result never reaches back into the caller's original
// scored slice (a plain `copy` of []ScoredRef only copies the map HEADER,
// leaving out[i].Reasons pointing at the same underlying map as
// scored[i].Reasons).
func GraphRerank(scored []ScoredRef, edges [][2]int64, graphWeight, alpha float64, iters int) []ScoredRef {
	if graphWeight <= 0 || len(edges) == 0 || len(scored) == 0 {
		return scored
	}
	seeds := make(map[int64]float64, len(scored))
	for _, sr := range scored {
		if sr.Score > 0 {
			seeds[sr.Def.ID] = sr.Score
		}
	}
	pr := PersonalizedPageRank(edges, seeds, alpha, iters)
	out := make([]ScoredRef, len(scored))
	for i, sr := range scored {
		reasons := make(map[string]float64, len(sr.Reasons)+1)
		for k, v := range sr.Reasons {
			reasons[k] = v
		}
		g := pr[sr.Def.ID]
		reasons["graph_centrality"] = g
		out[i] = ScoredRef{Def: sr.Def, Score: sr.Score + g*graphWeight, Reasons: reasons}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}
