package planformat

import (
	"reflect"
	"testing"
)

func TestDSL_RoundTrip(t *testing.T) {
	steps := []Step{
		{Target: "Mux.ServeHTTP", Field: "body"},
		{Target: "Mux.Route", Field: "callers", ExcludeTest: true},
		{Target: "Handler", Field: "outline"},
	}
	got, err := ParseDSL(RenderDSL(steps))
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if !reflect.DeepEqual(got, steps) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, steps)
	}
}

func TestFormats_EquivalentAcrossJSONDSLSExpr(t *testing.T) {
	steps := []Step{
		{Target: "Handler.ServeHTTP", Field: "body"},
		{Target: "Handler.proxyLoopIteration", Field: "body"},
		{Target: "LoadBalancing.tryAgain", Field: "callers", ExcludeTest: true},
		{Target: "HTTPTransport", Field: "outline"},
	}
	jsonText, err := RenderJSON(steps)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	fromJSON, err := ParseJSON(jsonText)
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	fromDSL, err := ParseDSL(RenderDSL(steps))
	if err != nil {
		t.Fatalf("ParseDSL: %v", err)
	}
	fromSExpr, err := ParseSExpr(RenderSExpr(steps))
	if err != nil {
		t.Fatalf("ParseSExpr: %v", err)
	}
	if !reflect.DeepEqual(fromJSON, fromDSL) || !reflect.DeepEqual(fromDSL, fromSExpr) {
		t.Errorf("formats diverged: json=%+v dsl=%+v sexpr=%+v", fromJSON, fromDSL, fromSExpr)
	}
}

func TestJSON_RoundTrip(t *testing.T) {
	steps := []Step{
		{Target: "Mux.ServeHTTP", Field: "body"},
		{Target: "Mux.Route", Field: "callers", ExcludeTest: true},
	}
	text, err := RenderJSON(steps)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	got, err := ParseJSON(text)
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if !reflect.DeepEqual(got, steps) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, steps)
	}
}

func TestParseDSL_Basic(t *testing.T) {
	got, err := ParseDSL("@Handler.ServeHTTP.body\n@LoadBalancing.tryAgain.callers[!test]\n")
	if err != nil {
		t.Fatalf("ParseDSL: %v", err)
	}
	want := []Step{
		{Target: "Handler.ServeHTTP", Field: "body"},
		{Target: "LoadBalancing.tryAgain", Field: "callers", ExcludeTest: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseDSL_CommentsAndBlankLines(t *testing.T) {
	got, err := ParseDSL("# a comment\n\n@Foo.outline\n")
	if err != nil {
		t.Fatalf("ParseDSL: %v", err)
	}
	want := []Step{{Target: "Foo", Field: "outline"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseDSL_Errors(t *testing.T) {
	cases := []string{
		"Foo.body",         // missing @
		"@Foo",             // missing .field
		"@Foo.bogus",       // unknown field
		"@Foo.body[!nope]", // unsupported filter
		"@Foo.body[!test",  // unterminated filter
		"@.body",           // empty target
	}
	for _, c := range cases {
		if _, err := ParseDSL(c); err == nil {
			t.Errorf("ParseDSL(%q): expected error, got nil", c)
		}
	}
}

func TestParseSExpr_Basic(t *testing.T) {
	got, err := ParseSExpr("(read Handler.ServeHTTP)\n(impact LoadBalancing.tryAgain !test)\n")
	if err != nil {
		t.Fatalf("ParseSExpr: %v", err)
	}
	want := []Step{
		{Target: "Handler.ServeHTTP", Field: "body"},
		{Target: "LoadBalancing.tryAgain", Field: "callers", ExcludeTest: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseSExpr_Errors(t *testing.T) {
	cases := []string{
		"read Handler.ServeHTTP)",      // missing (
		"(read Handler.ServeHTTP",      // missing )
		"(read)",                       // missing target
		"(bogus Handler.ServeHTTP)",    // unknown op
		"(read Handler.ServeHTTP !no)", // unsupported flag
		"(read a b c d)",               // too many atoms
	}
	for _, c := range cases {
		if _, err := ParseSExpr(c); err == nil {
			t.Errorf("ParseSExpr(%q): expected error, got nil", c)
		}
	}
}

func TestSExpr_RoundTrip(t *testing.T) {
	steps := []Step{
		{Target: "Mux.ServeHTTP", Field: "body"},
		{Target: "Mux.Route", Field: "callers", ExcludeTest: true},
		{Target: "Handler", Field: "outline"},
	}
	got, err := ParseSExpr(RenderSExpr(steps))
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if !reflect.DeepEqual(got, steps) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, steps)
	}
}
