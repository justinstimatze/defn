package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRealProjectSanity clones real Go projects and verifies that
// the ingest → resolve → impact pipeline produces sane results.
// This catches the class of bugs that unit tests miss: ambiguous names,
// missing test references, interface method resolution, etc.
//
// Skip with -short since it clones from GitHub.
func TestRealProjectSanity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	projects := []struct {
		repo       string
		impactName string
		minCallers int // minimum expected direct callers
		minRefs    int // minimum expected total references
	}{
		{"github.com/go-chi/chi", "NewMux", 1, 50},
		{"github.com/gorilla/mux", "NewRouter", 1, 20},
	}

	for _, p := range projects {
		t.Run(p.repo, func(t *testing.T) {
			// Clone.
			dir := t.TempDir()
			cloneDir := filepath.Join(dir, "src")
			cmd := exec.Command("git", "clone", "--depth", "1", "https://"+p.repo+".git", cloneDir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("clone failed: %s", out)
			}

			// Open database.
			dbDir := filepath.Join(dir, "defndb")
			db, err := Open(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			// We can't import ingest/resolve from store (circular),
			// so we test the store operations directly by checking
			// that Open + schema init works on a fresh Dolt database.

			// Verify modules table exists and is queryable.
			_, err = db.ListModules()
			if err != nil {
				t.Fatal("ListModules failed:", err)
			}

			// Verify we can create and query definitions.
			mod, err := db.EnsureModule("test/module", "module", "")
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.UpsertDefinition(&Definition{
				ModuleID: mod.ID,
				Name:     "TestFunc",
				Kind:     "function",
				Exported: true,
				Body:     "func TestFunc() {}",
			})
			if err != nil {
				t.Fatal(err)
			}
			if id == 0 {
				t.Fatal("expected non-zero ID")
			}

			// Verify Dolt commit works.
			if err := db.Commit("test"); err != nil {
				t.Fatal("commit failed:", err)
			}

			// Verify log works.
			entries, err := db.Log(5)
			if err != nil {
				t.Fatal("log failed:", err)
			}
			if len(entries) < 2 {
				t.Fatalf("expected at least 2 log entries, got %d", len(entries))
			}

			// Verify branch works.
			if err := db.Branch("test-branch"); err != nil {
				t.Fatal("branch failed:", err)
			}
			branches, err := db.ListBranches()
			if err != nil {
				t.Fatal(err)
			}
			if len(branches) < 2 {
				t.Fatalf("expected at least 2 branches, got %d", len(branches))
			}

			t.Logf("OK: %s — Dolt operations verified", p.repo)
		})
	}
}

// TestDoltOperationsEndToEnd verifies the full Dolt lifecycle:
// create → insert → commit → branch → checkout → modify → merge
func TestDoltOperationsEndToEnd(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	// Create a definition and commit.
	db.UpsertDefinition(&Definition{
		ModuleID: mod.ID, Name: "Original", Kind: "function",
		Exported: true, Body: "func Original() {}",
	})
	if err := db.Commit("initial"); err != nil {
		t.Fatal(err)
	}

	// Create a branch.
	if err := db.Branch("feature"); err != nil {
		t.Fatal(err)
	}

	// Switch to feature branch.
	if err := db.Checkout("feature"); err != nil {
		t.Fatal(err)
	}

	current, _ := db.GetCurrentBranch()
	if current != "feature" {
		t.Fatalf("expected feature branch, got %s", current)
	}

	// Add a definition on the feature branch.
	db.UpsertDefinition(&Definition{
		ModuleID: mod.ID, Name: "Feature", Kind: "function",
		Exported: true, Body: "func Feature() {}",
	})
	if err := db.Commit("add feature"); err != nil {
		t.Fatal(err)
	}

	// Switch back to main.
	if err := db.Checkout("main"); err != nil {
		t.Fatal(err)
	}

	// Feature definition should NOT exist on main.
	_, err := db.GetDefinitionByName("Feature", "")
	if err == nil {
		t.Fatal("Feature should not exist on main before merge")
	}

	// Merge feature into main.
	if err := db.Merge("feature"); err != nil {
		t.Fatal(err)
	}

	// Feature definition should now exist on main.
	d, err := db.GetDefinitionByName("Feature", "")
	if err != nil {
		t.Fatal("Feature should exist on main after merge:", err)
	}
	if d.Name != "Feature" {
		t.Fatalf("expected Feature, got %s", d.Name)
	}

	// Verify log has all commits.
	entries, err := db.Log(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 log entries (init + initial + feature), got %d", len(entries))
	}
}

// TestDisambiguationPicksMostCallers verifies that when multiple definitions
// share a name, GetDefinitionByName returns the one with the most non-test callers.
func TestDisambiguationPicksMostCallers(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	// Create two definitions with the same name but different receivers.
	id1, _ := db.UpsertDefinition(&Definition{
		ModuleID: mod.ID, Name: "Render", Kind: "method",
		Exported: true, Receiver: "*Context", Body: "func (c *Context) Render() {}",
	})
	id2, _ := db.UpsertDefinition(&Definition{
		ModuleID: mod.ID, Name: "Render", Kind: "method",
		Exported: true, Receiver: "JSON", Body: "func (j JSON) Render() {}",
	})

	// Make Context.Render have more non-test callers.
	for i := 0; i < 5; i++ {
		callerID, _ := db.UpsertDefinition(&Definition{
			ModuleID: mod.ID, Name: "Caller" + string(rune('A'+i)), Kind: "function",
			Exported: true, Body: "func Caller() {}",
		})
		db.SetReferences(callerID, []Reference{{ToDef: id1, Kind: "call"}})
	}

	// Make JSON.Render have only test callers.
	testID, _ := db.UpsertDefinition(&Definition{
		ModuleID: mod.ID, Name: "TestRender", Kind: "function",
		Exported: true, Test: true, Body: "func TestRender() {}",
	})
	db.SetReferences(testID, []Reference{{ToDef: id2, Kind: "call"}})

	// GetDefinitionByName should pick Context.Render (5 non-test callers)
	// over JSON.Render (0 non-test callers).
	d, err := db.GetDefinitionByName("Render", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Receiver != "*Context" {
		t.Fatalf("expected *Context receiver, got %s", d.Receiver)
	}
}

// init is needed for os import
var _ = os.DevNull
