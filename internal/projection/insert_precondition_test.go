package projection

import (
	"testing"
)

// insertPreconditionFixture is a byte-exact PUTGET golden. Each fixture
// describes a `put(before, {condition, ret})` and its expected projection
// output. Success criterion is byte-equality between InsertPrecondition's
// output and `after`.
type insertPreconditionFixture struct {
	name      string
	before    string
	condition string
	ret       string
	after     string
}

var insertPreconditionFixtures = []insertPreconditionFixture{
	{
		name: "simple_negative_check",
		before: `func Foo(x int) error {
	return nil
}`,
		condition: "x < 0",
		ret:       `return errors.New("negative")`,
		after: `func Foo(x int) error {
	if x < 0 {
		return errors.New("negative")
	}
	return nil
}`,
	},
	{
		name: "nil_pointer_check",
		before: `func Bar(p *Data) error {
	return p.Process()
}`,
		condition: "p == nil",
		ret:       `return errors.New("nil data")`,
		after: `func Bar(p *Data) error {
	if p == nil {
		return errors.New("nil data")
	}
	return p.Process()
}`,
	},
	{
		name: "empty_body",
		before: `func Empty() {
}`,
		condition: "false",
		ret:       "return",
		after: `func Empty() {
	if false {
		return
	}
}`,
	},
	{
		name: "with_doc_comment",
		before: `// Foo does something.
func Foo(x int) error {
	return nil
}`,
		condition: "x < 0",
		ret:       `return errors.New("negative")`,
		after: `// Foo does something.
func Foo(x int) error {
	if x < 0 {
		return errors.New("negative")
	}
	return nil
}`,
	},
	{
		name: "pointer_receiver_method",
		before: `func (s *Server) Handle(req *Request) error {
	return s.process(req)
}`,
		condition: "req == nil",
		ret:       `return errors.New("nil request")`,
		after: `func (s *Server) Handle(req *Request) error {
	if req == nil {
		return errors.New("nil request")
	}
	return s.process(req)
}`,
	},
	{
		name: "named_returns",
		before: `func Divide(a, b int) (result int, err error) {
	result = a / b
	return
}`,
		condition: "b == 0",
		ret:       `return 0, errors.New("divide by zero")`,
		after: `func Divide(a, b int) (result int, err error) {
	if b == 0 {
		return 0, errors.New("divide by zero")
	}
	result = a / b
	return
}`,
	},
	{
		name: "multiple_return_values",
		before: `func Split(s string) (string, string, error) {
	parts := strings.SplitN(s, ":", 2)
	return parts[0], parts[1], nil
}`,
		condition: `s == ""`,
		ret:       `return "", "", errors.New("empty")`,
		after: `func Split(s string) (string, string, error) {
	if s == "" {
		return "", "", errors.New("empty")
	}
	parts := strings.SplitN(s, ":", 2)
	return parts[0], parts[1], nil
}`,
	},
	{
		name: "variadic",
		before: `func Sum(vals ...int) int {
	total := 0
	for _, v := range vals {
		total += v
	}
	return total
}`,
		condition: "len(vals) == 0",
		ret:       "return 0",
		after: `func Sum(vals ...int) int {
	if len(vals) == 0 {
		return 0
	}
	total := 0
	for _, v := range vals {
		total += v
	}
	return total
}`,
	},
	{
		name: "generic_type_param",
		before: `func Find[T comparable](xs []T, target T) (int, bool) {
	for i, x := range xs {
		if x == target {
			return i, true
		}
	}
	return -1, false
}`,
		condition: "len(xs) == 0",
		ret:       "return -1, false",
		after: `func Find[T comparable](xs []T, target T) (int, bool) {
	if len(xs) == 0 {
		return -1, false
	}
	for i, x := range xs {
		if x == target {
			return i, true
		}
	}
	return -1, false
}`,
	},
	{
		name: "existing_precondition_prepended",
		before: `func Chain(x int) error {
	if x > 100 {
		return errors.New("too large")
	}
	return nil
}`,
		condition: "x < 0",
		ret:       `return errors.New("negative")`,
		after: `func Chain(x int) error {
	if x < 0 {
		return errors.New("negative")
	}
	if x > 100 {
		return errors.New("too large")
	}
	return nil
}`,
	},
}

func TestInsertPrecondition_ByteExactPUTGET(t *testing.T) {
	for _, tc := range insertPreconditionFixtures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := InsertPrecondition(tc.before, tc.condition, tc.ret)
			if err != nil {
				t.Fatalf("InsertPrecondition: unexpected error: %v", err)
			}
			if got != tc.after {
				t.Errorf("byte-exact PUTGET failed for %q\n--- want ---\n%s\n--- got ---\n%s", tc.name, tc.after, got)
			}
		})
	}
}

func TestInsertPrecondition_ErrorCases(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		condition string
		ret       string
		wantSub   string
	}{
		{"empty_body", "", "x < 0", "return nil", "body is empty"},
		{"empty_condition", "func F() {}", "", "return", "condition is required"},
		{"empty_ret", "func F() {}", "true", "", "ret is required"},
		{"unparseable_body", "not go code {", "true", "return", "parse body"},
		{"not_a_func", "type T struct{}", "true", "return", "not a function declaration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := InsertPrecondition(tc.body, tc.condition, tc.ret)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
