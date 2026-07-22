package store

import (
	"crypto/sha256"
	"fmt"
)

// HashBody computes the content hash of a definition body.
func HashBody(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h)
}

// Definition represents a single Go definition (function, type, method, etc.).
type Definition struct {
	ID         int64
	ModuleID   int64
	Name       string
	Kind       string
	Exported   bool
	Test       bool
	Receiver   string
	Signature  string
	Body       string
	Doc        string
	StartLine  int
	EndLine    int
	SourceFile string
	Hash       string
}

// Module represents a Go package/module in the database.
type Module struct {
	ID   int64
	Path string
	Name string
	Doc  string
}

// Reference represents a reference from one definition to another.
type Reference struct {
	FromDef int64
	ToDef   int64
	Kind    string
}

// Import represents an import recorded for a module.
type Import struct {
	ModuleID     int64
	ImportedPath string
	Alias        string
}

// Comment represents a comment or pragma extracted from Go source.
type Comment struct {
	ID         int64
	DefID      *int64 // nil for file-level comments
	DefName    string // name of associated definition (empty if file-level)
	SourceFile string
	Line       int
	Text       string
	Kind       string // "doc", "line", "block", "pragma"
	PragmaKey  string // e.g. "go:generate", "winze:contested"
	PragmaVal  string // rest of line after pragma directive
}

// LiteralField represents a field in a composite literal (e.g. Config{Field: "val"}).
type LiteralField struct {
	ID         int64
	DefID      int64
	DefName    string
	TypeName   string
	FieldName  string
	FieldValue string
	Line       int
}

// UpstreamFingerprint is a row in upstream_fingerprints — a structural
// hash of one definition from a well-known Go module at a tagged version.
type UpstreamFingerprint struct {
	ModulePath  string
	Version     string
	DefName     string
	Kind        string
	Receiver    string
	Fingerprint string
	Signature   string
	Doc         string
}

// Impact computes the blast radius of a definition.
type Impact struct {
	Definition               Definition
	Module                   string
	DirectCallers            []Definition
	InterfaceDispatchCallers []Definition
	TransitiveCount          int
	Tests                    []Definition
	UncoveredBy              int
}

// BodyMatch is a hit from SearchBodiesLike.
type BodyMatch struct {
	Name       string
	Kind       string
	Receiver   string
	SourceFile string
	Line       int
	Snippet    string
}

// TraverseResult holds a definition found during graph traversal.
type TraverseResult struct {
	Definition Definition
	Depth      int
	Path       []string
}

type Mutation struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Receiver string `json:"receiver,omitempty"`
}

type SimulationResult struct {
	Steps           []SimulationStep `json:"steps"`
	TotalMutations  int              `json:"total_mutations"`
	CombinedCallers int              `json:"combined_callers"`
	CombinedTests   int              `json:"combined_tests"`
	TestDensity     float64          `json:"test_density"`
}

type SimulationStep struct {
	Mutation          Mutation `json:"mutation"`
	ProductionCallers int      `json:"production_callers"`
	TestCallers       int      `json:"test_callers"`
	TransitiveCallers int      `json:"transitive_callers"`
	TestCoverage      int      `json:"test_coverage"`
	UncoveredCallers  int      `json:"uncovered_callers"`
	Error             string   `json:"error,omitempty"`
}
