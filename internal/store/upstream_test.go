package store

import (
	"path/filepath"
	"testing"
)

func TestUpstreamFingerprints_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	n, err := db.CountUpstreamFingerprints()
	if err != nil {
		t.Fatalf("count on empty: %v", err)
	}
	if n != 0 {
		t.Errorf("fresh DB should have 0 upstream rows, got %d", n)
	}

	rows := []UpstreamFingerprint{
		{
			ModulePath:  "github.com/go-chi/chi/v5",
			Version:     "v5.1.0",
			DefName:     "Mux.ServeHTTP",
			Kind:        "method",
			Receiver:    "*Mux",
			Fingerprint: "abc123",
			Signature:   "func (mx *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request)",
			Doc:         "ServeHTTP dispatches to the routing tree.",
		},
		{
			ModulePath:  "github.com/go-chi/chi/v5",
			Version:     "v5.1.0",
			DefName:     "NewRouter",
			Kind:        "function",
			Receiver:    "",
			Fingerprint: "def456",
			Signature:   "func NewRouter() *Mux",
			Doc:         "NewRouter returns a new Mux router.",
		},
	}
	if err := db.InsertUpstreamFingerprints(rows); err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err = db.CountUpstreamFingerprints()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows after insert, got %d", n)
	}

	// Match: correct module/def/kind/receiver + matching fingerprint returns the row.
	match, err := db.FindUpstreamMatch("github.com/go-chi/chi/v5", "Mux.ServeHTTP", "method", "*Mux", "abc123")
	if err != nil {
		t.Fatalf("find match: %v", err)
	}
	if match == nil {
		t.Fatal("expected match, got nil")
	}
	if match.Version != "v5.1.0" {
		t.Errorf("version = %q, want v5.1.0", match.Version)
	}

	// Miss: wrong fingerprint returns nil.
	miss, err := db.FindUpstreamMatch("github.com/go-chi/chi/v5", "Mux.ServeHTTP", "method", "*Mux", "wrong-hash")
	if err != nil {
		t.Fatalf("find miss: %v", err)
	}
	if miss != nil {
		t.Errorf("expected nil for wrong fingerprint, got %+v", miss)
	}

	// Miss: unknown module returns nil.
	unknown, err := db.FindUpstreamMatch("github.com/unknown/pkg", "Foo", "function", "", "any")
	if err != nil {
		t.Fatalf("find unknown: %v", err)
	}
	if unknown != nil {
		t.Errorf("expected nil for unknown module, got %+v", unknown)
	}

	// FindUpstreamVersions ignores fingerprint — returns all known versions.
	versions, err := db.FindUpstreamVersions("github.com/go-chi/chi/v5", "Mux.ServeHTTP", "method", "*Mux")
	if err != nil {
		t.Fatalf("find versions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("expected 1 version, got %d", len(versions))
	}
}

func TestUpstreamFingerprints_UpsertOnDuplicateKey(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	row := UpstreamFingerprint{
		ModulePath:  "example.com/mod",
		Version:     "v1.0.0",
		DefName:     "Foo",
		Kind:        "function",
		Receiver:    "",
		Fingerprint: "first",
		Signature:   "func Foo()",
		Doc:         "original doc",
	}
	if err := db.InsertUpstreamFingerprint(row); err != nil {
		t.Fatalf("insert first: %v", err)
	}

	// Same primary key with a new fingerprint should update the row,
	// not add a duplicate.
	row.Fingerprint = "second"
	row.Doc = "updated doc"
	if err := db.InsertUpstreamFingerprint(row); err != nil {
		t.Fatalf("insert second: %v", err)
	}

	n, _ := db.CountUpstreamFingerprints()
	if n != 1 {
		t.Errorf("expected 1 row after upsert, got %d", n)
	}

	match, _ := db.FindUpstreamMatch("example.com/mod", "Foo", "function", "", "second")
	if match == nil || match.Doc != "updated doc" {
		t.Errorf("expected upserted row with second fingerprint + updated doc, got %+v", match)
	}
}
