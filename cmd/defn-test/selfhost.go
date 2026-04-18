// Selfhost test: ingest defn itself into a throwaway DB, emit to a
// tmpdir, and `go build` the emitted tree. Proves the full round-trip
// (ingest → emit → compile) without touching the project's own .defn/,
// so it runs cleanly while an MCP serve is holding that lock.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// findRepoRoot walks up from start until it finds a go.mod whose module
// directive matches wantModule. Lets the selfhost test run from any
// subdirectory of the repo.
func findRepoRoot(start, wantModule string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "module") {
					continue
				}
				// "module X" or "module\tX", optionally trailed by a
				// comment. Fields splits on any whitespace run.
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] == wantModule {
					return dir, nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod with module %q found above %s", wantModule, start)
		}
		dir = parent
	}
}

func runSelfhostTest() workflowResult {
	start := time.Now()
	fmt.Println("=== Selfhost (ingest → emit → go build defn) ===")

	cwd, err := os.Getwd()
	if err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("getwd: %v", err)}
	}
	repoRoot, err := findRepoRoot(cwd, "github.com/justinstimatze/defn")
	if err != nil {
		return workflowResult{name: "selfhost", message: err.Error()}
	}
	fmt.Printf("  repo: %s\n", repoRoot)

	tmp, err := os.MkdirTemp("", "defn-selfhost-*")
	if err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("mkdir: %v", err)}
	}
	defer os.RemoveAll(tmp)

	dbDir := filepath.Join(tmp, "db")
	db, err := store.Open(dbDir)
	if err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("store open: %v", err)}
	}
	defer db.Close()

	if err := ingest.Ingest(db, repoRoot); err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("ingest: %v", err)}
	}
	fmt.Printf("  ingested (%.1fs)\n", time.Since(start).Seconds())

	if err := resolve.Resolve(db, repoRoot); err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("resolve: %v", err)}
	}
	db.Commit("selfhost")
	fmt.Printf("  resolved (%.1fs)\n", time.Since(start).Seconds())

	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("mkdir out: %v", err)}
	}
	if err := emit.Emit(db, outDir); err != nil {
		return workflowResult{name: "selfhost", message: fmt.Sprintf("emit: %v", err)}
	}
	fmt.Printf("  emitted (%.1fs)\n", time.Since(start).Seconds())

	// Verify the emitted tree compiles. Use the module's own vendor/GOPATH
	// — the emit should have copied go.mod/go.sum verbatim, so `go build`
	// in outDir resolves dependencies the same way as the original repo.
	cmd := exec.Command("go", "build", "./cmd/defn/")
	cmd.Dir = outDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		snippet := string(out)
		if len(snippet) > 2000 {
			snippet = snippet[:2000] + "...[truncated]"
		}
		return workflowResult{
			name:    "selfhost",
			message: fmt.Sprintf("go build in %s failed: %v\n%s", outDir, err, snippet),
		}
	}
	fmt.Printf("  built (%.1fs)\n", time.Since(start).Seconds())

	return workflowResult{name: "selfhost", passed: true}
}
