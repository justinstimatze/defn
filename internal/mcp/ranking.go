package mcp

import (
	"github.com/justinstimatze/defn/internal/rank"
	"github.com/justinstimatze/defn/internal/store"
)

// dbBodySource adapts *store.DB to rank.BodySource. Kept thin — the rank
// package owns the sampling/caching policy, this just forwards.
type dbBodySource struct{ db *store.DB }

func (a dbBodySource) CountDefinitions() (int, error) { return a.db.CountDefinitions() }
func (a dbBodySource) SampleBodies(n int) ([]string, error) {
	return a.db.SampleBodies(n)
}

func newIDF(db *store.DB) *rank.LazyIDF {
	return rank.NewLazyIDF(dbBodySource{db: db})
}
