package projection

import (
	"strings"
	"testing"
)

// replaceHunkFixture is a byte-exact PUTGET golden for the replace-hunk
// operator. Content-addressed hunk replacement inside a def body.
type replaceHunkFixture struct {
	name        string
	body        string
	old         string
	replacement string
	index       int
	after       string
}

var replaceHunkFixtures = []replaceHunkFixture{
	{
		name: "simple_middle",
		body: `func F(x int) int {
	a := x + 1
	b := a * 2
	return b
}`,
		old:         "\tb := a * 2\n",
		replacement: "\tb := a * 3\n",
		after: `func F(x int) int {
	a := x + 1
	b := a * 3
	return b
}`,
	},
	{
		name: "simple_start",
		body: `func F() int {
	x := 1
	return x
}`,
		old:         "\tx := 1\n",
		replacement: "\tx := 42\n",
		after: `func F() int {
	x := 42
	return x
}`,
	},
	{
		name: "simple_end",
		body: `func F() int {
	x := 1
	return x
}`,
		old:         "\treturn x\n}",
		replacement: "\treturn x + 1\n}",
		after: `func F() int {
	x := 1
	return x + 1
}`,
	},
	{
		name: "repeated_unique_in_target",
		body: `func F() error {
	if a() {
		return err
	}
	do()
	return nil
}`,
		old:         "\t\treturn err\n",
		replacement: "\t\treturn fmt.Errorf(\"a: %w\", err)\n",
		after: `func F() error {
	if a() {
		return fmt.Errorf("a: %w", err)
	}
	do()
	return nil
}`,
	},
	{
		name: "repeated_with_index_second",
		body: `func F() error {
	if a() {
		return err
	}
	if b() {
		return err
	}
	if c() {
		return err
	}
	return nil
}`,
		old:         "\t\treturn err\n",
		replacement: "\t\treturn fmt.Errorf(\"b: %w\", err)\n",
		index:       2,
		after: `func F() error {
	if a() {
		return err
	}
	if b() {
		return fmt.Errorf("b: %w", err)
	}
	if c() {
		return err
	}
	return nil
}`,
	},
	{
		name: "empty_replacement_deletion",
		body: `func F() {
	log.Print("debug: entering F")
	do()
}`,
		old:         "\tlog.Print(\"debug: entering F\")\n",
		replacement: "",
		after: `func F() {
	do()
}`,
	},
	{
		name: "indent_preservation_tabs",
		body: `func F() {
	if true {
		x := 1
		y := 2
	}
}`,
		old:         "\t\tx := 1\n\t\ty := 2\n",
		replacement: "\t\tx, y := 1, 2\n",
		after: `func F() {
	if true {
		x, y := 1, 2
	}
}`,
	},
	{
		name: "embedded_comment_in_hunk",
		body: `func F() int {
	// old logic
	return 1
}`,
		old:         "\t// old logic\n\treturn 1\n",
		replacement: "\t// new logic\n\treturn 2\n",
		after: `func F() int {
	// new logic
	return 2
}`,
	},
	{
		name: "multi_line_hunk",
		body: `func F(x int) int {
	if x < 0 {
		return -x
	}
	return x
}`,
		old:         "\tif x < 0 {\n\t\treturn -x\n\t}\n",
		replacement: "\tif x < 0 {\n\t\treturn 0 - x\n\t}\n",
		after: `func F(x int) int {
	if x < 0 {
		return 0 - x
	}
	return x
}`,
	},
	{
		name: "unique_hunk_zero_index",
		body: `func F() int {
	answer := 41
	return answer
}`,
		old:         "answer := 41",
		replacement: "answer := 42",
		index:       0,
		after: `func F() int {
	answer := 42
	return answer
}`,
	},
	{
		name: "repeated_with_index_first",
		body: `func F() {
	x := 1
	x = 2
	y := x
	x = 3
	_ = y
}`,
		old:         "\tx = ",
		replacement: "\tx += ",
		index:       1,
		after: `func F() {
	x := 1
	x += 2
	y := x
	x = 3
	_ = y
}`,
	},
	{
		name: "repeated_with_index_last",
		body: `func F() {
	x := 1
	x = 2
	x = 3
	x = 4
}`,
		old:         "\tx = ",
		replacement: "\tx += ",
		index:       3,
		after: `func F() {
	x := 1
	x = 2
	x = 3
	x += 4
}`,
	},
}

func TestReplaceHunk_ByteExactPUTGET(t *testing.T) {
	for _, tc := range replaceHunkFixtures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceHunk(tc.body, tc.old, tc.replacement, tc.index)
			if err != nil {
				t.Fatalf("ReplaceHunk: unexpected error: %v", err)
			}
			if got != tc.after {
				t.Errorf("byte-exact PUTGET failed for %q\n--- want ---\n%s\n--- got ---\n%s", tc.name, tc.after, got)
			}
		})
	}
}

func TestReplaceHunk_ErrorCases(t *testing.T) {
	simple := `func F() error {
	return nil
}`
	repeated := `func F() error {
	if a() {
		return err
	}
	if b() {
		return err
	}
	return nil
}`
	cases := []struct {
		name        string
		body        string
		old         string
		replacement string
		index       int
		want        string
	}{
		{"empty_body", "", "return nil", "return err", 0, "body is empty"},
		{"empty_old", simple, "", "return err", 0, "old is required"},
		{"not_found", simple, "return 42", "return err", 0, "hunk not found"},
		{"ambiguous_no_index", repeated, "\t\treturn err\n", "return nil\n", 0, "hunk occurs 2 times"},
		{"index_out_of_range", repeated, "\t\treturn err\n", "return nil\n", 5, "exceeds 2 match"},
		{"negative_index", simple, "return nil", "return err", -1, "index must be >= 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReplaceHunk(tc.body, tc.old, tc.replacement, tc.index)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.want)
			}
		})
	}
}
