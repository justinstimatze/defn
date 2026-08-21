package mcp

import (
	"reflect"
	"strings"
	"testing"
)

// TestApplyOpTagsAllowMixedOps locks in #182: every applyOp field
// except Op must carry ",omitempty" in its json tag. mcp-go generates
// the "code" tool's JSON schema from these tags via reflection; a
// field without omitempty becomes "required" in the schema, so a
// heterogeneous batch (e.g., {op:"create", ...} + {op:"add-import", ...})
// gets rejected at validation time with "missing properties: [condition,
// ret, slice, import_path, ...]". Op is the discriminator and MUST
// stay required; every other field is per-op conditional.
func TestApplyOpTagsAllowMixedOps(t *testing.T) {
	rt := reflect.TypeOf(applyOp{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if f.Name == "Op" {
			if tag != "op" {
				t.Errorf("Op tag = %q, want %q (discriminator must stay required)", tag, "op")
			}
			continue
		}
		if !strings.Contains(tag, ",omitempty") {
			t.Errorf("applyOp.%s json tag %q missing ,omitempty — mixed-op apply batches will fail schema validation",
				f.Name, tag)
		}
	}
}

// TestFragmentFieldsHaveEscapingGuidanceInSchema guards a v9 (sonnet)
// bench finding: a real apply batch failed client-side with
// InputValidationError because the model emitted an unescaped literal
// tab byte inside old_fragment, never reaching the server at all. Not
// fixable server-side (the malformed call never arrives), but the
// jsonschema struct tag (used by mcp-go's reflection-based schema
// generation, per google/jsonschema-go's doc.go) can surface escaping
// guidance at the exact point the model fills in the field instead of
// a 700-word tool-level preamble. This locks in that the tags parse as
// intended: reflect.StructTag.Get applies strconv.Unquote-style escape
// processing, so the Go source must use \\n (double backslash) to
// produce a literal two-character "\n" in the description -- a single
// backslash would decode to an actual newline and silently corrupt the
// guidance text.
func TestFragmentFieldsHaveEscapingGuidanceInSchema(t *testing.T) {
	for _, tc := range []struct {
		typ   reflect.Type
		field string
	}{
		{reflect.TypeOf(codeParam{}), "OldFragment"},
		{reflect.TypeOf(codeParam{}), "NewFragment"},
		{reflect.TypeOf(codeParam{}), "Old"},
		{reflect.TypeOf(codeParam{}), "New"},
		{reflect.TypeOf(applyOp{}), "OldFragment"},
		{reflect.TypeOf(applyOp{}), "NewFragment"},
		{reflect.TypeOf(applyOp{}), "Old"},
		{reflect.TypeOf(applyOp{}), "New"},
	} {
		f, ok := tc.typ.FieldByName(tc.field)
		if !ok {
			t.Fatalf("%s has no field %s", tc.typ, tc.field)
		}
		desc, ok := f.Tag.Lookup("jsonschema")
		if !ok {
			t.Errorf("%s.%s missing jsonschema tag", tc.typ, tc.field)
			continue
		}
		if !strings.Contains(desc, `\n`) {
			t.Errorf("%s.%s jsonschema tag should mention literal \\n escaping, got: %q", tc.typ, tc.field, desc)
		}
		if strings.ContainsRune(desc, '\n') {
			t.Errorf("%s.%s jsonschema tag contains an ACTUAL newline -- the Go source used a single backslash where it needed \\\\n; got: %q", tc.typ, tc.field, desc)
		}
	}

	testField, ok := reflect.TypeOf(codeParam{}).FieldByName("Test")
	if !ok {
		t.Fatal("codeParam has no Test field")
	}
	testDesc, ok := testField.Tag.Lookup("jsonschema")
	if !ok {
		t.Fatal("codeParam.Test missing jsonschema tag")
	}
	if !strings.Contains(testDesc, "RUN") {
		t.Errorf("codeParam.Test jsonschema tag should clarify it runs a test, got: %q", testDesc)
	}
}
