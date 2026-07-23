package summary

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/justinstimatze/defn/internal/store"
)

// TestWorker_StubToPersister exercises the full stage-1 pipeline:
// Enqueue → background goroutine → Backend.Generate → Persister.
// Uses a Stub backend and an in-memory Persister that records every
// SetDefSummary call. Proves the interface boundary end-to-end; the
// store's SQLite path is covered separately in the store package.
func TestWorker_StubToPersister(t *testing.T) {
	p := &mapPersister{seen: map[int64]*store.DefSummary{}}
	w := NewWorker(Stub{}, p, 8)
	w.Start(context.Background())
	t.Cleanup(w.Stop)

	req := Request{DefID: 42, Name: "HandleFoo", BodyHash: "hash-abc"}
	if ok := w.Enqueue(req); !ok {
		t.Fatalf("Enqueue: dropped despite empty queue")
	}

	// Bounded poll rather than fixed sleep so a fast machine doesn't
	// waste wall. 500ms is generous — the stub backend is synchronous
	// and the worker loop is tight.
	deadline := time.Now().Add(500 * time.Millisecond)
	var got *store.DefSummary
	for time.Now().Before(deadline) {
		got = p.get(42)
		if got != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil {
		t.Fatalf("worker did not persist within 500ms")
	}
	if got.OneLine != "TODO: HandleFoo" {
		t.Errorf("OneLine: got %q, want %q", got.OneLine, "TODO: HandleFoo")
	}
	if got.BodyHash != "hash-abc" {
		t.Errorf("BodyHash: got %q, want %q", got.BodyHash, "hash-abc")
	}
	if got.Model != "stub" {
		t.Errorf("Model: got %q, want %q", got.Model, "stub")
	}
}

// TestWorker_EnqueueDropsWhenFull verifies fire-and-forget semantics:
// a full queue drops requests silently rather than blocking the
// producer. Fixes the failure mode where an over-active ingest could
// stall the write path waiting on a slow model.
func TestWorker_EnqueueDropsWhenFull(t *testing.T) {
	// Never start the worker — the queue fills without draining.
	p := &mapPersister{seen: map[int64]*store.DefSummary{}}
	w := NewWorker(Stub{}, p, 2)
	if !w.Enqueue(Request{DefID: 1}) {
		t.Fatal("first Enqueue dropped unexpectedly")
	}
	if !w.Enqueue(Request{DefID: 2}) {
		t.Fatal("second Enqueue dropped unexpectedly")
	}
	if w.Enqueue(Request{DefID: 3}) {
		t.Fatal("third Enqueue should have been dropped (queue depth 2)")
	}
}

// mapPersister records every SetDefSummary call. Guarded by a mutex
// because the worker goroutine races with the test-thread reads.
type mapPersister struct {
	mu   sync.Mutex
	seen map[int64]*store.DefSummary
}

func (m *mapPersister) SetDefSummary(defID int64, s *store.DefSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[defID] = s
	return nil
}

func (m *mapPersister) get(defID int64) *store.DefSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[defID]
}
