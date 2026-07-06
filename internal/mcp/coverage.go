package mcp

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
)

// coverageRange is one profiled statement block from a `go test -coverprofile`
// output file: "file:startline.startcol,endline.endcol numstmts count".
type coverageRange struct {
	file               string
	startLine, endLine int
	count              int
}

// parseCoverProfile reads a Go coverprofile file into per-block coverage counts.
func parseCoverProfile(path string) ([]coverageRange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ranges []coverageRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		loc := fields[0]
		sep := strings.LastIndex(loc, ":")
		if sep < 0 {
			continue
		}
		file := loc[:sep]
		posPart := loc[sep+1:] // "12.3,15.4"
		commaIdx := strings.Index(posPart, ",")
		if commaIdx < 0 {
			continue
		}
		startLine, err := strconv.Atoi(strings.SplitN(posPart[:commaIdx], ".", 2)[0])
		if err != nil {
			continue
		}
		endLine, err := strconv.Atoi(strings.SplitN(posPart[commaIdx+1:], ".", 2)[0])
		if err != nil {
			continue
		}
		ranges = append(ranges, coverageRange{file: file, startLine: startLine, endLine: endLine, count: count})
	}
	return ranges, sc.Err()
}

// moduleRootPath reads the module directive from go.mod so coverprofile file
// paths (which are import-style, e.g. "github.com/x/y/internal/foo/foo.go")
// can be stripped down to defn's project-relative source_file convention.
func moduleRootPath(projectDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module")), nil
		}
	}
	return "", fmt.Errorf("module directive not found in go.mod")
}

// errBranchCovered heuristically checks whether a covered line inside d falls
// within an "if err != nil" block, by locating the string in the stored body
// and mapping its offset back to an absolute file line. A syntactic heuristic,
// not an AST-precise one — good enough for an advisory, non-gating signal.
func errBranchCovered(db *store.DB, d store.Definition, covered []coverageRange) bool {
	full, err := db.GetDefinition(d.ID)
	if err != nil || full.Body == "" {
		return false
	}
	body := full.Body
	const needle = "err != nil"
	for idx := strings.Index(body, needle); idx >= 0; {
		absLine := d.StartLine + strings.Count(body[:idx], "\n")
		for _, r := range covered {
			if absLine >= r.startLine && absLine <= r.endLine {
				return true
			}
		}
		next := strings.Index(body[idx+len(needle):], needle)
		if next < 0 {
			break
		}
		idx = idx + len(needle) + next
	}
	return false
}

// recordCoverageFacts parses a coverprofile from a `test` run and records
// per-definition behavior_facts: "covered" for any definition with at least
// one exercised line, "error-branch-hit" when a covered line falls inside an
// "if err != nil" block. Returns the number of facts recorded.
func recordCoverageFacts(db *store.DB, projectDir, coverFile string) (int, error) {
	ranges, err := parseCoverProfile(coverFile)
	if err != nil {
		return 0, fmt.Errorf("parse coverprofile: %w", err)
	}
	modRoot, err := moduleRootPath(projectDir)
	if err != nil {
		return 0, fmt.Errorf("module root: %w", err)
	}

	runRef := "uncommitted"
	if log, logErr := db.Log(1); logErr == nil && len(log) > 0 {
		if hash, ok := log[0]["hash"].(string); ok && hash != "" {
			runRef = hash
		}
	}

	byFile := map[string][]coverageRange{}
	for _, r := range ranges {
		if r.count == 0 {
			continue
		}
		rel := strings.TrimPrefix(r.file, modRoot+"/")
		if rel == r.file {
			continue // not part of this module
		}
		byFile[rel] = append(byFile[rel], r)
	}

	var facts []store.BehaviorFact
	for rel, coveredInFile := range byFile {
		defs, dErr := db.FindDefinitionsBySourceFile(rel)
		if dErr != nil {
			continue
		}
		for _, d := range defs {
			if d.StartLine <= 0 || d.EndLine <= 0 {
				continue
			}
			overlaps := false
			for _, r := range coveredInFile {
				if r.startLine <= d.EndLine && r.endLine >= d.StartLine {
					overlaps = true
					break
				}
			}
			if !overlaps {
				continue
			}
			facts = append(facts, store.BehaviorFact{DefID: d.ID, Kind: "covered", RunRef: runRef})
			if errBranchCovered(db, d, coveredInFile) {
				facts = append(facts, store.BehaviorFact{DefID: d.ID, Kind: "error-branch-hit", RunRef: runRef})
			}
		}
	}

	if len(facts) == 0 {
		return 0, nil
	}
	if err := db.AddBehaviorFacts(facts); err != nil {
		return 0, err
	}
	return len(facts), nil
}
