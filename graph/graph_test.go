package graph

import (
	"testing"
)

func TestEmptyGraph(t *testing.T) {
	g := NewGraph(nil, nil, map[string]int64{}, map[int64]string{})

	if g.DefCount() != 0 {
		t.Errorf("expected 0 defs, got %d", g.DefCount())
	}
	if d := g.GetDef("nonexistent", ""); d != nil {
		t.Errorf("expected nil, got %v", d)
	}
	if len(g.CallerFiles("foo.go", 1)) != 0 {
		t.Error("expected no callers")
	}
	if len(g.Tests(999)) != 0 {
		t.Error("expected no tests")
	}
}

func TestGraphQueries(t *testing.T) {
	g := NewGraph(
		[]*Def{
			{ID: 1, Name: "Foo", Kind: "function", SourceFile: "foo.go", ModuleID: 1, Exported: true},
			{ID: 2, Name: "Bar", Kind: "function", SourceFile: "bar.go", ModuleID: 1, Exported: true},
			{ID: 3, Name: "TestFoo", Kind: "function", SourceFile: "foo_test.go", ModuleID: 1, Test: true},
			{ID: 4, Name: "Baz", Kind: "method", Receiver: "*Widget", SourceFile: "baz.go", ModuleID: 2, Exported: true},
		},
		[]Ref{
			{FromDef: 2, ToDef: 1, Kind: "call"},
			{FromDef: 3, ToDef: 1, Kind: "call"},
			{FromDef: 3, ToDef: 2, Kind: "call"},
		},
		map[string]int64{"github.com/test/pkg": 1, "github.com/test/other": 2},
		map[int64]string{1: "github.com/test/pkg", 2: "github.com/test/other"},
	)

	if g.CallerFiles("foo.go", 1)["bar.go"] != 1 {
		t.Error("expected 1 caller from bar.go")
	}
	if g.CalleeFiles("foo_test.go", 1)["foo.go"] != 1 {
		t.Error("expected 1 callee in foo.go")
	}
	if d := g.GetDef("Foo", ""); d == nil || d.ID != 1 {
		t.Errorf("expected Foo (id=1), got %v", d)
	}
	if d := g.GetDef("Widget.Baz", ""); d == nil || d.ID != 4 {
		t.Errorf("expected Baz (id=4), got %v", d)
	}
	// CallerDefs / CallerIDs
	callerDefs := g.CallerDefs(1)
	if len(callerDefs) != 2 {
		t.Errorf("expected 2 direct callers of Foo, got %d", len(callerDefs))
	}
	if len(g.CallerIDs(1)) != 2 {
		t.Errorf("expected 2 caller IDs, got %d", len(g.CallerIDs(1)))
	}

	// DefsInFile
	defs := g.DefsInFile("foo.go", 1)
	if len(defs) != 1 || defs[0].Name != "Foo" {
		t.Errorf("DefsInFile(foo.go, 1) = %v, want [Foo]", defs)
	}

	// DefsInFile unscoped (moduleID=0)
	allFoo := g.DefsInFile("foo.go", 0)
	if len(allFoo) != 1 {
		t.Errorf("DefsInFile(foo.go, 0) = %d, want 1", len(allFoo))
	}

	// CallerFiles unscoped
	callerFilesAll := g.CallerFiles("foo.go", 0)
	if callerFilesAll["bar.go"] != 1 {
		t.Error("unscoped CallerFiles missing bar.go")
	}

	if len(g.TransitiveCallers(1)) != 2 {
		t.Errorf("expected 2 transitive callers, got %d", len(g.TransitiveCallers(1)))
	}
	tests := g.Tests(1)
	if len(tests) != 1 || tests[0].Name != "TestFoo" {
		t.Errorf("expected [TestFoo], got %v", tests)
	}
}

func TestDuplicates(t *testing.T) {
	g := NewGraph(
		[]*Def{
			{ID: 1, Name: "Foo", Hash: "abc123", ModuleID: 1},
			{ID: 2, Name: "Bar", Hash: "def456", ModuleID: 1},
			{ID: 3, Name: "Foo", Hash: "abc123", ModuleID: 2}, // same hash as ID 1
			{ID: 4, Name: "Baz", Hash: "ghi789", ModuleID: 2},
		},
		nil,
		map[string]int64{"pkg1": 1, "pkg2": 2},
		map[int64]string{1: "pkg1", 2: "pkg2"},
	)

	dupes := g.Duplicates()
	if len(dupes) != 1 {
		t.Fatalf("expected 1 duplicate hash, got %d", len(dupes))
	}
	if defs := dupes["abc123"]; len(defs) != 2 {
		t.Errorf("expected 2 defs with hash abc123, got %d", len(defs))
	}

	// ByHash
	if defs := g.ByHash("abc123"); len(defs) != 2 {
		t.Errorf("ByHash(abc123) = %d defs, want 2", len(defs))
	}
	if defs := g.ByHash("nonexistent"); len(defs) != 0 {
		t.Errorf("ByHash(nonexistent) = %d defs, want 0", len(defs))
	}
}

func TestLoadMulti(t *testing.T) {
	// Create two graphs and merge them.
	g1 := NewGraph(
		[]*Def{
			{ID: 1, Name: "Foo", Hash: "abc", ModuleID: 1},
			{ID: 2, Name: "Bar", Hash: "def", ModuleID: 1},
		},
		[]Ref{{FromDef: 2, ToDef: 1, Kind: "call"}},
		map[string]int64{"pkg1": 1},
		map[int64]string{1: "pkg1"},
	)
	g2 := NewGraph(
		[]*Def{
			{ID: 1, Name: "Baz", Hash: "abc", ModuleID: 1}, // same hash as g1's Foo
			{ID: 2, Name: "Qux", Hash: "ghi", ModuleID: 1},
		},
		[]Ref{{FromDef: 1, ToDef: 2, Kind: "call"}},
		map[string]int64{"pkg2": 1},
		map[int64]string{1: "pkg2"},
	)

	// Cache both graphs so LoadMulti can find them.
	ClearCache()
	cacheMu.Lock()
	cache["/repo1"] = g1
	cache["/repo2"] = g2
	cacheMu.Unlock()

	merged, err := LoadMulti("/repo1", "/repo2")
	if err != nil {
		t.Fatal(err)
	}

	if merged.DefCount() != 4 {
		t.Errorf("merged defs = %d, want 4", merged.DefCount())
	}
	if merged.RefCount() != 2 {
		t.Errorf("merged refs = %d, want 2", merged.RefCount())
	}
	if merged.ModuleCount() != 2 {
		t.Errorf("merged modules = %d, want 2", merged.ModuleCount())
	}

	// Cross-repo duplicate by hash.
	dupes := merged.Duplicates()
	if len(dupes) != 1 {
		t.Fatalf("expected 1 cross-repo duplicate, got %d", len(dupes))
	}
	if defs := dupes["abc"]; len(defs) != 2 {
		t.Errorf("expected 2 defs with hash 'abc', got %d", len(defs))
	}
}

func TestLoadMultiSameModulePath(t *testing.T) {
	// Two repos with the same module path — should not corrupt the merged graph.
	ClearCache()

	g1 := NewGraph(
		[]*Def{{ID: 1, Name: "Foo", ModuleID: 1, Hash: "aaa"}},
		nil,
		map[string]int64{"github.com/same/pkg": 1},
		map[int64]string{1: "github.com/same/pkg"},
	)
	g2 := NewGraph(
		[]*Def{{ID: 1, Name: "Bar", ModuleID: 1, Hash: "bbb"}},
		nil,
		map[string]int64{"github.com/same/pkg": 1},
		map[int64]string{1: "github.com/same/pkg"},
	)

	cacheMu.Lock()
	cache["/repo1"] = g1
	cache["/repo2"] = g2
	cacheMu.Unlock()

	merged, err := LoadMulti("/repo1", "/repo2")
	if err != nil {
		t.Fatal(err)
	}

	if merged.DefCount() != 2 {
		t.Errorf("expected 2 defs, got %d", merged.DefCount())
	}

	// Both Foo and Bar should be findable.
	if d := merged.GetDef("Foo", ""); d == nil {
		t.Error("Foo not found in merged graph")
	}
	if d := merged.GetDef("Bar", ""); d == nil {
		t.Error("Bar not found in merged graph")
	}

	// GetDef with module path should find defs from BOTH repos.
	if d := merged.GetDef("Foo", "github.com/same/pkg"); d == nil {
		t.Error("Foo not found with module path filter")
	}
	if d := merged.GetDef("Bar", "github.com/same/pkg"); d == nil {
		t.Error("Bar not found with module path filter — second repo lost")
	}
}

func TestConcurrentLoad(t *testing.T) {
	ClearCache()
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			Load("/nonexistent/.defn")
			done <- true
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestInvalidateCache(t *testing.T) {
	ClearCache()
	g := NewGraph(nil, nil, map[string]int64{}, map[int64]string{})
	cacheMu.Lock()
	cache["/test"] = g
	cacheMu.Unlock()

	// Cached — loader shouldn't fire.
	loadOnce("/test", func() (*Graph, error) {
		t.Fatal("should not load")
		return nil, nil
	})

	InvalidateCache("/test")

	// After invalidation — loader fires.
	called := false
	loadOnce("/test", func() (*Graph, error) {
		called = true
		return g, nil
	})
	if !called {
		t.Error("expected loader after invalidation")
	}
}
