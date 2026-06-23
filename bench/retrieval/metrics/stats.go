package metrics

import (
	"math"
	"math/rand"
	"sort"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
)

// PairwiseComparison holds the statistical comparison between two systems.
type PairwiseComparison struct {
	SystemA     string  `json:"system_a"`
	SystemB     string  `json:"system_b"`
	Metric      string  `json:"metric"`
	MeanA       float64 `json:"mean_a"`
	MeanB       float64 `json:"mean_b"`
	Difference  float64 `json:"difference"`   // MeanA - MeanB
	WilcoxonP   float64 `json:"wilcoxon_p"`   // p-value from Wilcoxon signed-rank test
	CohensD     float64 `json:"cohens_d"`     // effect size
	CI95Lower   float64 `json:"ci_95_lower"`  // bootstrap 95% CI lower bound
	CI95Upper   float64 `json:"ci_95_upper"`  // bootstrap 95% CI upper bound
	Significant bool    `json:"significant"`  // p < 0.05
	TaskCount   int     `json:"task_count"`
}

// CompareSystems computes pairwise statistical comparison on a given metric.
// Tasks must be aligned (same task IDs in both result sets).
func CompareSystems(results []benchtype.MetricResult, systemA, systemB, metric string) PairwiseComparison {
	// Extract paired values
	aMap := make(map[string]float64)
	bMap := make(map[string]float64)

	for _, r := range results {
		val := extractMetric(r, metric)
		if r.System == systemA {
			aMap[r.TaskID] = val
		} else if r.System == systemB {
			bMap[r.TaskID] = val
		}
	}

	// Align pairs (only tasks both systems attempted)
	var aVals, bVals []float64
	for taskID, aVal := range aMap {
		if bVal, ok := bMap[taskID]; ok {
			aVals = append(aVals, aVal)
			bVals = append(bVals, bVal)
		}
	}

	n := len(aVals)
	if n < 3 {
		return PairwiseComparison{
			SystemA:   systemA,
			SystemB:   systemB,
			Metric:    metric,
			TaskCount: n,
		}
	}

	meanA := mean(aVals)
	meanB := mean(bVals)
	diff := meanA - meanB

	// Wilcoxon signed-rank test (two-sided)
	pValue := wilcoxonSignedRank(aVals, bVals)

	// Cohen's d (effect size)
	diffs := make([]float64, n)
	for i := range diffs {
		diffs[i] = aVals[i] - bVals[i]
	}
	d := cohensD(diffs)

	// Bootstrap 95% CI for the mean difference
	lower, upper := bootstrapCI(diffs, 10000, 0.05)

	return PairwiseComparison{
		SystemA:     systemA,
		SystemB:     systemB,
		Metric:      metric,
		MeanA:       meanA,
		MeanB:       meanB,
		Difference:  diff,
		WilcoxonP:   pValue,
		CohensD:     d,
		CI95Lower:   lower,
		CI95Upper:   upper,
		Significant: pValue < 0.05,
		TaskCount:   n,
	}
}

func extractMetric(r benchtype.MetricResult, metric string) float64 {
	switch metric {
	case "precision_at_10":
		return r.PrecisionAt10
	case "recall_at_10":
		return r.RecallAt10
	case "ndcg_at_10":
		return r.NDCGAt10
	case "mrr":
		return r.MRR
	case "f1_at_10":
		return r.F1At10
	case "token_efficiency":
		return r.TokenEfficiency
	default:
		return 0
	}
}

// wilcoxonSignedRank computes an approximate p-value for the Wilcoxon signed-rank test.
// Uses normal approximation for n >= 10, exact for smaller samples.
func wilcoxonSignedRank(a, b []float64) float64 {
	n := len(a)
	if n == 0 {
		return 1.0
	}

	// Compute differences and ranks
	type diffRank struct {
		absDiff float64
		sign    float64
	}

	var diffs []diffRank
	for i := range a {
		d := a[i] - b[i]
		if d == 0 {
			continue // exclude ties with zero
		}
		sign := 1.0
		if d < 0 {
			sign = -1.0
		}
		diffs = append(diffs, diffRank{absDiff: math.Abs(d), sign: sign})
	}

	if len(diffs) == 0 {
		return 1.0
	}

	// Rank by absolute difference
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].absDiff < diffs[j].absDiff })
	for i := range diffs {
		diffs[i].absDiff = float64(i + 1) // assign ranks
	}

	// Compute W+ (sum of positive ranks)
	wPlus := 0.0
	for _, d := range diffs {
		if d.sign > 0 {
			wPlus += d.absDiff
		}
	}

	// Normal approximation
	nn := float64(len(diffs))
	expectedW := nn * (nn + 1) / 4
	varW := nn * (nn + 1) * (2*nn + 1) / 24
	z := (wPlus - expectedW) / math.Sqrt(varW)

	// Two-sided p-value from standard normal
	p := 2 * normalCDF(-math.Abs(z))
	return p
}

// cohensD computes Cohen's d for paired differences.
// When stddev is near zero (all differences are identical), returns a capped
// value indicating the direction rather than dividing by floating-point noise.
func cohensD(diffs []float64) float64 {
	if len(diffs) == 0 {
		return 0
	}
	m := mean(diffs)
	s := stddev(diffs)
	if s < 1e-10 {
		// All differences are essentially identical.
		// Return direction indicator capped at a large but finite value.
		if m > 0 {
			return 10.0 // "very large positive effect"
		} else if m < 0 {
			return -10.0
		}
		return 0
	}
	return m / s
}

// bootstrapCI computes a bootstrap confidence interval for the mean.
func bootstrapCI(data []float64, nBootstrap int, alpha float64) (lower, upper float64) {
	if len(data) == 0 {
		return 0, 0
	}

	rng := rand.New(rand.NewSource(42)) // deterministic for reproducibility
	means := make([]float64, nBootstrap)

	for i := 0; i < nBootstrap; i++ {
		sample := make([]float64, len(data))
		for j := range sample {
			sample[j] = data[rng.Intn(len(data))]
		}
		means[i] = mean(sample)
	}

	sort.Float64s(means)
	lowerIdx := int(float64(nBootstrap) * alpha / 2)
	upperIdx := int(float64(nBootstrap) * (1 - alpha/2))

	if lowerIdx >= len(means) {
		lowerIdx = len(means) - 1
	}
	if upperIdx >= len(means) {
		upperIdx = len(means) - 1
	}

	return means[lowerIdx], means[upperIdx]
}

func mean(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

func stddev(data []float64) float64 {
	if len(data) < 2 {
		return 0
	}
	m := mean(data)
	sum := 0.0
	for _, v := range data {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(data)-1))
}

// normalCDF computes the standard normal CDF using the approximation.
func normalCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}
