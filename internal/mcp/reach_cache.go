// Package mcp: reachability cache. Task #154.
//
// The recursive-CTE impact (#149) is 46ms on winze's 410-caller Entity —
// already fast. But every impact call pays the SQL round-trip. When the
// model asks about the same or overlapping defs multiple times in a
// session, we can go faster.
//
// Design: cache the reverse-refs adjacency list in memory (to_def →
// [from_defs]). Impact BFS runs in-process against Go maps — sub-ms
// instead of ms. For winze's ~5000 edges the cache is ~40KB.
//
// Invalidation: any write op marks the cache stale via the existing
// handleCode deferred block. Next impact rebuilds by scanning refs
// (~5-20ms for defn/winze-scale trees) before serving.
//
// Fallback: on any error (cache empty, rebuild failure, unknown def)
// we defer to the backend's GetImpact — the CTE-based path preserves
// correctness at the cost of a SQL round-trip.
package mcp

import (
	"context"
	"sync"

	"github.com/justinstimatze/defn/internal/store"
)

type reachCache struct {
	mu       sync.RWMutex
	built    bool
	// revRefs[to_def] = list of from_defs that reference it (i.e., callers).
	// Not including edge kind — impact walks structural back-edges
	// regardless of ref kind for compatibility with GetImpact.
	revRefs map[int64][]int64
}

func newReachCache() *reachCache {
	return &reachCache{}
}

// invalidate drops the cached graph. Next reachable-callers call rebuilds.
func (r *reachCache) invalidate() {
	r.mu.Lock()
	r.built = false
	r.revRefs = nil
	r.mu.Unlock()
}

// rebuild scans the entire refs table and populates the reverse-adj
// list. Called on-demand from reachableCallers when built=false.
// Uses the backend's ad-hoc Query surface — no new interface method.
func (r *reachCache) rebuild(backend store.Backend) error {
	rows, err := backend.Query(`SELECT from_def, to_def FROM refs`)
	if err != nil {
		return err
	}
	rev := make(map[int64][]int64, len(rows))
	for _, row := range rows {
		to, ok := row["to_def"].(int64)
		if !ok {
			continue
		}
		from, ok := row["from_def"].(int64)
		if !ok {
			continue
		}
		rev[to] = append(rev[to], from)
	}
	r.mu.Lock()
	r.revRefs = rev
	r.built = true
	r.mu.Unlock()
	return nil
}

// reachableCallers returns the set of def IDs that transitively call
// defID (excluding defID itself). BFS over the in-memory reverse-adj
// list. Returns (ids, true) on cache hit / successful rebuild;
// returns (_, false) if the cache is genuinely empty (no refs table
// data, or rebuild failed) so callers can fall back to backend CTE.
func (r *reachCache) reachableCallers(ctx context.Context, backend store.Backend, defID int64) ([]int64, bool) {
	r.mu.RLock()
	if !r.built {
		r.mu.RUnlock()
		if err := r.rebuild(backend); err != nil {
			return nil, false
		}
		r.mu.RLock()
	}
	defer r.mu.RUnlock()

	if len(r.revRefs) == 0 {
		return nil, false
	}

	seen := map[int64]bool{defID: true}
	queue := []int64{defID}
	for len(queue) > 0 {
		// Respect context so we don't spin on a huge graph.
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		current := queue[0]
		queue = queue[1:]
		for _, caller := range r.revRefs[current] {
			if !seen[caller] {
				seen[caller] = true
				queue = append(queue, caller)
			}
		}
	}
	out := make([]int64, 0, len(seen)-1)
	for id := range seen {
		if id != defID {
			out = append(out, id)
		}
	}
	return out, true
}
