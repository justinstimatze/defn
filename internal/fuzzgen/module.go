package fuzzgen

import (
	"fmt"
	"os"
	"path/filepath"
)

// NewModule creates an empty module with a single trivial main package,
// so every generated module is guaranteed non-empty and buildable even
// before any hazard runs.
func NewModule(modPath string) *SyntheticModule {
	return &SyntheticModule{
		ModPath: modPath,
		Files: map[string]string{
			"main.go": "package main\n\nfunc main() {}\n",
		},
	}
}

// AddFile registers a file, used by hazards. Panics on a duplicate path --
// two hazards claiming the same path is a generator bug, not a
// fuzz-worthy condition.
func (m *SyntheticModule) AddFile(path, source string) {
	if _, exists := m.Files[path]; exists {
		panic(fmt.Sprintf("fuzzgen: duplicate file path %q", path))
	}
	m.Files[path] = source
}

// WriteTo materializes the module under dir: a go.mod plus every file.
func (m *SyntheticModule) WriteTo(dir string) error {
	goMod := fmt.Sprintf("module %s\n\ngo 1.21\n", m.ModPath)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		return fmt.Errorf("write go.mod: %w", err)
	}
	for path, source := range m.Files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", path, err)
		}
		if err := os.WriteFile(full, []byte(source), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// SyntheticModule is a generated, deterministic Go module tree used to
// stress-test defn's ingest->emit round trip. Files are plain source text
// rather than go/ast nodes: hazard functions compose it directly via
// string templates, and syntactic validity is checked for free by simply
// building it before handing it to ingest -- a generator bug surfaces
// immediately as "go build failed" on our OWN fixture, never mistaken
// for a defn corruption bug.
type SyntheticModule struct {
	ModPath string
	// Files maps a module-relative, slash-separated path (e.g.
	// "pkg/alpha/types.go") to its full source text.
	Files map[string]string
}
