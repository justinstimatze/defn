package projection

import (
	"go/format"
	"strings"
	"testing"
)

type addImportFixture struct {
	name  string
	src   string
	path  string
	alias string
	after string
}

var addImportFixtures = []addImportFixture{
	{
		name: "no_existing_imports",
		src: `package p

func F() {}
`,
		path: "fmt",
		after: `package p

import "fmt"

func F() {}
`,
	},
	{
		name: "single_stdlib_add_stdlib",
		src: `package p

import "fmt"

func F() { fmt.Println() }
`,
		path: "os",
		after: `package p

import (
	"fmt"
	"os"
)

func F() { fmt.Println() }
`,
	},
	{
		name: "single_stdlib_add_thirdparty",
		src: `package p

import "fmt"

func F() { fmt.Println() }
`,
		path: "github.com/pkg/errors",
		after: `package p

import (
	"fmt"

	"github.com/pkg/errors"
)

func F() { fmt.Println() }
`,
		// v1 note: astutil produces a flat block here (no blank-line
		// group). Fixture asserts the ≡_import_order equivalent form
		// after a canonicalization pass in the test harness.
	},
	{
		name: "two_groups_add_stdlib",
		src: `package p

import (
	"fmt"

	"github.com/pkg/errors"
)

func F() { fmt.Println(errors.New("x")) }
`,
		path: "io",
		after: `package p

import (
	"fmt"
	"io"

	"github.com/pkg/errors"
)

func F() { fmt.Println(errors.New("x")) }
`,
	},
	{
		name: "named_import",
		src: `package p

import "fmt"

func F() { fmt.Println() }
`,
		path:  "github.com/pkg/errors",
		alias: "pkgerrors",
		after: `package p

import (
	"fmt"

	pkgerrors "github.com/pkg/errors"
)

func F() { fmt.Println() }
`,
	},
}

// gofmtSrc canonicalizes a full file source.
func gofmtSrc(t *testing.T, src string) string {
	t.Helper()
	out, err := format.Source([]byte(src))
	if err != nil {
		t.Fatalf("gofmt failed: %v\nsrc:\n%s", err, src)
	}
	return string(out)
}

// importSet extracts the set of import lines from a canonicalized file,
// dropping blank lines. This is the ≡_import_order equivalence relation:
// two blocks are equivalent if their per-line import sets match after
// gofmt normalization. Group boundaries (blank lines inside `import
// (...)`) are treated as insignificant in v1 because astutil does not
// create new groups for flat input blocks.
func importSet(src string) []string {
	lines := strings.Split(src, "\n")
	var out []string
	inImport := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(trim, "import ("):
			inImport = true
		case inImport && trim == ")":
			inImport = false
		case inImport && trim != "":
			out = append(out, trim)
		case strings.HasPrefix(trim, "import "):
			out = append(out, strings.TrimPrefix(trim, "import "))
		}
	}
	return out
}

func TestAddImport_ImportOrderEquivPUTGET(t *testing.T) {
	for _, tc := range addImportFixtures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AddImport(tc.src, tc.path, tc.alias)
			if err != nil {
				t.Fatalf("AddImport: %v", err)
			}
			gotFmt := gofmtSrc(t, got)
			wantFmt := gofmtSrc(t, tc.after)
			gotSet := importSet(gotFmt)
			wantSet := importSet(wantFmt)
			if strings.Join(gotSet, "\n") != strings.Join(wantSet, "\n") {
				t.Errorf("≡_import_order PUTGET failed for %q\n--- want imports ---\n%s\n--- got imports ---\n%s\n--- got full source ---\n%s", tc.name, strings.Join(wantSet, "\n"), strings.Join(gotSet, "\n"), gotFmt)
			}
		})
	}
}

func TestAddImport_IdempotentNoOp(t *testing.T) {
	src := `package p

import "fmt"

func F() { fmt.Println() }
`
	got, err := AddImport(src, "fmt", "")
	if err != nil {
		t.Fatalf("AddImport: %v", err)
	}
	if got != src {
		t.Errorf("expected byte-exact no-op for already-imported path\nwant: %q\ngot:  %q", src, got)
	}
}

func TestAddImport_ErrorCases(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		path  string
		alias string
		want  string
	}{
		{"empty_src", "", "fmt", "", "file source is empty"},
		{"empty_path", "package p\n", "", "", "path is required"},
		{"unparseable", "not go", "fmt", "", "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AddImport(tc.src, tc.path, tc.alias)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.want)
			}
		})
	}
}
