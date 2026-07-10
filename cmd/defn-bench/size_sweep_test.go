package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildSweepFileBuilds verifies every sweep-size fixture actually
// compiles. If a size fails to build, the mutation bench at that size
// is unusable — defn's autoEmitAndBuild would reject the state before
// the mutation even runs, or files-mode would edit a broken file.
func TestBuildSweepFileBuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}
	for _, size := range sweepSizes {
		src := buildSweepFile(size)
		lines := strings.Count(src, "\n")
		if lines < 8 {
			t.Errorf("size=%d: only %d lines, expected at least ~10", size, lines)
			continue
		}
		tmp := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module fixture\n\ngo 1.26\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "s.go"), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("size=%d (actual %d lines) failed to build: %v\n%s", size, lines, err, out)
		}
	}
}

// TestBuildSweepFileSizeApprox verifies the LOC target is within
// tolerance so the axis label on the crossover plot isn't a lie.
func TestBuildSweepFileSizeApprox(t *testing.T) {
	for _, size := range sweepSizes {
		src := buildSweepFile(size)
		actual := strings.Count(src, "\n")
		if size >= 50 {
			// Above ~50 LOC, the fixed header should be negligible;
			// require within 25% of target.
			lo, hi := size*3/4, size*5/4
			if actual < lo || actual > hi {
				t.Errorf("size=%d: actual %d LOC outside [%d, %d]", size, actual, lo, hi)
			}
		} else {
			// Small sizes have header floor; just require ≥ 10 LOC.
			if actual < 10 {
				t.Errorf("size=%d: actual %d LOC too small", size, actual)
			}
		}
	}
}
