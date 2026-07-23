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
