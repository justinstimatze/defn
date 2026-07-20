package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestVersionEndpoint(t *testing.T) {
	// Route /version through the real mux to cover the method guard
	// and the Content-Type header contract that CLI status depends on.
	mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "defn", Version: Version}, nil)
	srv := httptest.NewServer(mcpHTTPMux(mcpServer, "/tmp/test-project"))
	defer srv.Close()

	// GET returns the version as text/plain.
	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /version status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != Version {
		t.Errorf("body = %q, want %q", got, Version)
	}

	// POST is rejected with 405 + Allow header.
	resp, err = http.Post(srv.URL+"/version", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /version status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, should include GET", allow)
	}
}

func TestIdentityEndpoint(t *testing.T) {
	// /identity must echo the projDir verbatim — cmdServe relies on
	// exact-match comparison (after filepath.Abs) to detect FNV
	// hash collisions between distinct projects.
	mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "defn", Version: Version}, nil)
	wantDir := "/some/abs/project/path"
	srv := httptest.NewServer(mcpHTTPMux(mcpServer, wantDir))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /identity status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != wantDir {
		t.Errorf("body = %q, want %q", got, wantDir)
	}
}

func TestExtractSignature(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"simple func", "func Foo(x int) error { return nil }", "func Foo(x int) error"},
		{"no params", "func Bar() { }", "func Bar()"},
		{"method", "func (c *Context) Render(code int) { }", "func (*Context) Render(code int)"},
		{"multi return", "func Baz() (int, error) { return 0, nil }", "func Baz() (int, error)"},
		{"const", "const MaxRetries = 5", "const MaxRetries"},
		{"var", "var ErrNotFound = errors.New(\"not found\")", "var ErrNotFound"},
		{"type", "type Config struct { Port int }", "type Config"},
		{"interface", "type Reader interface { Read(p []byte) (int, error) }", "type Reader"},
		{"doc comment", "// Foo does stuff.\nfunc Foo() {}", "func Foo()"},
		{"map param", "func Foo(m map[string]interface{}) error { return nil }", "func Foo(m map[string]interface{}) error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSignature(tt.body)
			if got != tt.want {
				t.Errorf("extractSignature(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestAstRename(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		oldName        string
		newName        string
		wantSkipped    int
		wantContain    string
		wantNotContain string
	}{
		{
			name:           "rename function call",
			body:           "func Foo() { Bar() }",
			oldName:        "Bar",
			newName:        "Baz",
			wantContain:    "Baz()",
			wantNotContain: "Bar()",
		},
		{
			name:        "preserve comment",
			body:        "func Foo() {\n\t// Bar is important\n\tBar()\n}",
			oldName:     "Bar",
			newName:     "Baz",
			wantContain: "// Bar is important", // comment preserved
		},
		{
			name:        "preserve string literal",
			body:        "func Foo() { fmt.Println(\"Bar\") }",
			oldName:     "Bar",
			newName:     "Baz",
			wantContain: "\"Bar\"", // string preserved
		},
		{
			name:        "skip local var declaration",
			body:        "func Foo() { Bar := 1; _ = Bar }",
			oldName:     "Bar",
			newName:     "Baz",
			wantSkipped: 1, // := declaration skipped, usage renamed
		},
		{
			name:        "skip param declaration",
			body:        "func Foo(Bar int) { _ = Bar }",
			oldName:     "Bar",
			newName:     "Baz",
			wantSkipped: 1, // param decl skipped, usage renamed
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, skipped := astRename(tt.body, tt.oldName, tt.newName)
			if tt.wantSkipped > 0 && skipped != tt.wantSkipped {
				t.Errorf("skipped = %d, want %d", skipped, tt.wantSkipped)
			}
			if tt.wantContain != "" && !strings.Contains(result, tt.wantContain) {
				t.Errorf("result missing %q:\n%s", tt.wantContain, result)
			}
			if tt.wantNotContain != "" && strings.Contains(result, tt.wantNotContain) {
				t.Errorf("result should not contain %q:\n%s", tt.wantNotContain, result)
			}
		})
	}
}

func TestInferFromBody(t *testing.T) {
	s := &server{}
	tests := []struct {
		body     string
		wantName string
		wantKind string
		wantRecv string
		wantTest bool
	}{
		{"func Foo() {}", "Foo", "function", "", false},
		{"func (c *Context) Render() {}", "Render", "method", "*Context", false},
		{"func TestFoo(t *testing.T) {}", "TestFoo", "function", "", true},
		{"func BenchmarkBar(b *testing.B) {}", "BenchmarkBar", "function", "", true},
		{"type Config struct {}", "Config", "type", "", false},
		{"type Reader interface { Read() }", "Reader", "interface", "", false},
		{"const MaxRetries = 5", "MaxRetries", "const", "", false},
		{"var ErrNotFound = errors.New(\"x\")", "ErrNotFound", "var", "", false},
		{"// Doc comment\nfunc Foo() {}", "Foo", "function", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.wantName, func(t *testing.T) {
			name, kind, recv, isTest := s.inferFromBody(tt.body)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
			if recv != tt.wantRecv {
				t.Errorf("receiver = %q, want %q", recv, tt.wantRecv)
			}
			if isTest != tt.wantTest {
				t.Errorf("isTest = %v, want %v", isTest, tt.wantTest)
			}
		})
	}
}

func TestFormatReceiver(t *testing.T) {
	tests := []struct{ recv, want string }{
		{"", ""},
		{"*Context", "(*Context)."},
		{"Context", "(Context)."},
	}
	for _, tt := range tests {
		got := formatReceiver(tt.recv)
		if got != tt.want {
			t.Errorf("formatReceiver(%q) = %q, want %q", tt.recv, got, tt.want)
		}
	}
}

func TestHandleCodeValidation(t *testing.T) {
	s := &server{db: nil} // handlers will fail on DB access but validation runs first

	tests := []struct {
		name    string
		args    codeParam
		wantErr string
	}{
		{"read missing name", codeParam{Op: "read"}, "name is required"},
		{"edit missing name", codeParam{Op: "edit", NewBody: "func X() {}"}, "name is required"},
		{"edit missing body", codeParam{Op: "edit", Name: "X"}, "new_body"},
		// Fragment edit passes validation (OldFragment is set, name is set) — skip, needs real DB.
		{"rename missing old", codeParam{Op: "rename", NewName: "Y"}, "old_name is required"},
		{"rename missing new", codeParam{Op: "rename", OldName: "X"}, "new_name is required"},
		{"move missing module", codeParam{Op: "move", Name: "X"}, "module is required"},
		{"query missing sql", codeParam{Op: "query"}, "sql is required"},
		{"insert missing after", codeParam{Op: "insert", Name: "X", Body: "code"}, "after is required"},
		{"insert missing body", codeParam{Op: "insert", Name: "X", After: "anchor"}, "body is required"},
		{"unknown op", codeParam{Op: "nonexistent"}, "unknown op"},
		{"whitespace name", codeParam{Op: "read", Name: "  "}, "name is required"},
		{"read-file missing file", codeParam{Op: "read-file"}, "file is required"},
		{"replace-hunk missing name", codeParam{Op: "replace-hunk", Old: "x", New: "y"}, "name is required"},
		{"replace-hunk missing old", codeParam{Op: "replace-hunk", Name: "F", New: "y"}, "old is required"},
		{"replace-hunk missing new", codeParam{Op: "replace-hunk", Name: "F", Old: "x"}, "new is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _, _ := s.handleCode(context.Background(), nil, tt.args)
			if tt.wantErr == "" {
				return // just checking it doesn't panic on validation
			}
			if result == nil {
				t.Fatal("expected error result, got nil")
			}
			text := resultText(t, result)
			if !strings.Contains(strings.ToLower(text), strings.ToLower(tt.wantErr)) {
				t.Errorf("error = %q, want to contain %q", text, tt.wantErr)
			}
		})
	}
}

func setupTestDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create a minimal Go project for ingestion.
	projDir := filepath.Join(dir, "testproj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

// Greet returns a greeting.
func Greet(name string) string {
	return "Hello, " + name
}

// Farewell says goodbye.
func Farewell(name string) string {
	return Greet(name) + " and goodbye"
}

func main() {
	Farewell("world")
}
`), 0644)
	os.WriteFile(filepath.Join(projDir, "main_test.go"), []byte(`package main

import "testing"

func TestGreet(t *testing.T) {
	if Greet("x") == "" {
		t.Fatal("empty")
	}
}

func TestFarewell(t *testing.T) {
	if Farewell("x") == "" {
		t.Fatal("empty")
	}
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	return db, projDir
}

func TestHandleEmit(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{db: db, projectDir: projDir}

	// Relative path resolves against projDir.
	outRel := filepath.Join("out-rel")
	result, _, _ := s.handleEmit(context.Background(), nil, codeParam{Out: outRel})
	text := resultText(t, result)
	if !strings.Contains(text, "Emitted") {
		t.Fatalf("expected success message, got: %s", text)
	}
	// Verify the emitted file exists and has Greet/Farewell.
	data, err := os.ReadFile(filepath.Join(projDir, outRel, "main.go"))
	if err != nil {
		t.Fatalf("read emitted file: %v", err)
	}
	if !strings.Contains(string(data), "func Greet(") {
		t.Errorf("emitted main.go missing Greet:\n%s", data)
	}
	if !strings.Contains(string(data), "func Farewell(") {
		t.Errorf("emitted main.go missing Farewell:\n%s", data)
	}

	// Absolute paths also work.
	outAbs := t.TempDir()
	result, _, _ = s.handleEmit(context.Background(), nil, codeParam{Out: outAbs})
	if !strings.Contains(resultText(t, result), "Emitted") {
		t.Fatalf("absolute emit failed: %s", resultText(t, result))
	}
	if _, err := os.Stat(filepath.Join(outAbs, "main.go")); err != nil {
		t.Fatalf("absolute emit didn't write main.go: %v", err)
	}
}

func TestHandleEmitRequiresOut(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{Op: "emit"})
	text := resultText(t, result)
	if !strings.Contains(text, "out is required") {
		t.Errorf("expected 'out is required' error, got: %s", text)
	}
}

func TestHandleImpact(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in impact output")
	}
	if !strings.Contains(text, "Direct callers") || !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell as a caller of Greet")
	}
}

func TestHandleImpact_Rank(t *testing.T) {
	// rank=true must not panic, must not lose callers, and must keep
	// the formatted output coherent. Score ordering is exercised
	// directly in internal/rank — here we just verify the wire-up.
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}
	s.idf = newIDF(db)

	result, _, err := s.handleImpact(context.Background(), nil, codeParam{Name: "Greet", Rank: true})
	if err != nil {
		t.Fatalf("rank=true impact: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in ranked impact output")
	}
	if !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell still present after ranking")
	}
}

func TestHandleRead(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Hello") {
		t.Error("expected function body containing 'Hello'")
	}

	// Structured usage: op labeled, bytes non-zero, alt bytes populated
	// from file_sources (ingest wrote main.go there).
	u, ok := result.StructuredContent.(usageStats)
	if !ok {
		t.Fatalf("expected StructuredContent = usageStats, got %T", result.StructuredContent)
	}
	if u.Op != "read" {
		t.Errorf("usage.Op = %q, want %q", u.Op, "read")
	}
	if u.BytesReturned == 0 {
		t.Error("usage.BytesReturned should be > 0")
	}
	if u.BytesAltRead == 0 {
		t.Error("usage.BytesAltRead should be > 0 (file_sources not populated?)")
	}
}

// TestHandleRead_UpstreamMatch seeds an upstream fingerprint whose hash
// matches the local Greet body exactly, then verifies the read op
// returns the compact provenance form (no body, tagged with version).
func TestHandleRead_UpstreamMatch(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	// Pull the local Greet so we can hash its exact body.
	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	hash := store.HashBodyStructural(d.Body)

	if err := db.InsertUpstreamFingerprint(store.UpstreamFingerprint{
		ModulePath:  "testproj",
		Version:     "v1.2.3",
		DefName:     "Greet",
		Kind:        "function",
		Receiver:    "",
		Fingerprint: hash,
		Signature:   "func Greet(name string) string",
		Doc:         "Greet returns a greeting.",
	}); err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "v1.2.3") {
		t.Errorf("expected upstream version tag, got: %s", text)
	}
	if !strings.Contains(text, "unchanged from upstream") {
		t.Errorf("expected provenance tag, got: %s", text)
	}
	if strings.Contains(text, "\"Hello, \"") {
		t.Errorf("expected body to be elided in compact form, got: %s", text)
	}
	if !strings.Contains(text, "full: true") {
		t.Errorf("expected hint about full:true, got: %s", text)
	}

	// full:true should bypass the compact form and return the body.
	fullResult, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet", Full: true})
	fullText := resultText(t, fullResult)
	if !strings.Contains(fullText, "\"Hello, \"") {
		t.Errorf("full:true should include body, got: %s", fullText)
	}
}

// TestHandleRead_UpstreamDivergence seeds an upstream row whose hash
// does NOT match the local body — the read op should return the full
// body with a divergence note (helpful when the user has patched a
// dep locally).
func TestHandleRead_UpstreamDivergence(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	if err := db.InsertUpstreamFingerprint(store.UpstreamFingerprint{
		ModulePath:  "testproj",
		Version:     "v1.2.3",
		DefName:     "Greet",
		Kind:        "function",
		Receiver:    "",
		Fingerprint: "different-hash-does-not-match",
		Signature:   "func Greet(name string) string",
		Doc:         "Greet returns a greeting.",
	}); err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "diverges from all known upstream versions") {
		t.Errorf("expected divergence note, got: %s", text)
	}
	if !strings.Contains(text, "v1.2.3") {
		t.Errorf("expected known version listed, got: %s", text)
	}
	if !strings.Contains(text, "\"Hello, \"") {
		t.Errorf("divergence path should include full body, got: %s", text)
	}
}

// TestHandleRead_UnknownModule verifies that a def whose module has
// no upstream_fingerprints rows falls through to the current body-in-fence
// behavior unchanged.
func TestHandleRead_UnknownModule(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "\"Hello, \"") {
		t.Errorf("expected full body in output for unknown module, got: %s", text)
	}
	if strings.Contains(text, "unchanged from upstream") {
		t.Errorf("no upstream rows exist — should not be tagged as unchanged, got: %s", text)
	}
	if strings.Contains(text, "diverges from") {
		t.Errorf("no upstream rows exist — should not be tagged as divergent, got: %s", text)
	}
}

func TestHandleReadFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, err := s.handleReadFile(context.Background(), nil, codeParam{File: "main.go"})
	if err != nil {
		t.Fatalf("read-file: %v", err)
	}
	text := resultText(t, result)

	// Both defs from main.go should appear, bodies included.
	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in output")
	}
	if !strings.Contains(text, "Farewell") {
		t.Error("expected Farewell in output")
	}
	if !strings.Contains(text, "Hello, ") {
		t.Error("expected Greet body ('Hello, ') in output")
	}
	if !strings.Contains(text, "goodbye") {
		t.Error("expected Farewell body ('goodbye') in output")
	}
	// Source-order: Greet (line 4) before Farewell (line 9).
	gi := strings.Index(text, "Greet")
	fi := strings.Index(text, "Farewell")
	if gi < 0 || fi < 0 || gi > fi {
		t.Errorf("expected Greet before Farewell in source order, got Greet@%d Farewell@%d", gi, fi)
	}

	u, ok := result.StructuredContent.(usageStats)
	if !ok {
		t.Fatalf("expected StructuredContent = usageStats, got %T", result.StructuredContent)
	}
	if u.Op != "read-file" {
		t.Errorf("usage.Op = %q, want %q", u.Op, "read-file")
	}
	if u.BytesReturned == 0 {
		t.Error("usage.BytesReturned should be > 0")
	}
}

func TestHandleReadFile_MissingFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleReadFile(context.Background(), nil, codeParam{File: "does-not-exist.go"})
	text := resultText(t, result)
	if !strings.Contains(text, "no definitions found") {
		t.Errorf("expected 'no definitions found' error, got: %s", text)
	}
}

// TestHandleExpand_BodyAndCallers exercises expand's happy path — one call
// returns body + callers in one tool_result. Attacks the N² cache-read
// problem by killing the read → impact → read multi-turn pattern.
func TestHandleExpand_BodyAndCallers(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "Greet",
		Include: []string{"body", "callers"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)

	if !strings.Contains(text, "### body") {
		t.Errorf("expected body section header, got: %s", text)
	}
	if !strings.Contains(text, "Hello, ") {
		t.Errorf("expected Greet body ('Hello, '), got: %s", text)
	}
	if !strings.Contains(text, "### callers") {
		t.Errorf("expected callers section header, got: %s", text)
	}
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected Farewell as a caller of Greet, got: %s", text)
	}
	// Test callers should be marked _(test)_.
	if !strings.Contains(text, "TestGreet") {
		t.Errorf("expected TestGreet as a (test) caller of Greet, got: %s", text)
	}

	u, ok := result.StructuredContent.(usageStats)
	if !ok {
		t.Fatalf("expected usageStats StructuredContent, got %T", result.StructuredContent)
	}
	if u.Op != "expand" {
		t.Errorf("usage.Op = %q, want %q", u.Op, "expand")
	}
	if u.BytesReturned == 0 {
		t.Error("usage.BytesReturned should be > 0")
	}
}

// TestHandleExpand_DefaultInclude verifies empty include:[] defaults to
// [body, callers] — the pair we picked as the MVP default.
func TestHandleExpand_DefaultInclude(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### body") {
		t.Errorf("default include should include body, got: %s", text)
	}
	if !strings.Contains(text, "### callers") {
		t.Errorf("default include should include callers, got: %s", text)
	}
}

// TestHandleExpand_UnknownIncludeKind ensures unsupported include kinds
// are ignored with a note (learn-the-vocabulary affordance) rather than
// erroring the whole request.
func TestHandleExpand_UnknownIncludeKind(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, err := s.handleExpand(context.Background(), nil, codeParam{
		Name:    "Greet",
		Include: []string{"body", "callers", "types-used"},
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "### body") {
		t.Error("expected body section")
	}
	if !strings.Contains(text, "types-used") {
		t.Error("expected note about the unsupported kind")
	}
}

// TestHandleFileDefs_RootLevelFile is the regression for the bug that
// let handleFileDefs miss defs when the file is at the module root and
// the module path did not contain the file stem (e.g. module "testproj"
// + file "main.go" — the old code stripped .go and searched for a "main"
// dir substring, which never matches "testproj"). Fixed by mirroring
// handleReadFile's dir="" pattern for bare filenames.
func TestHandleFileDefs_RootLevelFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, err := s.handleFileDefs(context.Background(), nil, codeParam{File: "main.go"})
	if err != nil {
		t.Fatalf("file-defs: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Greet") {
		t.Errorf("expected Greet in file-defs output, got: %s", text)
	}
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected Farewell in file-defs output, got: %s", text)
	}
}

// TestHandleAddImportRootFile regresses a bug where handleAddImport
// treated a root-level file (no "/") as its own directory when
// looking up its module, so FindDefinitionsByFile searched
// m.path LIKE '%main.go%' — which never matched the module path
// (e.g. "testproj"). The lookup returned zero defs and add-import
// errored with "no definitions found in file X — cannot resolve
// module", making callers thrash for many retries in benches.
func TestHandleAddImportRootFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleAddImport(context.Background(), nil, codeParam{
		File:       "main.go",
		ImportPath: "hash/fnv",
	})
	text := resultText(t, result)
	if strings.Contains(text, "no definitions found") {
		t.Fatalf("add-import failed on root-level file: %s", text)
	}
	if !strings.Contains(text, "added import") && !strings.Contains(text, "already present") {
		t.Errorf("expected success indicator in output, got: %s", text)
	}
}

func TestHandleEdit(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	newBody := `func Greet(name string) string {
	return "Hi, " + name
}`
	result, _, _ := s.handleEdit(context.Background(), nil, editParam{Name: "Greet", NewBody: newBody})
	text := resultText(t, result)

	if !strings.Contains(text, "Updated") {
		t.Error("expected 'Updated' in edit response")
	}

	// Verify the change persisted.
	d, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Body, "Hi, ") {
		t.Error("body not updated")
	}
}

func TestHandleEditSyntaxValidation(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleEdit(context.Background(), nil, editParam{
		Name:    "Greet",
		NewBody: "func Greet() { syntax error here",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "syntax error") {
		t.Errorf("expected syntax error, got: %s", text)
	}
}

func TestHandleFragmentEdit(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "Hello",
		NewFragment: "Hey",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Edited") {
		t.Errorf("expected 'Edited', got: %s", text)
	}

	d, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(d.Body, "Hey") {
		t.Error("fragment not replaced")
	}
}

func TestHandleFragmentEditNotFound(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "nonexistent text",
		NewFragment: "x",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "not found") {
		t.Errorf("expected 'not found', got: %s", text)
	}
}

func TestHandleFragmentEditEmpty(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleFragmentEdit(context.Background(), nil, codeParam{
		Name:        "Greet",
		OldFragment: "",
		NewFragment: "x",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "cannot be empty") {
		t.Errorf("expected 'cannot be empty', got: %s", text)
	}
}

func TestHandleInsert(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleInsert(context.Background(), nil, codeParam{
		Name:  "Greet",
		After: "Hello",
		Body:  " there",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Inserted") {
		t.Errorf("expected 'Inserted', got: %s", text)
	}

	d, _ := db.GetDefinitionByName("Greet", "")
	if !strings.Contains(d.Body, "Hello there") {
		t.Error("insert not applied")
	}
}

func TestHandleSearch(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleSearch(context.Background(), nil, codeParam{Pattern: "%Greet%"})
	text := resultText(t, result)

	if !strings.Contains(text, "Greet") {
		t.Error("expected Greet in search results")
	}
}

func TestHandleCreate(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func NewHelper() string { return \"help\" }",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Created") {
		t.Errorf("expected 'Created', got: %s", text)
	}
}

// Bug C: op:create with multi-decl body and NO file: must reject — the
// caller has no way to say where the defs land.
func TestHandleCreateRejectsMultiDeclWithoutFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	body := `const Limit = 10

func Helper() int { return Limit }

func Other() int { return 0 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{Body: body})
	text := resultText(t, result)

	if !strings.Contains(text, "top-level declarations") {
		t.Errorf("expected multi-decl rejection error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Helper", ""); err == nil {
		t.Error("Helper should not have been created when body was rejected")
	}
	if _, err := db.GetDefinitionByName("Other", ""); err == nil {
		t.Error("Other should not have been created when body was rejected")
	}
}

// Multi-def file authoring: with file: set, a multi-decl body should
// upsert each decl as its own Definition sharing the same SourceFile,
// running a single autoEmit+build. This is the write-granularity fix
// motivated by 2026-07-11 turns.txt trajectory analysis.
func TestHandleCreateMultiDeclWithFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	body := `// Limit is the max requests per second.
const Limit = 10

// Bucket tracks tokens.
type Bucket struct {
	tokens int
}

// NewBucket seeds a Bucket.
func NewBucket(n int) *Bucket {
	return &Bucket{tokens: n}
}

// Take drains a token.
func (b *Bucket) Take() bool {
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "main.go",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Created 4 defs") {
		t.Fatalf("expected 'Created 4 defs', got: %s", text)
	}
	for _, name := range []string{"Limit", "Bucket", "NewBucket", "Take"} {
		if !strings.Contains(text, name) {
			t.Errorf("expected %s in summary, got: %s", name, text)
		}
	}

	for _, want := range []struct{ Name, Kind string }{
		{"Limit", "const"},
		{"Bucket", "type"},
		{"NewBucket", "function"},
		{"Take", "method"},
	} {
		d, err := db.GetDefinitionByName(want.Name, "")
		if err != nil {
			t.Errorf("%s not found: %v", want.Name, err)
			continue
		}
		if d.Kind != want.Kind {
			t.Errorf("%s.Kind = %q, want %q", want.Name, d.Kind, want.Kind)
		}
		if d.SourceFile != "main.go" {
			t.Errorf("%s.SourceFile = %q, want main.go", want.Name, d.SourceFile)
		}
		if want.Name == "Take" && d.Receiver != "*Bucket" {
			t.Errorf("Take.Receiver = %q, want *Bucket", d.Receiver)
		}
		if !strings.Contains(d.Body, want.Name) {
			t.Errorf("%s body missing name: %q", want.Name, d.Body)
		}
	}
}

// If any name in a multi-decl body collides with an existing def, the
// whole batch must be rejected — no partial creates.
func TestHandleCreateMultiDeclRejectsNameCollision(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	// Seed one def whose name will collide.
	if _, _, err := s.handleCreate(context.Background(), nil, createParam{
		Body: "func Existing() int { return 1 }",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	body := `func Fresh() int { return 2 }

func Existing() int { return 3 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "main.go",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Fresh", ""); err == nil {
		t.Error("Fresh must not have been created (collision aborts batch)")
	}
}

// The model naturally writes whole-file bodies beginning with `package
// foo` when asked to author a new file. Multi-decl create must strip
// the leading package decl instead of choking on the resulting duplicate
// package clause. Regression from the 2026-07-17 probe.
func TestHandleCreateMultiDeclStripsLeadingPackage(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	body := `package multitest

// Alpha runs.
func Alpha() int { return 1 }

// Beta runs.
func Beta() int { return 2 }

// Gamma runs.
func Gamma() int { return 3 }`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "main.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created 3 defs") {
		t.Fatalf("expected 'Created 3 defs' (with leading package stripped), got: %s", text)
	}
	for _, name := range []string{"Alpha", "Beta", "Gamma"} {
		if _, err := db.GetDefinitionByName(name, ""); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

// Regression: model naturally writes bodies that start with `package X`
// followed by `import (...)` — that import block was tripping sliceDecls
// with "could not infer name (kind=*ast.GenDecl)". Fix skips import
// GenDecls silently; goimports re-adds them at emit from usage.
func TestHandleCreateMultiDeclSkipsImportBlock(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	body := `package middleware

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter guards.
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string]time.Time
}

// Allow reports whether the request may proceed.
func (r *RateLimiter) Allow(req *http.Request) bool {
	return true
}`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "middleware/ratelimit.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created 2 defs") {
		t.Fatalf("expected 'Created 2 defs' (import block skipped), got: %s", text)
	}
	for _, name := range []string{"RateLimiter", "Allow"} {
		if _, err := db.GetDefinitionByName(name, ""); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

// If the body contains ONLY an import block (no code decls), sliceDecls
// must error out with a helpful message rather than silently succeeding.
func TestHandleCreateMultiDeclImportsOnlyFails(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	body := `package middleware

import (
	"net/http"
	"sync"
)`

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: body,
		File: "middleware/ratelimit.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "no top-level declarations found") &&
		!strings.Contains(text, "couldn't infer definition name") {
		t.Fatalf("expected error about no decls, got: %s", text)
	}
}

// Bug C: op:create with file: param must route the new def into that file
// (SourceFile populated on the stored Definition).
func TestHandleCreateHonorsFileParam(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func PlacedHere() int { return 1 }",
		File: "main.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "Created") {
		t.Fatalf("expected 'Created', got: %s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("expected file path in output, got: %s", text)
	}

	d, err := db.GetDefinitionByName("PlacedHere", "")
	if err != nil {
		t.Fatalf("def not found: %v", err)
	}
	if d.SourceFile != "main.go" {
		t.Errorf("SourceFile = %q, want %q", d.SourceFile, "main.go")
	}
}

// Bug C: when file: maps to no known module, return an error rather than
// silently falling back to the first module.
func TestHandleCreateRejectsUnknownFile(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	result, _, _ := s.handleCreate(context.Background(), nil, createParam{
		Body: "func Nope() int { return 0 }",
		File: "no/such/package/file.go",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "does not map to any known module") {
		t.Errorf("expected unknown-module error, got: %s", text)
	}
	if _, err := db.GetDefinitionByName("Nope", ""); err == nil {
		t.Error("Nope should not have been created when file is unknown")
	}
}

func TestHandleRename(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}
	s.ready.Store(true) // setupTestDB ingest+resolve is synchronous; skip the wait

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "Greet",
		NewName: "SayHi",
	})
	text := resultText(t, result)

	if !strings.Contains(text, "Renamed") {
		t.Errorf("expected 'Renamed', got: %s", text)
	}

	// Verify caller was updated too.
	d, _ := db.GetDefinitionByName("Farewell", "")
	if !strings.Contains(d.Body, "SayHi") {
		t.Error("caller not updated after rename")
	}
}

func resultText(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("nil or empty result")
	}
	for _, c := range result.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}

func TestTopLevelFlow(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "empty body",
			body: "",
			want: nil,
		},
		{
			name: "not parseable",
			body: "not go source",
			want: nil,
		},
		{
			name: "simple return",
			body: "func F() int { return 42 }",
			want: []string{"L0:return"},
		},
		{
			name: "err check pattern",
			body: `func F() error {
	x, err := doThing()
	if err != nil {
		return err
	}
	return nil
}`,
			want: []string{"L1:assign", "L2:if", "L5:return"},
		},
		{
			name: "loop + defer + go",
			body: `func F() {
	defer cleanup()
	go bg()
	for i := 0; i < 10; i++ {
		process(i)
	}
	select {
	case <-ch:
	}
}`,
			want: []string{"L1:defer", "L2:go", "L3:for", "L6:select"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := topLevelFlow(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("length: got %d %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHandleOutline_SmallBodyFallsBackToRead(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	// Greet's body is well under outlineBodyThreshold (300 bytes) so
	// the size-aware fallback should return the read view — which
	// includes the full body inside a fenced code block.
	result, _, _ := s.handleOutline(context.Background(), nil, nameParam{Name: "Greet"})
	text := resultText(t, result)

	if !strings.Contains(text, "Hello, ") {
		t.Errorf("expected small body to fall back to read view (should include body content); got:\n%s", text)
	}
	if strings.Contains(text, "Body:") && strings.Contains(text, "fetch with") {
		t.Errorf("expected fallback to read, but output looks like outline (has 'Body: ... fetch with'):\n%s", text)
	}
}

func TestHandleOutline_LargeBodyReturnsOutline(t *testing.T) {
	// Build a project with one large function that trips the outline
	// threshold, to exercise the outline branch (not the small-body
	// fallback). setupTestDB's Greet/Farewell are both too small.
	db, projDir := setupTestDB(t)
	defer db.Close()

	// Overwrite main.go with a chunkier function that will comfortably
	// exceed outlineBodyThreshold (300 bytes) and has a mix of stmts
	// for the flow section to detect.
	big := `package main

// Chunky processes items with a mix of control-flow shapes so the
// outline op's flow detection has something interesting to report.
// Body is padded past outlineBodyThreshold via repeated statements.
func Chunky(items []string) (int, error) {
	total := 0
	for _, item := range items {
		if item == "" {
			continue
		}
		total++
	}
	if total == 0 {
		return 0, nil
	}
	defer func() {
		total = 0
	}()
	go func() {
		process(items)
	}()
	select {
	case <-done:
	}
	return total, nil
}

func process(_ []string) {}

var done = make(chan struct{})

func main() {}
`
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(big), 0644)
	os.Remove(filepath.Join(projDir, "main_test.go"))

	// Re-ingest.
	if _, err := ingest.IngestFile(db, projDir, filepath.Join(projDir, "main.go")); err != nil {
		t.Fatal("re-ingest:", err)
	}

	s := &server{db: db, projectDir: projDir}
	result, _, _ := s.handleOutline(context.Background(), nil, nameParam{Name: "Chunky"})
	text := resultText(t, result)

	// Outline output must NOT contain the body statements — that would
	// mean we fell through to read despite the body being large enough.
	if strings.Contains(text, "continue") || strings.Contains(text, "total++") {
		t.Errorf("expected outline (no body content); got read-shaped output:\n%s", text)
	}
	// Must contain the outline-specific lines.
	for _, want := range []string{"Body:", "fetch with", "Callers:", "Callees"} {
		if !strings.Contains(text, want) {
			t.Errorf("outline missing %q in:\n%s", want, text)
		}
	}
	// Flow section must be present and list at least one recognized
	// statement kind from the fixture.
	if !strings.Contains(text, "Flow (") {
		t.Errorf("outline missing Flow section:\n%s", text)
	}
	for _, kind := range []string{"range", "if", "defer", "go", "select", "return", "assign"} {
		if !strings.Contains(text, kind) {
			t.Errorf("flow section missing %q kind:\n%s", kind, text)
		}
	}
}

func TestHandleSlice_MissingArgs(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	// Missing name.
	result, _, _ := s.handleSlice(context.Background(), nil, codeParam{Slice: "return"})
	if !strings.Contains(resultText(t, result), "name is required") {
		t.Errorf("expected 'name is required' error")
	}

	// Missing slice kind.
	result, _, _ = s.handleSlice(context.Background(), nil, codeParam{Name: "Greet"})
	if !strings.Contains(resultText(t, result), "kind is required") {
		t.Errorf("expected 'kind is required' error")
	}
}

func TestHandleSlice_ReturnStmt(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{db: db}

	// Greet has one return statement.
	result, _, _ := s.handleSlice(context.Background(), nil, codeParam{Name: "Greet", Slice: "return"})
	text := resultText(t, result)

	if !strings.Contains(text, "return") {
		t.Errorf("expected return keyword in slice output:\n%s", text)
	}
	if !strings.Contains(text, `"Hello, "`) {
		t.Errorf("expected return expression content:\n%s", text)
	}
	if !strings.Contains(text, "slice: return, 1 match") {
		t.Errorf("expected match count header:\n%s", text)
	}
}

// TestHandleRename_EmitsOnlyNewName regression test for the chain-bench
// failure surfaced on 2026-07-08: rename left the OLD def name in the
// emitted file alongside the new one because the emit path treats the
// old on-disk decl as untracked. Same shape as the delete-race fixed in
// b274ccc; the rename fix (this file) passes the old qualified name
// through emit.Opts.AllowedRemovals.
func TestHandleRename_EmitsOnlyNewName(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{db: db, projectDir: projDir}
	s.ready.Store(true) // setupTestDB ingest+resolve is synchronous; skip the wait

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "Greet",
		NewName: "SayHi",
	})
	if result == nil {
		t.Fatal("nil result")
	}

	final, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(final)
	if strings.Contains(src, "func Greet(") {
		t.Errorf("emitted main.go still contains old def:\n%s", src)
	}
	if !strings.Contains(src, "func SayHi(") {
		t.Errorf("emitted main.go missing new def:\n%s", src)
	}
	if !strings.Contains(src, "SayHi(name)") {
		t.Errorf("emitted main.go missing updated caller (should call SayHi):\n%s", src)
	}
	if strings.Contains(src, "Greet(name)") {
		t.Errorf("emitted main.go still has old caller reference:\n%s", src)
	}
}

// TestHandleRename_SurvivesReopen synthetic repro of the chain-bench
// failure. Mirrors what the bench does step-for-step, but with NO MCP
// goroutines (no watchFiles, no ingestAndResolve at startup) and NO
// claude -p — just the store/emit/handler layer in a plain Go test.
//
// The bench fails: rename returns success, but a later `defn query`
// (fresh connection) shows the OLD name still on the original id AND
// a stray new id for the new name. This test isolates whether the bug
// requires MCP-level concurrency or reproduces in pure code paths.
//
// Bisect logic: if this test FAILS, the bug is in store/emit —
// something about Dolt session persistence, working-set commit, or
// journal flush. If this test PASSES, the bug requires goroutine
// concurrency (watchFiles polling, background ingestAndResolve, etc.).
func TestHandleRename_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")

	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, projDir, "go.mod", "module fixture\n\ngo 1.26\n")
	writeFixtureFile(t, projDir, "core.go", `package fixture

// ProcessData is the core operation.
func ProcessData(x int) int {
	return x * 2
}
`)
	writeFixtureFile(t, projDir, "caller_a.go", `package fixture

func RunA(x int) int {
	return ProcessData(x) + 1
}
`)
	writeFixtureFile(t, projDir, "caller_b.go", `package fixture

func RunB(x int) int {
	total := 0
	for i := 0; i < x; i++ {
		total += ProcessData(i)
	}
	return total
}
`)

	// --- FIRST SESSION: ingest, rename, close ---
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := ingest.Ingest(db1, projDir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := resolve.Resolve(db1, projDir); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := db1.Commit("initial ingest"); err != nil {
		t.Fatalf("commit initial: %v", err)
	}

	s := &server{db: db1, projectDir: projDir}
	s.ready.Store(true)
	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "ProcessData",
		NewName: "Handle",
	})
	if result == nil {
		t.Fatal("nil result from handleRename")
	}
	if txt := resultText(t, result); !strings.Contains(txt, "Renamed") {
		t.Fatalf("expected Renamed, got: %s", txt)
	}
	if err := db1.Close(); err != nil {
		t.Logf("db1.Close error (non-fatal): %v", err)
	}

	// --- SECOND SESSION: reopen, query — does the rename survive? ---
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer db2.Close()

	defs, err := db2.FilterDefinitions("", "function", "", 0)
	if err != nil {
		t.Fatalf("filter defs: %v", err)
	}
	names := make(map[string]int)
	for _, d := range defs {
		if d.Test {
			continue
		}
		names[d.Name]++
	}
	if names["ProcessData"] > 0 {
		t.Errorf("post-reopen DB still has ProcessData (should have been renamed to Handle): %v", names)
	}
	if names["Handle"] != 1 {
		t.Errorf("post-reopen DB should have exactly one Handle def, got: %v", names)
	}

	// Also verify on-disk core.go doesn't have both.
	if data, err := os.ReadFile(filepath.Join(projDir, "core.go")); err == nil {
		src := string(data)
		if strings.Contains(src, "func ProcessData(") {
			t.Errorf("core.go still contains func ProcessData:\n%s", src)
		}
		if !strings.Contains(src, "func Handle(") {
			t.Errorf("core.go missing func Handle:\n%s", src)
		}
	}
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestHandleRename_SurvivesReopen_WithBackgroundIngest layers the MCP
// startup pattern onto the synthetic repro: fires
// `go s.ingestAndResolve()` right before handleRename, mirroring what
// newMCPServer does. If the bench-level failure reproduces here, the
// bug is the goroutine race (ingest writing to Dolt's session state
// while handleRename is doing its work); if this ALSO passes, the bug
// requires either watchFiles polling or the specific shutdown path
// defn serve → RunShared → SIGTERM triggers.
func TestHandleRename_SurvivesReopen_WithBackgroundIngest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	projDir := filepath.Join(dir, "testproj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, projDir, "go.mod", "module fixture\n\ngo 1.26\n")
	writeFixtureFile(t, projDir, "core.go", `package fixture

// ProcessData is the core operation.
func ProcessData(x int) int {
	return x * 2
}
`)
	writeFixtureFile(t, projDir, "caller_a.go", `package fixture

func RunA(x int) int {
	return ProcessData(x) + 1
}
`)
	writeFixtureFile(t, projDir, "caller_b.go", `package fixture

func RunB(x int) int {
	total := 0
	for i := 0; i < x; i++ {
		total += ProcessData(i)
	}
	return total
}
`)

	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	// Seed the DB the way defn ingest CLI does (before serve starts).
	if err := ingest.Ingest(db1, projDir); err != nil {
		t.Fatalf("initial ingest: %v", err)
	}
	if err := resolve.Resolve(db1, projDir); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if err := db1.Commit("initial ingest"); err != nil {
		t.Fatalf("commit initial: %v", err)
	}

	// Now spin up a server the way newMCPServer does: fire the async
	// startup ingest+resolve, then serve requests.
	s := &server{db: db1, projectDir: projDir}
	go func() {
		if err := s.ingestAndResolve(); err != nil {
			t.Logf("startup ingestAndResolve: %v", err)
		}
		s.ready.Store(true)
	}()

	result, _, _ := s.handleRename(context.Background(), nil, renameParam{
		OldName: "ProcessData",
		NewName: "Handle",
	})
	if result == nil {
		t.Fatal("nil result from handleRename")
	}

	// Give any lingering goroutine time to potentially clobber state.
	time.Sleep(200 * time.Millisecond)

	if err := db1.Close(); err != nil {
		t.Logf("db1.Close error (non-fatal): %v", err)
	}

	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer db2.Close()

	defs, err := db2.FilterDefinitions("", "function", "", 0)
	if err != nil {
		t.Fatalf("filter defs: %v", err)
	}
	names := make(map[string]int)
	for _, d := range defs {
		if d.Test {
			continue
		}
		names[d.Name]++
	}
	if names["ProcessData"] > 0 {
		t.Errorf("post-reopen DB still has ProcessData (should have been renamed to Handle): %v", names)
	}
	if names["Handle"] != 1 {
		t.Errorf("post-reopen DB should have exactly one Handle def, got: %v", names)
	}
}

func TestTruncateTestOutput(t *testing.T) {
	// Small output passes through untouched.
	small := "=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\nok  \tpkg\t0.001s\n"
	if got := truncateTestOutput(small); got != small {
		t.Errorf("small output should pass through; got altered")
	}

	// Large output gets summarized: head + failures preserved, middle dropped.
	var large strings.Builder
	for i := 0; i < 200; i++ {
		large.WriteString(fmt.Sprintf("=== RUN   TestFoo_%d\n", i))
		large.WriteString("    foo_test.go:10: some noisy output that would inflate wire cost\n")
		if i == 100 {
			large.WriteString("--- FAIL: TestFoo_100 (0.01s)\n")
		} else {
			large.WriteString(fmt.Sprintf("--- PASS: TestFoo_%d (0.00s)\n", i))
		}
	}
	large.WriteString("FAIL\tpkg\t0.500s\n")
	large.WriteString("FAIL\n")
	got := truncateTestOutput(large.String())
	if len(got) >= len(large.String()) {
		t.Errorf("expected truncation, got same-or-larger length %d vs %d", len(got), len(large.String()))
	}
	if !strings.Contains(got, "--- FAIL: TestFoo_100") {
		t.Errorf("truncated output must preserve failed-test names, got:\n%s", got)
	}
	if !strings.Contains(got, "FAIL\tpkg\t0.500s") {
		t.Errorf("truncated output must preserve package-level result, got:\n%s", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncated output must include truncation marker, got:\n%s", got)
	}

	// All-pass large output still gets summarized but marker says no failures.
	var allPass strings.Builder
	for i := 0; i < 200; i++ {
		allPass.WriteString(fmt.Sprintf("=== RUN   TestFoo_%d\n--- PASS: TestFoo_%d (0.00s)\n", i, i))
	}
	allPass.WriteString("PASS\nok  \tpkg\t0.500s\n")
	got = truncateTestOutput(allPass.String())
	if !strings.Contains(got, "no failures") {
		t.Errorf("all-pass truncation should say 'no failures', got:\n%s", got)
	}
	if !strings.Contains(got, "ok  \tpkg\t0.500s") {
		t.Errorf("all-pass truncation must preserve package result, got:\n%s", got)
	}
}

func TestSearchShapedSQLRedirect(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		wantHint  string
		wantEmpty bool
	}{
		// Body grep (the cli-3461 anti-pattern)
		{"body_like", "SELECT d.name FROM definitions d JOIN bodies b ON b.def_id = d.id WHERE b.body LIKE '%api/v3%'", "op:\"search\"", false},
		{"body_like_lower", "select * from bodies where body like '%foo%'", "op:\"search\"", false},

		// Direct name lookup
		{"name_eq", "SELECT * FROM definitions WHERE name = 'GetJobs'", "op:\"read\"", false},
		{"d_name_eq", "SELECT d.name, b.body FROM definitions d JOIN bodies b ON b.def_id=d.id WHERE d.name = 'GetJobs'", "op:\"read\"", false},

		// Schema introspection
		{"show_tables", "SHOW TABLES", "schema is documented", false},
		{"describe", "DESCRIBE bodies", "schema is documented", false},
		{"info_schema", "SELECT * FROM INFORMATION_SCHEMA.COLUMNS", "schema is documented", false},

		// Legitimate analytics — should pass through
		{"count_by_kind", "SELECT `kind`, COUNT(*) FROM definitions GROUP BY `kind`", "", true},
		{"orphan_refs", "SELECT * FROM refs WHERE target_id NOT IN (SELECT id FROM definitions)", "", true},
		{"file_scan", "SELECT DISTINCT source_file FROM definitions", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := searchShapedSQLRedirect(tt.sql)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("expected pass-through, got redirect: %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected redirect containing %q, got empty (SQL not intercepted)", tt.wantHint)
			}
			if !strings.Contains(got, tt.wantHint) {
				t.Errorf("redirect missing hint %q, got: %q", tt.wantHint, got)
			}
		})
	}
}
