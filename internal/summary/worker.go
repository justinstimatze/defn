package summary

import (
	"context"
	"sync"
)

// defaultQueueDepth caps in-flight summary requests. Overflow is
// dropped (fire-and-forget) — the next mutation to the same def
// re-enqueues, and the read path falls back to full body when no
// summary exists. Sized to buffer a bulk ingest of a small package
// without blocking the ingest goroutine, but not so large that a
// stalled worker OOMs the process.
const defaultQueueDepth = 4096

// Worker owns the summary generation loop. One Worker per Backend;
// a running Worker owns a goroutine that consumes the queue, calls
// backend.Generate, and persists successful results.
//
// Zero value is not usable — construct via NewWorker. The returned
// Worker is inert until Start(); Enqueue is safe to call before
// Start (requests buffer in the channel).
type Worker struct {
	backend   Backend
	persister Persister
	queue     chan Request

	// mu guards state transitions (started/stopped).
	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewWorker constructs a Worker bound to a Backend and Persister.
// depth <= 0 uses defaultQueueDepth.
func NewWorker(b Backend, p Persister, depth int) *Worker {
	if depth <= 0 {
		depth = defaultQueueDepth
	}
	return &Worker{
		backend:   b,
		persister: p,
		queue:     make(chan Request, depth),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Enqueue submits a Request for asynchronous summary generation.
// Non-blocking: drops the request when the queue is full. Safe to
// call from many goroutines. Returns true when the request was
// accepted, false when dropped (caller can log; not a fatal error).
func (w *Worker) Enqueue(r Request) bool {
	select {
	case w.queue <- r:
		return true
	default:
		return false
	}
}

// Start launches the background consumer goroutine. Idempotent —
// calling twice is a no-op. Call Stop to shut down.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return
	}
	w.started = true
	go w.run(ctx)
}

// Stop signals the worker to exit and waits for the background
// goroutine to drain. Safe to call multiple times.
func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return
	}
	select {
	case <-w.stopCh:
		// already closed
	default:
		close(w.stopCh)
	}
	w.mu.Unlock()
	<-w.doneCh
}

// run is the consumer loop. Reads one request at a time in stage 1
// (the Stub backend is trivial); stage 2 will introduce batching to
// amortize API round-trips. Persistence errors are swallowed —
// summaries are advisory metadata, not correctness-critical.
func (w *Worker) run(ctx context.Context) {
	defer close(w.doneCh)
	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		case req := <-w.queue:
			results := w.backend.Generate(ctx, []Request{req})
			for _, res := range results {
				if res.Err != nil || res.OneLine == "" {
					continue
				}
				_ = w.persister.SetDefSummary(res.DefID, toStoreSummary(res))
			}
		}
	}
}
