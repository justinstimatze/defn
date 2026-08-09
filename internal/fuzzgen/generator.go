package fuzzgen

import "math/rand/v2"

// Generate builds a deterministic SyntheticModule: a trivial base module
// plus every hazard in opts.Hazards applied in order. The same (r, opts)
// always produces the same module -- callers seed r explicitly (e.g. via
// rand.New(rand.NewPCG(seed, seed))) for reproducibility.
func Generate(r *rand.Rand, opts GenOpts) *SyntheticModule {
	m := NewModule("example.com/synth")
	for _, h := range opts.Hazards {
		h.Apply(r, m)
	}
	return m
}

// GenOpts selects which hazards Generate applies.
type GenOpts struct {
	Hazards []Hazard
}
