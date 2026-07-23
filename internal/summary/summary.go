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
	DefID    int64
	OneLine  string
	BodyHash string
	Model    string
	Err      error
}

// Backend produces one-line intent summaries. Implementations:
//   - Stub: returns a synthetic "TODO: <Name>" line. Used for stage 1
//     wiring tests and as the null-object when no real backend is
//     configured.
//   - Haiku (stage 2, not yet implemented): calls Anthropic API.
//
// Generate returns one Result per Request in the same order. Failure
// modes: partial (some Results have Err set), complete (all Err), or
// success (all Err nil). Never returns a slice of different length
// than the input.
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

func (Stub) Name() string { return "stub" }

func (Stub) Generate(_ context.Context, reqs []Request) []Result {
	out := make([]Result, len(reqs))
	for i, r := range reqs {
		out[i] = Result{
			DefID:    r.DefID,
			OneLine:  fmt.Sprintf("TODO: %s", r.Name),
			BodyHash: r.BodyHash,
			Model:    "stub",
		}
	}
	return out
}

// toStoreSummary lifts a Result into the storage-layer type. Kept in
// this file so the summary package owns the translation.
func toStoreSummary(r Result) *store.DefSummary {
	return &store.DefSummary{
		OneLine:  r.OneLine,
		BodyHash: r.BodyHash,
		Model:    r.Model,
	}
}
