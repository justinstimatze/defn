package projection

import (
	"strings"
	"testing"
)

type wrapInDeferFixture struct {
	name      string
	body      string
	stmtIndex int
	deferBody string
	after     string
}

var wrapInDeferFixtures = []wrapInDeferFixture{
	{
		name: "empty_body",
		body: `func F(ch chan int) {
}`,
		stmtIndex: 0,
		deferBody: `close(ch)`,
		after: `func F(ch chan int) {
	defer close(ch)
}`,
	},
	{
		name: "single_stmt_top",
		body: `func F() {
	fmt.Println("hi")
}`,
		stmtIndex: 1,
		deferBody: `fmt.Println("bye")`,
		after: `func F() {
	defer fmt.Println("bye")
	fmt.Println("hi")
}`,
	},
	{
		name: "multi_stmt_at_top",
		body: `func F(mu sync.Mutex) {
	mu.Lock()
	work()
	log("done")
}`,
		stmtIndex: 1,
		deferBody: `mu.Unlock()`,
		after: `func F(mu sync.Mutex) {
	defer mu.Unlock()
	mu.Lock()
	work()
	log("done")
}`,
	},
	{
		name: "multi_stmt_middle",
		body: `func F(mu sync.Mutex) {
	mu.Lock()
	work()
	log("done")
}`,
		stmtIndex: 2,
		deferBody: `mu.Unlock()`,
		after: `func F(mu sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	work()
	log("done")
}`,
	},
	{
		name: "multi_stmt_last",
		body: `func F() error {
	x := open()
	y := load(x)
	return process(y)
}`,
		stmtIndex: 3,
		deferBody: `x.Close()`,
		after: `func F() error {
	x := open()
	y := load(x)
	defer x.Close()
	return process(y)
}`,
	},
}

func TestWrapInDefer_ByteExactPUTGET(t *testing.T) {
	for _, tc := range wrapInDeferFixtures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WrapInDefer(tc.body, tc.stmtIndex, tc.deferBody)
			if err != nil {
				t.Fatalf("WrapInDefer: %v", err)
			}
			if got != tc.after {
				t.Errorf("byte-exact PUTGET failed for %q\n--- want ---\n%s\n--- got ---\n%s", tc.name, tc.after, got)
			}
		})
	}
}

func TestWrapInDefer_ErrorCases(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		stmtIndex int
		defer_    string
		want      string
	}{
		{"empty_body", "", 1, "close()", "body is empty"},
		{"empty_defer", "func F() {}", 1, "", "defer_body is required"},
		{"unparseable", "not go code {", 1, "close()", "parse body"},
		{"not_a_func", "type T struct{}", 1, "close()", "not a function declaration"},
		{"index_too_big", "func F() {\n\ta()\n}", 5, "close()", "exceeds 1 statement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WrapInDefer(tc.body, tc.stmtIndex, tc.defer_)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.want)
			}
		})
	}
}
