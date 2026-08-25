// Package summary generates and persists model-generated intent
// summaries for definitions. Task #160.
//
// Design: fire-and-forget. Ingest calls Worker.Enqueue on each
// new/updated def; a background goroutine batches requests and calls
// the configured Backend (Stub for stage 1, Haiku for stage 2).
// Results are written via a Persister (typically store.Backend). A
// failed generation leaves the def with no summary — the next
// mutation re-enqueues. Best-effort: dropped requests when the queue
// is full are silently swallowed (background quality, not correctness).
package summary

import (
	"context"
	"fmt"

	"github.com/justinstimatze/defn/internal/store"
)

// Request is one def-to-summarize handed to a Backend. BodyHash is
// captured at enqueue time so a later staleness check can detect
// edits that landed between enqueue and persistence.
type Request struct {
	DefID      int64
	Name       string
	Kind       string
	Receiver   string
	ModulePath string
	Body       string
	BodyHash   string
}

// Result is one generated summary. Err is non-nil when Generate
// couldn't produce a summary for this def; callers must not persist
// failed results (they'd overwrite a good prior summary with junk).
type Result struct {
	DefID   int64
	OneLine string
	// Crux is the single most load-bearing contiguous span of Body,
	// sliced verbatim from it -- empty when the backend found no single
	// focal span worth calling out (a trivial getter, a plain data
	// holder). Stub never sets this.
	Crux     string
	BodyHash string
	Model    string
	Err      error
}

type Backend interface {
	Generate(ctx context.Context, reqs []Request) []Result
	// Name is the model identifier written into
	// def_summaries.summary_model for provenance and A/B experiments.
	Name() string
}

// Persister is the subset of [store.Backend] the worker uses to write
// results. Kept narrow so summary doesn't pull an import cycle if
// store ever needs to see a summary type.
type Persister interface {
	SetDefSummary(defID int64, s *store.DefSummary) error
}

// Stub is a no-op Backend that returns "TODO: <Name>" for every
// request. Useful for exercising the whole pipeline (worker + wiring
// + read path) before a real model is wired up.
type Stub struct{}

func (Stub) Name() string { return StubModelName }

func (Stub) Generate(_ context.Context, reqs []Request) []Result {
	out := make([]Result, len(reqs))
	for i, r := range reqs {
		out[i] = Result{
			DefID:    r.DefID,
			OneLine:  fmt.Sprintf("TODO: %s", r.Name),
			BodyHash: r.BodyHash,
			Model:    StubModelName,
		}
	}
	return out
}

// toStoreSummary lifts a Result into the storage-layer type. Kept in
// this file so the summary package owns the translation.
func toStoreSummary(r Result) *store.DefSummary {
	return &store.DefSummary{
		OneLine:  r.OneLine,
		Crux:     r.Crux,
		BodyHash: r.BodyHash,
		Model:    r.Model,
	}
}

// StubModelName is the sentinel Result.Model/DefSummary.Model value
// written by Stub. Callers use it to tell a real (if low-quality)
// summary apart from a placeholder that hasn't been backfilled yet --
// see internal/mcp's handleGetDefinition and renderReadNeighborhood.
const StubModelName = "stub"
