package rank

import (
	"math"
	"sync"
)

// BodySource provides definition bodies for IDF computation. Decoupled from
// *store.Store so tests can substitute fixtures and so rank/ doesn't bind to
// store internals beyond the Definition struct already imported in rank.go.
type BodySource interface {
	// CountDefinitions returns the total number of non-test definitions in
	// the corpus. Returning 0 is fine — Score will treat the table as empty.
	CountDefinitions() (int, error)
	// SampleBodies returns up to n definition bodies. If n >= total, returns
	// all. Sampling should be deterministic for a given corpus state so two
	// queries against the same version see the same IDF.
	SampleBodies(n int) ([]string, error)
}

const (
	idfSampleThreshold = 10_000
	idfSampleSize      = 5_000
)

// LazyIDF builds an IDF table on first use and caches it until Invalidate.
// Concurrent Score calls during a rebuild serialize on the mutex — IDF
// rebuilds are infrequent (post-ingest only) and cheap relative to a query.
type LazyIDF struct {
	src BodySource

	mu          sync.Mutex
	version     uint64
	cachedReady bool
	cached      map[string]float64
	cachedN     int
}

func NewLazyIDF(src BodySource) *LazyIDF {
	return &LazyIDF{src: src}
}

// Invalidate marks the cached table stale. Bump after ingest, resolve, or
// any edit that materially changes the body corpus. Cheap — actual rebuild
// is deferred until the next Score call.
func (l *LazyIDF) Invalidate() {
	l.mu.Lock()
	l.version++
	l.cachedReady = false
	l.mu.Unlock()
}

// Score returns IDF for a token. Tokens absent from the corpus get the
// score of a hypothetical df=0.5 entry — higher than any seen token, so
// unique query terms still carry signal instead of dropping to zero.
func (l *LazyIDF) Score(token string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cachedReady {
		l.rebuildLocked()
	}
	if v, ok := l.cached[token]; ok {
		return v
	}
	if l.cachedN == 0 {
		return 0
	}
	return math.Log(float64(l.cachedN) / 0.5)
}

func (l *LazyIDF) rebuildLocked() {
	defer func() {
		l.cachedReady = true
	}()
	n, err := l.src.CountDefinitions()
	if err != nil || n == 0 {
		l.cached = map[string]float64{}
		l.cachedN = 0
		return
	}
	sampleN := n
	if n > idfSampleThreshold {
		sampleN = idfSampleSize
	}
	bodies, err := l.src.SampleBodies(sampleN)
	if err != nil || len(bodies) == 0 {
		l.cached = map[string]float64{}
		l.cachedN = 0
		return
	}
	df := make(map[string]int, 4096)
	for _, b := range bodies {
		seen := make(map[string]struct{}, 64)
		for _, t := range tokenize(b) {
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			df[t]++
		}
	}
	N := len(bodies)
	idf := make(map[string]float64, len(df))
	for t, d := range df {
		idf[t] = math.Log(float64(N) / float64(d))
	}
	l.cached = idf
	l.cachedN = N
}
