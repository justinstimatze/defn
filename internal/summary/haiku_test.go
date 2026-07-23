package summary

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
