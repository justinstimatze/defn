package fuzzgen

import (
	"math/rand/v2"
	"testing"
)

// FuzzRoundTrip is the slower, opt-in deep-search companion to
// TestRoundTrip_Hazards: `go test -run=FuzzRoundTrip -fuzz=FuzzRoundTrip
// -fuzztime=5m`. Any crasher Go's fuzzing engine finds is written to
// testdata/fuzz/FuzzRoundTrip/<hash> automatically and becomes a
// permanent regression seed the next time `go test ./...` runs it as
// part of the corpus -- found bugs become permanent tests for free.
func FuzzRoundTrip(f *testing.F) {
	// -short skips corpus replay entirely: this corpus only ever grows
	// (every crasher Go's fuzzer finds becomes a permanent seed), so its
	// replay cost has no ceiling over time. Keeps it out of the fast,
	// push-gating path (.githooks/pre-push runs -short) while CI's full,
	// non-short run still exercises the whole accumulated corpus.
	if testing.Short() {
		f.Skip("skipping FuzzRoundTrip corpus replay in -short mode")
	}
	f.Add(uint64(1))
	f.Add(uint64(2))
	f.Add(uint64(42))
	f.Fuzz(func(t *testing.T, seed uint64) {
		r := rand.New(rand.NewPCG(seed, seed))
		m := Generate(r, GenOpts{Hazards: hazardSubset(seed)})
		assertRoundTrip(t, m)
	})
}

// hazardSubset deterministically picks a non-empty subset of AllHazards
// from a single seed, so FuzzRoundTrip's one integer argument still
// explores hazard combinations, not just per-hazard internal
// randomization.
func hazardSubset(seed uint64) []Hazard {
	r := rand.New(rand.NewPCG(seed, seed))
	mask := r.IntN((1<<len(AllHazards))-1) + 1 // never empty
	var out []Hazard
	for i, h := range AllHazards {
		if mask&(1<<i) != 0 {
			out = append(out, h)
		}
	}
	return out
}
