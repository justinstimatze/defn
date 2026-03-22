// Package lint runs Go linters on emitted files and remaps diagnostics
// back to defn definitions. The emitted files are throwaway — all fixes
// must be applied to definitions in the database.
package lint

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/justinstimatze/defn/internal/emit"
	"github.com/justinstimatze/defn/internal/store"
)

// Diagnostic is a single lint finding mapped back to a defn definition.
type Diagnostic struct {
	Module    string // e.g. "github.com/justinstimatze/defn/internal/store"
	DefName   string // e.g. "Open"
	DefKind   string // e.g. "function"
	DefID     int64
	Linter    string // e.g. "errcheck"
	Message   string
	BodyLine  int // 1-based line within the definition body
}

// String formats a diagnostic in defn terms.
func (d Diagnostic) String() string {
	loc := fmt.Sprintf("%s.%s", lastComponent(d.Module), d.DefName)
	if d.BodyLine > 0 {
		loc += fmt.Sprintf(":%d", d.BodyLine)
	}
	if d.Linter != "" {
		return fmt.Sprintf("%s: %s: %s", loc, d.Linter, d.Message)
	}
	return fmt.Sprintf("%s: %s", loc, d.Message)
}

// Run emits files to a temp directory, runs golangci-lint, and remaps
// the output back to defn definitions.
func Run(db *store.DB) ([]Diagnostic, error) {
	tmpDir, err := os.MkdirTemp("", "defn-lint-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Emit all definitions to temp dir.
	locs, err := emit.EmitWithMap(db, tmpDir)
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	// Build a lookup: file path + line number → DefLocation.
	locIndex := buildLocIndex(locs)

	// Run golangci-lint on the emitted directory.
	raw, err := runLinter(tmpDir)
	if err != nil && len(raw) == 0 {
		return nil, fmt.Errorf("golangci-lint: %w", err)
	}

	// Parse and remap diagnostics.
	return remap(raw, locIndex, tmpDir), nil
}

// locKey is a file path used to look up definitions.
type locKey = string

// buildLocIndex builds a map from emitted file path to sorted DefLocations.
func buildLocIndex(locs []emit.DefLocation) map[locKey][]emit.DefLocation {
	idx := make(map[locKey][]emit.DefLocation)
	for _, loc := range locs {
		idx[loc.File] = append(idx[loc.File], loc)
	}
	return idx
}

// lintLine matches golangci-lint output: path:line:col: message (linter)
var lintLine = regexp.MustCompile(`^(.+?):(\d+):(\d+):\s+(.+?)(?:\s+\((\w+)\))?$`)

func runLinter(dir string) ([]string, error) {
	cmd := exec.Command("golangci-lint", "run", "./...")
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()

	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, err
}

func remap(lines []string, locIndex map[locKey][]emit.DefLocation, tmpDir string) []Diagnostic {
	var diags []Diagnostic

	for _, line := range lines {
		m := lintLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		filePath := m[1]
		lineNum, _ := strconv.Atoi(m[2])
		message := m[4]
		linter := m[5]

		// golangci-lint paths are relative to the dir it ran in.
		absPath := filepath.Join(tmpDir, filePath)

		locs, ok := locIndex[absPath]
		if !ok {
			// Can't map — emit as-is with file path.
			diags = append(diags, Diagnostic{
				Linter:  linter,
				Message: fmt.Sprintf("%s (unmapped: %s:%d)", message, filePath, lineNum),
			})
			continue
		}

		// Find which definition this line falls within.
		loc := findDef(locs, lineNum)
		if loc == nil {
			diags = append(diags, Diagnostic{
				Linter:  linter,
				Message: fmt.Sprintf("%s (unmapped: %s:%d)", message, filePath, lineNum),
			})
			continue
		}

		diags = append(diags, Diagnostic{
			Module:   loc.Module,
			DefName:  loc.DefName,
			DefKind:  loc.Kind,
			DefID:    loc.DefID,
			Linter:   linter,
			Message:  message,
			BodyLine: lineNum - loc.StartLine + 1,
		})
	}

	return diags
}

// findDef returns the DefLocation that contains the given line number,
// or nil if the line falls outside any definition.
func findDef(locs []emit.DefLocation, line int) *emit.DefLocation {
	for i := range locs {
		if line >= locs[i].StartLine && line <= locs[i].EndLine {
			return &locs[i]
		}
	}
	return nil
}

func lastComponent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}
