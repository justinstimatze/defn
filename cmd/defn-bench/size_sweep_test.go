package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildSweepFileBuilds verifies every sweep-size fixture actually
// compiles for every mutation family. If a size fails to build, the
// bench at that size is unusable — defn's autoEmitAndBuild would
// reject the state before the mutation even runs, or files-mode
// would edit a broken file.
func TestBuildSweepFileBuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}
	builders := map[string]func(int) string{
		"add-import":   buildSweepFile,
		"rename-param": buildSweepRenameParamFile,
	}
	for family, build := range builders {
		for _, size := range sweepSizes {
			src := build(size)
			lines := strings.Count(src, "\n")
			if lines < 8 {
				t.Errorf("%s size=%d: only %d lines, expected at least ~10", family, size, lines)
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
				t.Errorf("%s size=%d (actual %d lines) failed to build: %v\n%s", family, size, lines, err, out)
			}
		}
	}
}

// TestBuildRenameParamFileHasScatteredData confirms the rename-param
// fixture has enough `data` uses in Process for the mutation to have
// non-trivial scope — otherwise files-mode's read-tax wouldn't
// actually scale with LOC.
func TestBuildRenameParamFileHasScatteredData(t *testing.T) {
	for _, size := range sweepSizes {
		src := buildSweepRenameParamFile(size)
		count := strings.Count(src, "data")
		if count < 10 {
			t.Errorf("size=%d: only %d uses of `data`, expected ≥10 for scatter-cost", size, count)
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
