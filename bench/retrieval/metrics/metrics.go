// Package metrics computes retrieval quality metrics for the cross-system benchmark.
package metrics

import (
	"math"
	"sort"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/normalize"
)

// Compute calculates all metrics for a single retrieval result against ground truth.
func Compute(result benchtype.RetrievalResult, groundTruth []string) benchtype.MetricResult {
	retrieved := make([]string, len(result.Symbols))
	for i, s := range result.Symbols {
		retrieved[i] = s.QualifiedName
	}

	p5 := precisionAtK(retrieved, groundTruth, 5)
	p10 := precisionAtK(retrieved, groundTruth, 10)
	p20 := precisionAtK(retrieved, groundTruth, 20)
	r5 := recallAtK(retrieved, groundTruth, 5)
	r10 := recallAtK(retrieved, groundTruth, 10)
	r20 := recallAtK(retrieved, groundTruth, 20)
	ndcg := ndcgAtK(retrieved, groundTruth, 10)
	mrr := reciprocalRank(retrieved, groundTruth)
	f1 := harmonicMean(p10, r10)

	var tokenEff float64
	if result.TokensUsed > 0 {
		relevant := countRelevant(retrieved, groundTruth)
		tokenEff = float64(relevant) / float64(result.TokensUsed)
	}

	return benchtype.MetricResult{
		System:          result.System,
		TaskID:          result.TaskID,
		PrecisionAt5:    p5,
		PrecisionAt10:   p10,
		PrecisionAt20:   p20,
		RecallAt5:       r5,
		RecallAt10:      r10,
		RecallAt20:      r20,
		NDCGAt10:        ndcg,
		MRR:             mrr,
		F1At10:          f1,
		TokenEfficiency: tokenEff,
		TokensUsed:      result.TokensUsed,
		LatencyMs:       result.LatencyMs,
	}
}

// Aggregate computes per-system aggregate metrics from individual results.
func Aggregate(results []benchtype.MetricResult, system string) benchtype.AggregateResult {
	var filtered []benchtype.MetricResult
	for _, r := range results {
		if r.System == system {
			filtered = append(filtered, r)
		}
	}

	if len(filtered) == 0 {
		return benchtype.AggregateResult{System: system}
	}

	var sumP10, sumR10, sumNDCG, sumMRR, sumF1, sumTokenEff float64
	var latencies []int64
	failures := 0

	for _, r := range filtered {
		sumP10 += r.PrecisionAt10
		sumR10 += r.RecallAt10
		sumNDCG += r.NDCGAt10
		sumMRR += r.MRR
		sumF1 += r.F1At10
		sumTokenEff += r.TokenEfficiency
		latencies = append(latencies, r.LatencyMs)
		if r.PrecisionAt10 == 0 && r.RecallAt10 == 0 {
			failures++
		}
	}

	n := float64(len(filtered))
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	var medianLatency int64
	if len(latencies) > 0 {
		medianLatency = latencies[len(latencies)/2]
	}

	return benchtype.AggregateResult{
		System:            system,
		MeanPrecisionAt10: sumP10 / n,
		MeanRecallAt10:    sumR10 / n,
		MeanNDCGAt10:      sumNDCG / n,
		MeanMRR:           sumMRR / n,
		MeanF1At10:        sumF1 / n,
		MeanTokenEff:      sumTokenEff / n,
		MedianLatencyMs:   medianLatency,
		TaskCount:         len(filtered),
		FailureCount:      failures,
	}
}

func precisionAtK(retrieved, groundTruth []string, k int) float64 {
	if k <= 0 {
		return 0
	}
	topK := retrieved
	if len(topK) > k {
		topK = topK[:k]
	}
	if len(topK) == 0 {
		return 0
	}

	relevant := 0
	for _, r := range topK {
		if isRelevant(r, groundTruth) {
			relevant++
		}
	}
	return float64(relevant) / float64(k)
}

func recallAtK(retrieved, groundTruth []string, k int) float64 {
	if len(groundTruth) == 0 {
		return 0
	}
	topK := retrieved
	if len(topK) > k {
		topK = topK[:k]
	}

	found := 0
	for _, gt := range groundTruth {
		for _, r := range topK {
			if normalize.MatchesGroundTruth(r, gt) {
				found++
				break
			}
		}
	}
	return float64(found) / float64(len(groundTruth))
}

func ndcgAtK(retrieved, groundTruth []string, k int) float64 {
	topK := retrieved
	if len(topK) > k {
		topK = topK[:k]
	}

	dcg := 0.0
	for i, r := range topK {
		if isRelevant(r, groundTruth) {
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}

	// IDCG: ideal ranking puts all relevant at the top
	idealK := len(groundTruth)
	if idealK > k {
		idealK = k
	}
	idcg := 0.0
	for i := 0; i < idealK; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}

	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func reciprocalRank(retrieved, groundTruth []string) float64 {
	for i, r := range retrieved {
		if isRelevant(r, groundTruth) {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

func harmonicMean(a, b float64) float64 {
	if a+b == 0 {
		return 0
	}
	return 2 * (a * b) / (a + b)
}

func countRelevant(retrieved, groundTruth []string) int {
	count := 0
	for _, r := range retrieved {
		if isRelevant(r, groundTruth) {
			count++
		}
	}
	return count
}

func isRelevant(retrieved string, groundTruth []string) bool {
	for _, gt := range groundTruth {
		if normalize.MatchesGroundTruth(retrieved, gt) {
			return true
		}
	}
	return false
}
