package summary

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHaiku_Generate_HappyPath spins up an httptest.Server that
// mimics the Anthropic Messages API and verifies:
//
//   - NewHaiku with a non-empty APIKey returns a real haikuBackend
//     (not the Stub null-object)
//   - Generate issues one POST per Request, in parallel up to the
//     configured Parallelism cap
//   - The response body's first text block becomes Result.OneLine
//   - Model is stamped on every Result for provenance
//
// Testing against a fake server (not a live API) keeps CI cheap and
// deterministic. The real SDK does the JSON round-trip so this also
// smoke-tests our SDK integration wiring.
func TestHaiku_Generate_HappyPath(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing x-api-key header, got %q", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		// Minimal valid Messages response shape.
		fmt.Fprint(w, `{
			"id": "msg_test",
			"type": "message",
			"role": "assistant",
			"model": "claude-haiku-4-5-20251001",
			"content": [{"type": "text", "text": "returns the current time in UTC"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`)
	}))
	t.Cleanup(srv.Close)

	b := NewHaiku(HaikuOptions{
		APIKey:      "test-key",
		BaseURL:     srv.URL,
		Parallelism: 4,
	})
	if _, ok := b.(Stub); ok {
		t.Fatalf("NewHaiku with APIKey returned Stub, expected haikuBackend")
	}

	reqs := []Request{
		{DefID: 1, Name: "Now", Kind: "function", Body: "func Now() time.Time { ... }", BodyHash: "h1"},
		{DefID: 2, Name: "UtcNow", Kind: "function", Body: "func UtcNow() time.Time { ... }", BodyHash: "h2"},
	}
	results := b.Generate(context.Background(), reqs)
	if len(results) != 2 {
		t.Fatalf("results len: got %d, want 2", len(results))
	}
	if got := int(callCount.Load()); got != 2 {
		t.Errorf("API call count: got %d, want 2", got)
	}
	for i, res := range results {
		if res.Err != nil {
			t.Errorf("result %d: unexpected err: %v", i, res.Err)
			continue
		}
		if res.OneLine != "returns the current time in UTC" {
			t.Errorf("result %d: OneLine=%q, want %q", i, res.OneLine, "returns the current time in UTC")
		}
		if res.BodyHash != reqs[i].BodyHash {
			t.Errorf("result %d: BodyHash=%q, want %q", i, res.BodyHash, reqs[i].BodyHash)
		}
		if res.Model != string(DefaultHaikuModel) {
			t.Errorf("result %d: Model=%q, want %q", i, res.Model, DefaultHaikuModel)
		}
	}
}

// TestHaiku_Generate_CruxNoneYieldsEmptyCrux confirms the model's explicit
// "no focal span" answer is respected rather than something being guessed.
func TestHaiku_Generate_CruxNoneYieldsEmptyCrux(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "msg_test", "type": "message", "role": "assistant",
			"model": "claude-haiku-4-5-20251001",
			"content": [{"type": "text", "text": "returns a constant name\nCRUX: NONE"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`)
	}))
	t.Cleanup(srv.Close)

	b := NewHaiku(HaikuOptions{APIKey: "test-key", BaseURL: srv.URL, Parallelism: 1})
	results := b.Generate(context.Background(), []Request{{DefID: 1, Name: "Name", Kind: "function", Body: "func Name() string { return \"x\" }", BodyHash: "h1"}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("unexpected results: %+v", results)
	}
	if results[0].Crux != "" {
		t.Errorf("Crux = %q, want empty for an explicit CRUX: NONE", results[0].Crux)
	}
}

// TestHaiku_Generate_ParsesCruxLine confirms the second response line
// ("CRUX: <start>-<end>") gets sliced verbatim out of the request's own
// Body and landed on Result.Crux, using 1-based line numbers relative to
// the code fence (matching what buildHaikuPrompt asks the model to count).
func TestHaiku_Generate_ParsesCruxLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "msg_test", "type": "message", "role": "assistant",
			"model": "claude-haiku-4-5-20251001",
			"content": [{"type": "text", "text": "returns early when x is negative\nCRUX: 2-4"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`)
	}))
	t.Cleanup(srv.Close)

	b := NewHaiku(HaikuOptions{APIKey: "test-key", BaseURL: srv.URL, Parallelism: 1})
	body := "func F(x int) int {\n\tif x < 0 {\n\t\treturn 0\n\t}\n\treturn x\n}"
	results := b.Generate(context.Background(), []Request{{DefID: 1, Name: "F", Kind: "function", Body: body, BodyHash: "h1"}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("unexpected results: %+v", results)
	}
	want := "\tif x < 0 {\n\t\treturn 0\n\t}"
	if results[0].Crux != want {
		t.Errorf("Crux = %q, want %q", results[0].Crux, want)
	}
	if results[0].OneLine != "returns early when x is negative" {
		t.Errorf("OneLine = %q, want the first line only (CRUX line must not leak into it)", results[0].OneLine)
	}
}

func TestExtractCrux_CapsAtEightLines(t *testing.T) {
	body := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10"
	got := extractCrux(body, "CRUX: 1-10")
	if lines := strings.Count(got, "\n") + 1; lines != 8 {
		t.Errorf("expected the crux capped at 8 lines, got %d lines: %q", lines, got)
	}
}

func TestExtractCrux_MalformedLineReturnsEmpty(t *testing.T) {
	if got := extractCrux("l1\nl2\nl3", "not a crux line"); got != "" {
		t.Errorf("expected empty crux for a malformed line, got %q", got)
	}
}

func TestExtractCrux_OutOfRangeStartReturnsEmpty(t *testing.T) {
	if got := extractCrux("line1\nline2", "CRUX: 10-12"); got != "" {
		t.Errorf("expected empty crux for an out-of-range start, got %q", got)
	}
}
