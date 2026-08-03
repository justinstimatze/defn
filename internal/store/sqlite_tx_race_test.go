package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestBeginRollback_ConcurrentWriterCannotObserveRolledBackWrite guards
// the contract Begin()'s doc comment claims: a rolled-back write must
// never be observable afterward, even under concurrent pool access.
//
// Context: a real data-integrity bug was found live 2026-08-03 via
// defn's own running MCP serve (code(op:"apply") with one valid op
// then one deliberately-failing op) -- the tool reported "transaction
// rolled back", but a fresh out-of-process `defn query` against the DB
// file showed the valid op's write had persisted anyway. Adding a
// txMu mutex to Begin() (serializing it against SetMaxOpenConns(4)'s
// other pool connections) made the live repro go away across multiple
// clean reproductions before the fix and multiple clean non-
// reproductions after.
//
// IMPORTANT CAVEAT: this synthetic test does NOT independently prove
// that mechanism -- run against both the pre-fix and post-fix code, it
// PASSES either way (confirmed by temporarily reverting the fix and
// re-running). Whatever precise combination of connections/goroutines
// defn serve has live (summary worker, file watcher, GC ticker,
// concurrent MCP requests) triggers something this test's simpler
// single-background-writer-on-a-different-row pattern doesn't
// reproduce. Keep this test as real, valuable coverage of the stated
// contract on its own merits -- but do not treat a pass here as proof
// the live bug's exact mechanism is fixed. The live before/after
// reproduction via a rebuilt binary is the actual evidence for that.
func TestBeginRollback_ConcurrentWriterCannotObserveRolledBackWrite(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "defn.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mod, err := db.EnsureModule("github.com/foo/race", "race", "")
	if err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}

	target := &Definition{ModuleID: mod.ID, Name: "Target", Kind: "func", Body: "func Target() {}"}
	targetID, err := db.UpsertDefinition(target)
	if err != nil {
		t.Fatalf("seed Target: %v", err)
	}

	const iterations = 100
	const writers = 1

	for i := 0; i < iterations; i++ {
		stopBg := make(chan struct{})
		var wg sync.WaitGroup

		// Background writers hammering the pool directly (not through
		// the tx-scoped view) -- mimics defn serve's own background
		// goroutines (summary worker, file watcher) writing concurrently
		// while an apply's tx is open.
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				bg := &Definition{ModuleID: mod.ID, Name: "Bg", Kind: "func", Body: "func Bg() {}"}
				for {
					select {
					case <-stopBg:
						return
					default:
						_, _ = db.UpsertDefinition(bg)
						time.Sleep(time.Millisecond)
					}
				}
			}(w)
		}

		tx, _, rollback, err := db.Begin()
		if err != nil {
			close(stopBg)
			wg.Wait()
			t.Fatalf("iter %d: Begin: %v", i, err)
		}
		d, err := tx.GetDefinitionByName("Target", "")
		if err != nil {
			rollback()
			close(stopBg)
			wg.Wait()
			t.Fatalf("iter %d: GetDefinitionByName: %v", i, err)
		}
		d.Body = "func Target() { /* SHOULD_BE_ROLLED_BACK */ }"
		// Transient SQLITE_BUSY from the concurrent background writer is
		// expected contention, not the bug under test -- retry the tx's
		// own write a few times before giving up.
		var upsertErr error
		for attempt := 0; attempt < 20; attempt++ {
			if _, upsertErr = tx.UpsertDefinition(d); upsertErr == nil {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if upsertErr != nil {
			rollback()
			close(stopBg)
			wg.Wait()
			t.Fatalf("iter %d: tx UpsertDefinition: %v", i, upsertErr)
		}

		rollback() // deliberately never commit

		close(stopBg)
		wg.Wait()

		fresh, err := db.GetDefinitionByName("Target", "")
		if err != nil {
			t.Fatalf("iter %d: fresh GetDefinitionByName: %v", i, err)
		}
		if fresh.ID != targetID {
			t.Fatalf("iter %d: fresh lookup returned wrong def id %d, want %d", i, fresh.ID, targetID)
		}
		if fresh.Body != "func Target() {}" {
			t.Fatalf("iter %d: rolled-back write leaked into DB: body = %q", i, fresh.Body)
		}
	}
}
