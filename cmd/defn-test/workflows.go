// Workflow tests exercise MCP tool operations against real Go projects.
// These simulate the 7 SWE-bench agent operation patterns:
// 1. Navigate (search + read)
// 2. Understand (impact + explain)
// 3. Edit
// 4. Create
// 5. Delete
// 6. Rename
// 7. Verify (test + diff + history)
//
// Run with: go run ./cmd/defn-test -workflows
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

type workflowResult struct {
	name    string
	passed  bool
	message string
}

func runWorkflowTests() {
	fmt.Println("=== Workflow Tests (SWE-bench patterns) ===")

	// Clone chi for workflow tests (small, clean, fast).
	dir, err := os.MkdirTemp("", "defn-workflow-*")
	if err != nil {
		fmt.Println("FAIL: could not create temp dir")
		return
	}
	defer os.RemoveAll(dir)

	cloneDir := filepath.Join(dir, "src")
	cmd := exec.Command("git", "clone", "--depth", "1", "https://github.com/go-chi/chi.git", cloneDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("SKIP: clone failed: %s\n", out)
		return
	}

	dbDir := filepath.Join(dir, "db")
	db, err := store.Open(dbDir)
	if err != nil {
		fmt.Printf("FAIL: open db: %v\n", err)
		return
	}
	defer db.Close()

	// Ingest + resolve.
	if err := ingest.Ingest(db, cloneDir); err != nil {
		fmt.Printf("FAIL: ingest: %v\n", err)
		return
	}
	if err := resolve.Resolve(db, cloneDir); err != nil {
		fmt.Printf("FAIL: resolve: %v\n", err)
		return
	}
	db.Commit("initial")

	var results []workflowResult

	// === 1. NAVIGATE: search + read ===
	results = append(results, testNavigate(db))

	// === 2. UNDERSTAND: impact ===
	results = append(results, testUnderstand(db))

	// === 3. EDIT ===
	results = append(results, testEdit(db))

	// === 4. CREATE ===
	results = append(results, testCreate(db))

	// === 5. DELETE ===
	results = append(results, testDelete(db))

	// === 6. RENAME ===
	results = append(results, testRename(db))

	// === 7. VERIFY: diff + history ===
	results = append(results, testVerify(db))

	// === 8. UNTESTED ===
	results = append(results, testUntested(db))

	// Summary.
	passed := 0
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.passed {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("  %s: %s — %s\n", status, r.name, r.message)
	}
	fmt.Printf("\n=== Workflow Results: %d passed, %d failed ===\n", passed, failed)
}

func testNavigate(db *store.DB) workflowResult {
	// Search for a pattern.
	defs, err := db.FindDefinitions("%Route%")
	if err != nil || len(defs) == 0 {
		return workflowResult{"navigate: search", false, fmt.Sprintf("FindDefinitions failed: %v, got %d results", err, len(defs))}
	}

	// Read a specific definition.
	d, err := db.GetDefinitionByName("NewMux", "")
	if err != nil || d.Body == "" {
		return workflowResult{"navigate: read", false, fmt.Sprintf("GetDefinitionByName failed: %v", err)}
	}

	return workflowResult{"navigate (search + read)", true, fmt.Sprintf("found %d Route* defs, read NewMux (%d bytes)", len(defs), len(d.Body))}
}

func testUnderstand(db *store.DB) workflowResult {
	d, err := db.GetDefinitionByName("NewMux", "")
	if err != nil {
		return workflowResult{"understand: impact", false, fmt.Sprintf("lookup failed: %v", err)}
	}

	impact, err := db.GetImpact(d.ID)
	if err != nil {
		return workflowResult{"understand: impact", false, fmt.Sprintf("GetImpact failed: %v", err)}
	}

	if len(impact.DirectCallers) == 0 {
		return workflowResult{"understand: impact", false, "NewMux has no callers?"}
	}

	return workflowResult{"understand (impact)", true, fmt.Sprintf("%d direct callers, %d transitive, %d tests", len(impact.DirectCallers), impact.TransitiveCount, len(impact.Tests))}
}

func testEdit(db *store.DB) workflowResult {
	d, err := db.GetDefinitionByName("NewMux", "")
	if err != nil {
		return workflowResult{"edit", false, fmt.Sprintf("lookup failed: %v", err)}
	}

	// Add a comment to the body.
	newBody := "// EDITED BY WORKFLOW TEST\n" + d.Body
	d.Body = newBody
	id, err := db.UpsertDefinition(d)
	if err != nil {
		return workflowResult{"edit", false, fmt.Sprintf("UpsertDefinition failed: %v", err)}
	}

	// Verify the edit persisted.
	d2, _ := db.GetDefinition(id)
	if !strings.Contains(d2.Body, "EDITED BY WORKFLOW TEST") {
		return workflowResult{"edit", false, "edit didn't persist"}
	}

	return workflowResult{"edit", true, fmt.Sprintf("edited NewMux (id=%d)", id)}
}

func testCreate(db *store.DB) workflowResult {
	mod, err := db.GetModuleByPath("github.com/go-chi/chi/v5")
	if err != nil {
		// Try without v5.
		mods, _ := db.ListModules()
		if len(mods) > 0 {
			mod = &mods[0]
		}
	}
	if mod == nil {
		return workflowResult{"create", false, "no module found"}
	}

	d := &store.Definition{
		ModuleID:  mod.ID,
		Name:      "WorkflowTestHelper",
		Kind:      "function",
		Exported:  true,
		Body:      "func WorkflowTestHelper() string { return \"hello from workflow test\" }",
		Signature: "func WorkflowTestHelper() string",
	}
	id, err := db.UpsertDefinition(d)
	if err != nil {
		return workflowResult{"create", false, fmt.Sprintf("create failed: %v", err)}
	}

	// Verify it exists.
	d2, err := db.GetDefinitionByName("WorkflowTestHelper", "")
	if err != nil {
		return workflowResult{"create", false, fmt.Sprintf("created but can't find: %v", err)}
	}

	return workflowResult{"create", true, fmt.Sprintf("created WorkflowTestHelper (id=%d) in %s", id, d2.Name)}
}

func testDelete(db *store.DB) workflowResult {
	// Delete the test helper we just created.
	d, err := db.GetDefinitionByName("WorkflowTestHelper", "")
	if err != nil {
		return workflowResult{"delete", false, "WorkflowTestHelper not found"}
	}

	if err := db.DeleteDefinition(d.ID); err != nil {
		return workflowResult{"delete", false, fmt.Sprintf("delete failed: %v", err)}
	}

	// Verify it's gone.
	_, err = db.GetDefinitionByName("WorkflowTestHelper", "")
	if err == nil {
		return workflowResult{"delete", false, "definition still exists after delete"}
	}

	return workflowResult{"delete", true, "deleted WorkflowTestHelper"}
}

func testRename(db *store.DB) workflowResult {
	// Create a temp definition to rename.
	mods, _ := db.ListModules()
	if len(mods) == 0 {
		return workflowResult{"rename", false, "no modules"}
	}

	d := &store.Definition{
		ModuleID: mods[0].ID,
		Name:     "OldName",
		Kind:     "function",
		Exported: true,
		Body:     "func OldName() {}",
	}
	db.UpsertDefinition(d)

	// Now rename it.
	d, _ = db.GetDefinitionByName("OldName", "")
	d.Name = "NewName"
	d.Body = strings.ReplaceAll(d.Body, "OldName", "NewName")
	db.UpsertDefinition(d)

	// Verify new name exists.
	d2, err := db.GetDefinitionByName("NewName", "")
	if err != nil {
		return workflowResult{"rename", false, fmt.Sprintf("renamed but can't find: %v", err)}
	}
	if !strings.Contains(d2.Body, "NewName") {
		return workflowResult{"rename", false, "body not updated"}
	}

	// Clean up.
	db.DeleteDefinition(d2.ID)

	return workflowResult{"rename", true, "renamed OldName → NewName"}
}

func testVerify(db *store.DB) workflowResult {
	// Diff: check for uncommitted changes.
	status, _ := db.Diff() // may be empty or error if nothing changed

	// Commit.
	if err := db.Commit("workflow test changes"); err != nil {
		// OK if nothing to commit.
		_ = err
	}

	// Log: check history.
	log, err := db.Log(5)
	if err != nil {
		return workflowResult{"verify: history", false, fmt.Sprintf("log failed: %v", err)}
	}
	if len(log) < 2 {
		return workflowResult{"verify: history", false, fmt.Sprintf("expected >= 2 commits, got %d", len(log))}
	}

	return workflowResult{"verify (diff + commit + history)", true, fmt.Sprintf("diff: %d changes, log: %d commits", len(status), len(log))}
}

func testUntested(db *store.DB) workflowResult {
	defs, err := db.GetUntested()
	if err != nil {
		return workflowResult{"untested", false, fmt.Sprintf("GetUntested failed: %v", err)}
	}

	return workflowResult{"untested", true, fmt.Sprintf("%d exported definitions without tests", len(defs))}
}
