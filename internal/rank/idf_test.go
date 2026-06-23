package rank

import "testing"

type fakeBodySource struct {
	bodies     []string
	count      int
	sampleHits int
}

func (f *fakeBodySource) CountDefinitions() (int, error) {
	if f.count > 0 {
		return f.count, nil
	}
	return len(f.bodies), nil
}

func (f *fakeBodySource) SampleBodies(n int) ([]string, error) {
	f.sampleHits++
	if n >= len(f.bodies) {
		return f.bodies, nil
	}
	return f.bodies[:n], nil
}

func TestLazyIDF_FrequentTokenScoresLowerThanRare(t *testing.T) {
	src := &fakeBodySource{bodies: []string{
		"ctx err foo",
		"ctx err bar",
		"ctx err baz",
		"ctx err qux",
		"ctx needleString",
	}}
	idf := NewLazyIDF(src)
	frequent := idf.Score("ctx")
	rare := idf.Score("needlestring")
	if !(rare > frequent) {
		t.Errorf("rare token (%.3f) should outscore frequent (%.3f)", rare, frequent)
	}
	if frequent < 0 {
		t.Errorf("frequent token IDF should be >= 0, got %.3f", frequent)
	}
}

func TestLazyIDF_AbsentTokenScoresAtLeastRarest(t *testing.T) {
	src := &fakeBodySource{bodies: []string{
		"ctx err",
		"ctx err",
		"ctx unique",
	}}
	idf := NewLazyIDF(src)
	rarest := idf.Score("unique")
	absent := idf.Score("nevermentioned")
	if !(absent >= rarest) {
		t.Errorf("absent (%.3f) should be >= rarest seen (%.3f)", absent, rarest)
	}
}

func TestLazyIDF_EmptyCorpusReturnsZero(t *testing.T) {
	src := &fakeBodySource{}
	idf := NewLazyIDF(src)
	if got := idf.Score("anything"); got != 0 {
		t.Errorf("empty corpus should score 0, got %.3f", got)
	}
}

func TestLazyIDF_CachesAcrossCalls(t *testing.T) {
	src := &fakeBodySource{bodies: []string{"a b", "a c"}}
	idf := NewLazyIDF(src)
	idf.Score("a")
	idf.Score("b")
	idf.Score("c")
	if src.sampleHits != 1 {
		t.Errorf("expected 1 sample call across 3 scores, got %d", src.sampleHits)
	}
}

func TestLazyIDF_InvalidateForcesRebuild(t *testing.T) {
	src := &fakeBodySource{bodies: []string{"a b"}}
	idf := NewLazyIDF(src)
	idf.Score("a")
	if src.sampleHits != 1 {
		t.Fatalf("expected 1 sample, got %d", src.sampleHits)
	}
	idf.Invalidate()
	idf.Score("a")
	if src.sampleHits != 2 {
		t.Errorf("invalidate should trigger rebuild, got %d sample hits", src.sampleHits)
	}
}

func TestLazyIDF_SamplingCapsAtThreshold(t *testing.T) {
	// Synthetic large corpus: 20k unique bodies.
	bodies := make([]string, 20_000)
	for i := range bodies {
		bodies[i] = "common_token"
	}
	src := &fakeBodySource{bodies: bodies, count: 20_000}
	idf := NewLazyIDF(src)
	idf.Score("common_token")
	if idf.cachedN != idfSampleSize {
		t.Errorf("expected cachedN=%d under sampling, got %d", idfSampleSize, idf.cachedN)
	}
}

func TestLazyIDF_RebuildSurvivesSampleError(t *testing.T) {
	// CountDefinitions reports a non-zero corpus but SampleBodies returns
	// nothing — Score must not panic and must return 0.
	src := &fakeBodySource{count: 5} // bodies is empty → SampleBodies returns []
	idf := NewLazyIDF(src)
	if got := idf.Score("anything"); got != 0 {
		t.Errorf("empty sample should yield 0, got %.3f", got)
	}
}
