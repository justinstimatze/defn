package store

import (
	"testing"

	_ "modernc.org/sqlite"
)

func TestEdgesAmong_ReturnsOnlyEdgesWithBothEndpointsInSet(t *testing.T) {
	db, err := OpenBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mod, err := db.EnsureModule("testmod", "testmod", "")
	if err != nil {
		t.Fatal(err)
	}
	greet, err := db.UpsertDefinition(&Definition{ModuleID: mod.ID, Name: "Greet", Kind: "function", Body: "func Greet() {}", SourceFile: "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	farewell, err := db.UpsertDefinition(&Definition{ModuleID: mod.ID, Name: "Farewell", Kind: "function", Body: "func Farewell() { Greet() }", SourceFile: "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	main, err := db.UpsertDefinition(&Definition{ModuleID: mod.ID, Name: "main", Kind: "function", Body: "func main() { Farewell() }", SourceFile: "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetManyReferences(map[int64][]Reference{
		farewell: {{ToDef: greet, Kind: "call"}},
		main:     {{ToDef: farewell, Kind: "call"}},
	}); err != nil {
		t.Fatal(err)
	}

	edges, err := db.EdgesAmong([]int64{greet, farewell})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0] != [2]int64{farewell, greet} {
		t.Fatalf("expected exactly the farewell->greet edge (main excluded from the set), got %v", edges)
	}

	all, err := db.EdgesAmong([]int64{greet, farewell, main})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected both edges once main is included in the set, got %v", all)
	}
}

// TestSetDefSummary_PersistsAndRoundTripsCrux confirms the crux column
// actually persists through a real SQLite round-trip -- the summary
// package's own tests only ever cover Result/Request in memory, never the
// storage layer that def_summaries.crux depends on.
func TestSetDefSummary_PersistsAndRoundTripsCrux(t *testing.T) {
	db, err := OpenBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mod, err := db.EnsureModule("testmod", "testmod", "")
	if err != nil {
		t.Fatal(err)
	}
	defID, err := db.UpsertDefinition(&Definition{ModuleID: mod.ID, Name: "F", Kind: "function", Body: "func F() {}", SourceFile: "a.go"})
	if err != nil {
		t.Fatal(err)
	}

	want := &DefSummary{OneLine: "does something", Crux: "\tif x < 0 {\n\t\treturn 0\n\t}", BodyHash: "h1", Model: "claude-haiku-4-5"}
	if err := db.SetDefSummary(defID, want); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetDefSummary(defID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a summary row, got nil")
	}
	if got.Crux != want.Crux {
		t.Errorf("Crux = %q, want %q", got.Crux, want.Crux)
	}
	if got.OneLine != want.OneLine {
		t.Errorf("OneLine = %q, want %q", got.OneLine, want.OneLine)
	}

	// Update path: SetDefSummary again with a different crux must
	// overwrite, not append or leave the old value behind.
	want2 := &DefSummary{OneLine: "does something else", Crux: "", BodyHash: "h2", Model: "claude-haiku-4-5"}
	if err := db.SetDefSummary(defID, want2); err != nil {
		t.Fatal(err)
	}
	got2, err := db.GetDefSummary(defID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Crux != "" {
		t.Errorf("expected the crux to be cleared on update, got %q", got2.Crux)
	}
}
