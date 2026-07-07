package projection

import (
	"strings"
	"testing"
)

// replaceSliceFixture is a byte-exact PUTGET golden for the
// replace-slice operator. Per the Phase C plan: 30 fixtures spanning
// return / error-branch / loop kinds (~10 each).
type replaceSliceFixture struct {
	name  string
	body  string
	kind  string
	index int
	new   string
	after string
}

var replaceSliceFixtures = []replaceSliceFixture{
	// ── kind: return (10) ────────────────────────────────────────────
	{
		name: "return_nil_to_io_eof",
		body: `func F() error {
	return nil
}`,
		kind: "return", index: 1,
		new: `return io.EOF`,
		after: `func F() error {
	return io.EOF
}`,
	},
	{
		name: "return_err_to_wrapped",
		body: `func F() error {
	return err
}`,
		kind: "return", index: 1,
		new: `return fmt.Errorf("F: %w", err)`,
		after: `func F() error {
	return fmt.Errorf("F: %w", err)
}`,
	},
	{
		name: "return_empty_string_to_computed",
		body: `func F(s string) (string, error) {
	return "", nil
}`,
		kind: "return", index: 1,
		new: `return strings.TrimSpace(s), nil`,
		after: `func F(s string) (string, error) {
	return strings.TrimSpace(s), nil
}`,
	},
	{
		name: "return_bare_to_valued",
		body: `func F() (result int, err error) {
	result = 42
	return
}`,
		kind: "return", index: 1,
		new: `return result, nil`,
		after: `func F() (result int, err error) {
	result = 42
	return result, nil
}`,
	},
	{
		name: "return_first_of_two",
		body: `func F(x int) int {
	if x < 0 {
		return -1
	}
	return x
}`,
		kind: "return", index: 1,
		new: `return 0`,
		after: `func F(x int) int {
	if x < 0 {
		return 0
	}
	return x
}`,
	},
	{
		name: "return_second_of_two",
		body: `func F(x int) int {
	if x < 0 {
		return -1
	}
	return x
}`,
		kind: "return", index: 2,
		new: `return x * 2`,
		after: `func F(x int) int {
	if x < 0 {
		return -1
	}
	return x * 2
}`,
	},
	{
		name: "return_true_to_false",
		body: `func F() bool {
	return true
}`,
		kind: "return", index: 1,
		new: `return false`,
		after: `func F() bool {
	return false
}`,
	},
	{
		name: "return_multivalue",
		body: `func F() (int, string, error) {
	return 0, "", nil
}`,
		kind: "return", index: 1,
		new: `return 1, "x", errors.New("boom")`,
		after: `func F() (int, string, error) {
	return 1, "x", errors.New("boom")
}`,
	},
	{
		name: "return_method_receiver",
		body: `func (s *Server) Get() string {
	return s.name
}`,
		kind: "return", index: 1,
		new: `return strings.ToLower(s.name)`,
		after: `func (s *Server) Get() string {
	return strings.ToLower(s.name)
}`,
	},
	{
		name: "return_third_of_three",
		body: `func F(x int) string {
	if x < 0 {
		return "neg"
	}
	if x == 0 {
		return "zero"
	}
	return "pos"
}`,
		kind: "return", index: 3,
		new: `return fmt.Sprintf("pos:%d", x)`,
		after: `func F(x int) string {
	if x < 0 {
		return "neg"
	}
	if x == 0 {
		return "zero"
	}
	return fmt.Sprintf("pos:%d", x)
}`,
	},

	// ── kind: error-branch (10) ──────────────────────────────────────
	{
		name: "errbranch_simple",
		body: `func F() error {
	if err != nil {
		return err
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if err != nil {
		return fmt.Errorf("F: %w", err)
	}`,
		after: `func F() error {
	if err != nil {
		return fmt.Errorf("F: %w", err)
	}
	return nil
}`,
	},
	{
		name: "errbranch_named_dbErr",
		body: `func F() error {
	if dbErr != nil {
		return dbErr
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if dbErr != nil {
		log.Printf("db: %v", dbErr)
		return dbErr
	}`,
		after: `func F() error {
	if dbErr != nil {
		log.Printf("db: %v", dbErr)
		return dbErr
	}
	return nil
}`,
	},
	{
		name: "errbranch_first_of_two",
		body: `func F() error {
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if err != nil {
		return errors.New("first")
	}`,
		after: `func F() error {
	if err != nil {
		return errors.New("first")
	}
	if err != nil {
		return err
	}
	return nil
}`,
	},
	{
		name: "errbranch_second_of_two",
		body: `func F() error {
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	return nil
}`,
		kind: "error-branch", index: 2,
		new: `if err != nil {
		return errors.New("second")
	}`,
		after: `func F() error {
	if err != nil {
		return err
	}
	if err != nil {
		return errors.New("second")
	}
	return nil
}`,
	},
	{
		name: "errbranch_with_body_wrap",
		body: `func F() error {
	x := 1
	if err != nil {
		return err
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if err != nil {
		return fmt.Errorf("x=%d: %w", x, err)
	}`,
		after: `func F() error {
	x := 1
	if err != nil {
		return fmt.Errorf("x=%d: %w", x, err)
	}
	return nil
}`,
	},
	{
		name: "errbranch_readErr",
		body: `func F(r io.Reader) error {
	_, readErr := r.Read(nil)
	if readErr != nil {
		return readErr
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if readErr != nil {
		return io.ErrUnexpectedEOF
	}`,
		after: `func F(r io.Reader) error {
	_, readErr := r.Read(nil)
	if readErr != nil {
		return io.ErrUnexpectedEOF
	}
	return nil
}`,
	},
	{
		name: "errbranch_method_receiver",
		body: `func (s *Server) Do() error {
	if err != nil {
		return err
	}
	return s.commit()
}`,
		kind: "error-branch", index: 1,
		new: `if err != nil {
		s.rollback()
		return err
	}`,
		after: `func (s *Server) Do() error {
	if err != nil {
		s.rollback()
		return err
	}
	return s.commit()
}`,
	},
	{
		name: "errbranch_middle_of_three",
		body: `func F() error {
	if err != nil {
		return errors.New("a")
	}
	if err != nil {
		return errors.New("b")
	}
	if err != nil {
		return errors.New("c")
	}
	return nil
}`,
		kind: "error-branch", index: 2,
		new: `if err != nil {
		return errors.New("B")
	}`,
		after: `func F() error {
	if err != nil {
		return errors.New("a")
	}
	if err != nil {
		return errors.New("B")
	}
	if err != nil {
		return errors.New("c")
	}
	return nil
}`,
	},
	{
		name: "errbranch_expand_to_log_and_return",
		body: `func F() error {
	if err != nil {
		return err
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if err != nil {
		log.Println(err)
		return err
	}`,
		after: `func F() error {
	if err != nil {
		log.Println(err)
		return err
	}
	return nil
}`,
	},
	{
		name: "errbranch_shrink_to_panic",
		body: `func F() error {
	if err != nil {
		log.Println("boom")
		return err
	}
	return nil
}`,
		kind: "error-branch", index: 1,
		new: `if err != nil {
		panic(err)
	}`,
		after: `func F() error {
	if err != nil {
		panic(err)
	}
	return nil
}`,
	},

	// ── kind: loop (10) ──────────────────────────────────────────────
	{
		name: "loop_for_i",
		body: `func F(n int) {
	for i := 0; i < n; i++ {
		fmt.Println(i)
	}
}`,
		kind: "loop", index: 1,
		new: `for i := n - 1; i >= 0; i-- {
		fmt.Println(i)
	}`,
		after: `func F(n int) {
	for i := n - 1; i >= 0; i-- {
		fmt.Println(i)
	}
}`,
	},
	{
		name: "loop_range_slice",
		body: `func F(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}`,
		kind: "loop", index: 1,
		new: `for i := 0; i < len(xs); i++ {
		total += xs[i]
	}`,
		after: `func F(xs []int) int {
	total := 0
	for i := 0; i < len(xs); i++ {
		total += xs[i]
	}
	return total
}`,
	},
	{
		name: "loop_range_map",
		body: `func F(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}`,
		kind: "loop", index: 1,
		new: `for k, v := range m {
		if k != "" {
			total += v
		}
	}`,
		after: `func F(m map[string]int) int {
	total := 0
	for k, v := range m {
		if k != "" {
			total += v
		}
	}
	return total
}`,
	},
	{
		name: "loop_for_true",
		body: `func F(ch chan int) int {
	for {
		v := <-ch
		if v == 0 {
			return v
		}
	}
}`,
		kind: "loop", index: 1,
		new: `for v := range ch {
		if v == 0 {
			return v
		}
	}`,
		after: `func F(ch chan int) int {
	for v := range ch {
		if v == 0 {
			return v
		}
	}
}`,
	},
	{
		name: "loop_select",
		body: `func F(a, b chan int) int {
	select {
	case v := <-a:
		return v
	case v := <-b:
		return v
	}
}`,
		kind: "loop", index: 1,
		new: `select {
	case v := <-a:
		return v * 2
	case v := <-b:
		return v * 3
	}`,
		after: `func F(a, b chan int) int {
	select {
	case v := <-a:
		return v * 2
	case v := <-b:
		return v * 3
	}
}`,
	},
	{
		name: "loop_second_of_two",
		body: `func F(a, b []int) int {
	total := 0
	for _, x := range a {
		total += x
	}
	for _, x := range b {
		total += x
	}
	return total
}`,
		kind: "loop", index: 2,
		new: `for i := len(b) - 1; i >= 0; i-- {
		total += b[i]
	}`,
		after: `func F(a, b []int) int {
	total := 0
	for _, x := range a {
		total += x
	}
	for i := len(b) - 1; i >= 0; i-- {
		total += b[i]
	}
	return total
}`,
	},
	{
		name: "loop_method_receiver",
		body: `func (c *Coll) Sum() int {
	total := 0
	for _, x := range c.items {
		total += x
	}
	return total
}`,
		kind: "loop", index: 1,
		new: `for _, x := range c.items {
		total += x * x
	}`,
		after: `func (c *Coll) Sum() int {
	total := 0
	for _, x := range c.items {
		total += x * x
	}
	return total
}`,
	},
	{
		name: "loop_infinite_replace_bounded",
		body: `func F() {
	for {
		break
	}
}`,
		kind: "loop", index: 1,
		new: `for i := 0; i < 10; i++ {
		break
	}`,
		after: `func F() {
	for i := 0; i < 10; i++ {
		break
	}
}`,
	},
	{
		name: "loop_first_of_three",
		body: `func F(a, b, c []int) int {
	total := 0
	for _, x := range a {
		total += x
	}
	for _, x := range b {
		total += x
	}
	for _, x := range c {
		total += x
	}
	return total
}`,
		kind: "loop", index: 1,
		new: `for _, x := range a {
		total -= x
	}`,
		after: `func F(a, b, c []int) int {
	total := 0
	for _, x := range a {
		total -= x
	}
	for _, x := range b {
		total += x
	}
	for _, x := range c {
		total += x
	}
	return total
}`,
	},
	{
		name: "loop_range_with_index_only",
		body: `func F(xs []int) int {
	count := 0
	for i := range xs {
		count += i
	}
	return count
}`,
		kind: "loop", index: 1,
		new: `for i, v := range xs {
		count += i + v
	}`,
		after: `func F(xs []int) int {
	count := 0
	for i, v := range xs {
		count += i + v
	}
	return count
}`,
	},
}

func TestReplaceSlice_ByteExactPUTGET(t *testing.T) {
	for _, tc := range replaceSliceFixtures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceSlice(tc.body, tc.kind, tc.index, tc.new)
			if err != nil {
				t.Fatalf("ReplaceSlice: unexpected error: %v", err)
			}
			if got != tc.after {
				t.Errorf("byte-exact PUTGET failed for %q\n--- want ---\n%s\n--- got ---\n%s", tc.name, tc.after, got)
			}
		})
	}
}

func TestReplaceSlice_ErrorCases(t *testing.T) {
	simple := `func F() error {
	return nil
}`
	cases := []struct {
		name  string
		body  string
		kind  string
		index int
		new   string
		want  string
	}{
		{"empty_body", "", "return", 1, "return nil", "body is empty"},
		{"unknown_kind", simple, "banana", 1, "return nil", "unknown slice kind"},
		{"index_zero", simple, "return", 0, "return nil", "index must be >= 1"},
		{"index_out_of_range", simple, "return", 5, "return nil", "exceeds 1 match"},
		{"no_matches", `func F() {}`, "return", 1, "return", "no return slices found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReplaceSlice(tc.body, tc.kind, tc.index, tc.new)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSlices_ByteExactInvariant(t *testing.T) {
	for _, tc := range replaceSliceFixtures {
		t.Run(tc.name, func(t *testing.T) {
			slices, err := Slices(tc.body, tc.kind)
			if err != nil {
				t.Fatalf("Slices: %v", err)
			}
			for i, s := range slices {
				if s.Source != tc.body[s.StartOff:s.EndOff] {
					t.Errorf("slice[%d] Source vs offsets mismatch\nSource:      %q\nbody[s:e]:   %q", i, s.Source, tc.body[s.StartOff:s.EndOff])
				}
			}
		})
	}
}
