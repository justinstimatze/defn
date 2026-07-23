package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

// TestReachCache_BFSFromCache validates that a warmed reach cache
// walks the reverse-refs graph correctly (matches what the CTE
// would return). Seeds a small three-def linear chain A→B→C where
// arrows are call direction, and asserts:
//   - reachableCallers(C) = {A, B}
//   - reachableCallers(A) = {}
func TestReachCache_BFSFromCache(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mod, _ := db.EnsureModule("example.com/chain", "chain", "")
	seed := []*store.Definition{
		{ModuleID: mod.ID, Name: "A", Kind: "function", Body: "func A() { B() }"},
		{ModuleID: mod.ID, Name: "B", Kind: "function", Body: "func B() { C() }"},
		{ModuleID: mod.ID, Name: "C", Kind: "function", Body: "func C() {}"},
	}
	ids := map[string]int64{}
	for _, d := range seed {
		d.Hash = store.HashBody(d.Body)
		id, err := db.UpsertDefinition(d)
		if err != nil {
			t.Fatal(err)
		}
		ids[d.Name] = id
	}
	// Wire refs manually: A→B, B→C.
	if err := db.SetReferences(ids["A"], []store.Reference{{FromDef: ids["A"], ToDef: ids["B"], Kind: "call"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetReferences(ids["B"], []store.Reference{{FromDef: ids["B"], ToDef: ids["C"], Kind: "call"}}); err != nil {
		t.Fatal(err)
	}

	rc := newReachCache()

	// C's transitive callers: {A, B}.
	callersOfC, ok := rc.reachableCallers(context.Background(), db, ids["C"])
	if !ok {
		t.Fatal("reachableCallers returned ok=false; cache should have built")
	}
	if len(callersOfC) != 2 {
		t.Errorf("C should have 2 transitive callers, got %d: %v", len(callersOfC), callersOfC)
	}
	set := map[int64]bool{}
	for _, id := range callersOfC {
		set[id] = true
	}
	if !set[ids["A"]] || !set[ids["B"]] {
		t.Errorf("C's callers should be {A, B}; got %v", callersOfC)
	}

	// A has no callers.
	callersOfA, ok := rc.reachableCallers(context.Background(), db, ids["A"])
	if !ok {
		t.Fatal("reachableCallers(A) returned ok=false")
	}
	if len(callersOfA) != 0 {
		t.Errorf("A should have 0 callers, got %d: %v", len(callersOfA), callersOfA)
	}
}

// TestReachCache_InvalidateRebuilds verifies that after a write,
// invalidate() forces a rebuild on the next call — new edges show up.
func TestReachCache_InvalidateRebuilds(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mod, _ := db.EnsureModule("example.com/pair", "pair", "")
	a := &store.Definition{ModuleID: mod.ID, Name: "A", Kind: "function", Body: "func A() {}"}
	b := &store.Definition{ModuleID: mod.ID, Name: "B", Kind: "function", Body: "func B() {}"}
	a.Hash = store.HashBody(a.Body)
	b.Hash = store.HashBody(b.Body)
	aID, _ := db.UpsertDefinition(a)
	bID, _ := db.UpsertDefinition(b)

	rc := newReachCache()

	// Initially no edges → B has no callers.
	callers, _ := rc.reachableCallers(context.Background(), db, bID)
	if len(callers) != 0 {
		t.Errorf("initial B callers: want 0, got %d", len(callers))
	}

	// Add A→B edge, invalidate, expect A in B's callers.
	db.SetReferences(aID, []store.Reference{{FromDef: aID, ToDef: bID, Kind: "call"}})
	rc.invalidate()
	callers, _ = rc.reachableCallers(context.Background(), db, bID)
	if len(callers) != 1 || callers[0] != aID {
		t.Errorf("after invalidate: want B callers=[A]; got %v", callers)
	}
}
