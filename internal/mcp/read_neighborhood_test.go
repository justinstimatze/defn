package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

// TestReadNeighborhoodAppendedByDefault locks in #202: a bare
// code(op:"read") on a real def returns the body PLUS a "Related
// (#202)" footer listing callers/callees. Uses the setupTestDB
// fixture where Farewell calls Greet, so Greet has a caller.
func TestReadNeighborhoodAppendedByDefault(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	// Body still present (small def, below auto-downgrade threshold)
	if !strings.Contains(text, "Hello,") {
		t.Errorf("expected Greet body, got: %s", text)
	}
	// Neighborhood footer must appear
	if !strings.Contains(text, "Related (#202)") {
		t.Errorf("expected #202 Related footer, got: %s", text)
	}
	// Farewell calls Greet → should appear as a caller
	if !strings.Contains(text, "Farewell") {
		t.Errorf("expected Farewell as a caller in neighborhood, got: %s", text)
	}
}

// TestReadNeighborhoodSkippedForQuery verifies that query-adaptive
// filtered reads skip the neighborhood (query already narrows intent).
func TestReadNeighborhoodSkippedForQuery(t *testing.T) {
	db, _ := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db}

	result, _, err := s.handleGetDefinition(context.Background(), nil, nameParam{Name: "Greet", Query: "hello"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	if strings.Contains(text, "Related (#202)") {
		t.Errorf("query-scoped read must NOT emit #202 footer; got: %s", text)
	}
}

// TestBodyReferencesIdent_RequiresWordBoundary confirms the
// identifier-boundary check doesn't false-positive on a name that's
// merely a substring of a longer identifier (e.g. "Add" inside
// "AddAll"), while still matching plain calls and method calls.
func TestBodyReferencesIdent_RequiresWordBoundary(t *testing.T) {
	cases := []struct {
		body string
		name string
		want bool
	}{
		{"x := AddAll(1)", "Add", false},
		{"x := Add(1)", "Add", true},
		{"x.Add(1)", "Add", true},
		{"y := Address", "Add", false},
	}
	for _, c := range cases {
		if got := bodyReferencesIdent(c.body, c.name); got != c.want {
			t.Errorf("bodyReferencesIdent(%q, %q) = %v, want %v", c.body, c.name, got, c.want)
		}
	}
}

// TestPrioritizeByBodyReference_PromotesBodyReferencedCalleeAheadOfAlphabetical
// locks in the #313 cost-gap fix: GetCallees orders purely
// alphabetically, so a capped top-3 display on a high-fan-out
// function can surface names unrelated to what the body just shown
// actually calls, forcing an avoidable extra round-trip. Fabricates
// callee defs directly (bypassing ingest) since reproducing the exact
// real-world mechanism that creates a callee edge not literally named
// in body text is out of scope -- this pins the reordering logic
// itself, which is what changed.
func TestPrioritizeByBodyReference_PromotesBodyReferencedCalleeAheadOfAlphabetical(t *testing.T) {
	defs := []store.Definition{
		{Name: "AlphaFirst"},
		{Name: "AlphaSecond"},
		{Name: "AlphaThird"},
		{Name: "ZzzTarget"},
	}
	body := "func BigFunc() int {\n\treturn ZzzTarget()\n}"
	got := prioritizeByBodyReference(defs, body)
	names := make([]string, len(got))
	for i, d := range got {
		names[i] = d.Name
	}
	if len(got) != 4 || names[0] != "ZzzTarget" {
		t.Fatalf("expected ZzzTarget promoted to front, got order: %v", names)
	}
	wantRest := []string{"AlphaFirst", "AlphaSecond", "AlphaThird"}
	for i, name := range wantRest {
		if names[i+1] != name {
			t.Errorf("expected rest to preserve original relative order; position %d: want %s got %s", i+1, name, names[i+1])
		}
	}
}

// TestBodyReferencesIdent_RequiresCallShapeNotJustOccurrence locks in
// the first refinement found validating against a real prometheus
// trajectory ((*MSKDiscovery).refresh): a bare identifier-occurrence
// check false-positived on every type reference and struct field
// access sharing a name with an unrelated callee edge (all 44/44
// callees matched, making the reordering a no-op). Requiring a
// trailing "(" (call shape) excludes those non-call roles.
func TestBodyReferencesIdent_RequiresCallShapeNotJustOccurrence(t *testing.T) {
	cases := []struct {
		body string
		name string
		want bool
	}{
		{"var x types.Cluster", "Cluster", false},                // type reference
		{"tg.Targets = append(tg.Targets, x)", "Targets", false}, // field access (both occurrences)
		{"x := Cluster()", "Cluster", true},                      // real call
		{"y.Cluster()", "Cluster", true},                         // real method call
	}
	for _, c := range cases {
		if got := bodyReferencesIdent(c.body, c.name); got != c.want {
			t.Errorf("bodyReferencesIdent(%q, %q) = %v, want %v", c.body, c.name, got, c.want)
		}
	}
}

// TestPrioritizeByBodyReference_OrdersReferencedByBodyPositionNotAlphabetical
// locks in the second refinement found validating against a real
// prometheus trajectory: alphabetically-early common method names
// (Add, Done, Lock) genuinely called via an unrelated receiver
// (sync.WaitGroup, sync.Mutex) sorted ahead of a function's own
// earliest, most substantive calls when the referenced bucket kept
// its original (alphabetical) relative order. Ordering by first
// body-appearance position instead fixes this.
func TestPrioritizeByBodyReference_OrdersReferencedByBodyPositionNotAlphabetical(t *testing.T) {
	defs := []store.Definition{
		{Name: "AaaCalledLast"},
		{Name: "ZzzCalledFirst"},
	}
	body := "func F() {\n\tZzzCalledFirst()\n\t// ...\n\tAaaCalledLast()\n}"
	got := prioritizeByBodyReference(defs, body)
	if got[0].Name != "ZzzCalledFirst" || got[1].Name != "AaaCalledLast" {
		t.Fatalf("expected body-position order (ZzzCalledFirst, AaaCalledLast) despite reverse alphabetical order, got: %s, %s", got[0].Name, got[1].Name)
	}
}

// TestBodyAlreadyShowsDoc locks in the exact-match safety property:
// only skip the redundant doc echo when body's leading comment
// reconstructs doc byte-for-byte after stripping "// " markers.
func TestBodyAlreadyShowsDoc(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		body string
		want bool
	}{
		{
			name: "exact match, single line",
			doc:  "Widget does a thing.",
			body: "// Widget does a thing.\nfunc Widget() {}",
			want: true,
		},
		{
			name: "exact match, multi-line",
			doc:  "Widget does a thing.\nAnd another thing.",
			body: "// Widget does a thing.\n// And another thing.\nfunc Widget() {}",
			want: true,
		},
		{
			name: "body has no comment at all",
			doc:  "Widget does a thing.",
			body: "func Widget() {}",
			want: false,
		},
		{
			name: "doc differs from body's comment",
			doc:  "Widget does a thing, edited separately.",
			body: "// Widget does a thing.\nfunc Widget() {}",
			want: false,
		},
		{
			name: "empty doc never matches",
			doc:  "",
			body: "// Widget does a thing.\nfunc Widget() {}",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bodyAlreadyShowsDoc(c.doc, c.body); got != c.want {
				t.Errorf("bodyAlreadyShowsDoc(%q, %q) = %v, want %v", c.doc, c.body, got, c.want)
			}
		})
	}
}
