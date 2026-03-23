// Command defn-test runs integration tests against real Go projects.
// It clones repos, ingests them, and verifies that impact, callers,
// untested, and queries return correct results.
//
// Usage: go run ./cmd/defn-test
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

type project struct {
	repo string // e.g. "github.com/go-chi/chi"
	name string // short name for display

	// Assertions after ingest + resolve:
	minModules int
	minDefs    int
	minRefs    int

	// A known function to test impact on:
	impactName       string
	impactMinCallers int // minimum direct callers
	impactMinTests   int // minimum tests covering it transitively

	// Known callers (at least these should appear):
	knownCallers []string

	// Untested: minimum expected untested exported definitions
	minUntested int
}

var projects = []project{
	{
		repo: "github.com/go-chi/chi", name: "chi",
		minModules: 2, minDefs: 200, minRefs: 200,
		impactName: "NewMux", impactMinCallers: 1, impactMinTests: 0,
		knownCallers: []string{"NewRouter"},
		minUntested:  10,
	},
	{
		repo: "github.com/gorilla/mux", name: "mux",
		minModules: 1, minDefs: 30, minRefs: 20,
		impactName: "NewRouter", impactMinCallers: 0, impactMinTests: 0,
		minUntested: 1,
	},
	{
		repo: "github.com/gin-gonic/gin", name: "gin",
		minModules: 5, minDefs: 1000, minRefs: 2000,
		impactName: "Render", impactMinCallers: 15, impactMinTests: 40,
		knownCallers: []string{"AsciiJSON", "BSON", "HTML", "JSON", "XML", "YAML"},
		minUntested:  50,
	},
	{
		repo: "github.com/BurntSushi/toml", name: "toml",
		minModules: 1, minDefs: 30, minRefs: 10,
		impactName: "Decode", impactMinCallers: 0, impactMinTests: 0,
		minUntested: 0,
	},
}

func main() {
	passed := 0
	failed := 0
	skipped := 0

	for _, p := range projects {
		fmt.Printf("\n=== %s (%s) ===\n", p.name, p.repo)
		start := time.Now()

		err := testProject(p)
		dur := time.Since(start)

		if err != nil {
			if strings.Contains(err.Error(), "clone failed") {
				fmt.Printf("  SKIP: %v (%.1fs)\n", err, dur.Seconds())
				skipped++
			} else {
				fmt.Printf("  FAIL: %v (%.1fs)\n", err, dur.Seconds())
				failed++
			}
		} else {
			fmt.Printf("  PASS (%.1fs)\n", dur.Seconds())
			passed++
		}
	}

	fmt.Printf("\n=== Integration Results: %d passed, %d failed, %d skipped ===\n", passed, failed, skipped)

	// Run workflow tests (SWE-bench patterns).
	fmt.Println()
	runWorkflowTests()

	if failed > 0 {
		os.Exit(1)
	}
}

func testProject(p project) error {
	dir, err := os.MkdirTemp("", "defn-integration-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// Clone.
	cloneDir := filepath.Join(dir, "src")
	cmd := exec.Command("git", "clone", "--depth", "1", "https://"+p.repo+".git", cloneDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("clone failed: %s", strings.TrimSpace(string(out)))
	}
	fmt.Printf("  cloned\n")

	// Open database.
	dbDir := filepath.Join(dir, "defndb")
	db, err := store.Open(dbDir)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	// Ingest.
	if err := ingest.Ingest(db, cloneDir); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	fmt.Printf("  ingested\n")

	// Resolve.
	if err := resolve.Resolve(db, cloneDir); err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	fmt.Printf("  resolved\n")

	// Commit.
	db.Commit("test ingest")

	// --- Assertions ---

	// Module count.
	mods, err := db.ListModules()
	if err != nil {
		return fmt.Errorf("list modules: %w", err)
	}
	if len(mods) < p.minModules {
		return fmt.Errorf("modules: got %d, want >= %d", len(mods), p.minModules)
	}
	fmt.Printf("  modules: %d (>= %d) ✓\n", len(mods), p.minModules)

	// Definition count.
	defs, err := db.FindDefinitions("%")
	if err != nil {
		return fmt.Errorf("find defs: %w", err)
	}
	if len(defs) < p.minDefs {
		return fmt.Errorf("definitions: got %d, want >= %d", len(defs), p.minDefs)
	}
	fmt.Printf("  definitions: %d (>= %d) ✓\n", len(defs), p.minDefs)

	// Reference count.
	refResults, err := db.Query("SELECT COUNT(*) as c FROM `references`")
	if err != nil {
		return fmt.Errorf("count refs: %w", err)
	}
	var refCount int
	if len(refResults) > 0 {
		if c, ok := refResults[0]["c"].(int64); ok {
			refCount = int(c)
		}
	}
	if refCount < p.minRefs {
		return fmt.Errorf("references: got %d, want >= %d", refCount, p.minRefs)
	}
	fmt.Printf("  references: %d (>= %d) ✓\n", refCount, p.minRefs)

	// Impact test.
	if p.impactName != "" {
		d, err := db.GetDefinitionByName(p.impactName, "")
		if err != nil {
			return fmt.Errorf("impact target %q not found: %w", p.impactName, err)
		}
		impact, err := db.GetImpact(d.ID)
		if err != nil {
			return fmt.Errorf("get impact: %w", err)
		}

		if len(impact.DirectCallers) < p.impactMinCallers {
			return fmt.Errorf("impact %s: got %d direct callers, want >= %d",
				p.impactName, len(impact.DirectCallers), p.impactMinCallers)
		}
		fmt.Printf("  impact(%s): %d direct callers (>= %d) ✓\n",
			p.impactName, len(impact.DirectCallers), p.impactMinCallers)

		if len(impact.Tests) < p.impactMinTests {
			return fmt.Errorf("impact %s: got %d tests, want >= %d",
				p.impactName, len(impact.Tests), p.impactMinTests)
		}
		fmt.Printf("  impact(%s): %d tests (>= %d) ✓\n",
			p.impactName, len(impact.Tests), p.impactMinTests)

		// Check known callers.
		callerNames := map[string]bool{}
		for _, c := range impact.DirectCallers {
			callerNames[c.Name] = true
		}
		for _, expected := range p.knownCallers {
			if !callerNames[expected] {
				return fmt.Errorf("impact %s: expected caller %q not found in %v",
					p.impactName, expected, callerNames)
			}
		}
		if len(p.knownCallers) > 0 {
			fmt.Printf("  impact(%s): known callers present ✓\n", p.impactName)
		}
	}

	// Untested count.
	untested, err := db.GetUntested()
	if err != nil {
		return fmt.Errorf("get untested: %w", err)
	}
	if len(untested) < p.minUntested {
		return fmt.Errorf("untested: got %d, want >= %d", len(untested), p.minUntested)
	}
	fmt.Printf("  untested: %d (>= %d) ✓\n", len(untested), p.minUntested)

	// Dolt operations.
	if err := db.Branch("test-branch"); err != nil {
		return fmt.Errorf("branch: %w", err)
	}
	branches, err := db.ListBranches()
	if err != nil {
		return fmt.Errorf("list branches: %w", err)
	}
	if len(branches) < 2 {
		return fmt.Errorf("branches: got %d, want >= 2", len(branches))
	}
	fmt.Printf("  dolt branch/commit/log ✓\n")

	return nil
}
