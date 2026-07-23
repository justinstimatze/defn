package summary

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultHaikuModel is the model id used when [HaikuOptions.Model] is
// empty. Bump to Haiku 4.6/5 with a one-line change; the SDK's typed
// [anthropic.Model] constants track the wire ids.
const DefaultHaikuModel = anthropic.ModelClaudeHaiku4_5

// HaikuOptions configures a Haiku-backed summary generator.
//
//   - APIKey: required. Empty → [NewHaiku] returns [Stub]{} so defn
//     stays usable offline without ceremony.
//   - Model: [anthropic.Model] id. Empty → [DefaultHaikuModel].
//   - BaseURL: override for testing against httptest.Server. Empty →
//     the real Anthropic endpoint.
//   - HTTPClient: injected transport for tests or custom retry. Empty
//     → the SDK's default client.
//   - Parallelism: max concurrent in-flight API calls. Empty → 8.
//     Balances throughput vs API rate limits; SDK handles per-call
//     retry/backoff on 429/5xx, this bound protects the tier quota.
type HaikuOptions struct {
	APIKey      string
	Model       anthropic.Model
	BaseURL     string
	HTTPClient  *http.Client
	Parallelism int
}

// NewHaiku returns a Haiku [Backend], or [Stub]{} when APIKey is
// empty. Never returns nil — the pipeline is always usable.
func NewHaiku(opts HaikuOptions) Backend {
	if opts.APIKey == "" {
		return Stub{}
	}
	model := opts.Model
	if model == "" {
		model = DefaultHaikuModel
	}
	par := opts.Parallelism
	if par <= 0 {
		par = 8
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	if opts.HTTPClient != nil {
		clientOpts = append(clientOpts, option.WithHTTPClient(opts.HTTPClient))
	}
	// Bound per-request wall so a hung API call can't wedge the worker
	// forever. SDK enforces this on top of its own retry logic.
	clientOpts = append(clientOpts, option.WithRequestTimeout(30*time.Second))
	client := anthropic.NewClient(clientOpts...)
	return &haikuBackend{
		client: &client,
		model:  model,
		sem:    make(chan struct{}, par),
	}
}

type haikuBackend struct {
	client *anthropic.Client
	model  anthropic.Model
	sem    chan struct{}
}

func (h *haikuBackend) Name() string { return string(h.model) }

// Generate fans out over reqs up to Parallelism concurrent API
// calls. One Result per Request, in input order. Failed calls carry
// Err — the worker skips persistence for those, so a bad batch never
// overwrites good prior summaries.
func (h *haikuBackend) Generate(ctx context.Context, reqs []Request) []Result {
	out := make([]Result, len(reqs))
	var wg sync.WaitGroup
	for i, r := range reqs {
		wg.Add(1)
		go func(i int, r Request) {
			defer wg.Done()
			select {
			case h.sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = Result{DefID: r.DefID, BodyHash: r.BodyHash, Model: string(h.model), Err: ctx.Err()}
				return
			}
			defer func() { <-h.sem }()
			line, err := h.callOne(ctx, r)
			if err != nil {
				out[i] = Result{DefID: r.DefID, BodyHash: r.BodyHash, Model: string(h.model), Err: err}
				return
			}
			out[i] = Result{
				DefID:    r.DefID,
				OneLine:  line,
				BodyHash: r.BodyHash,
				Model:    string(h.model),
			}
		}(i, r)
	}
	wg.Wait()
	return out
}

// callOne issues one Messages request and returns the first line of
// the model's response. SDK handles retry/backoff internally.
func (h *haikuBackend) callOne(ctx context.Context, r Request) (string, error) {
	prompt := buildHaikuPrompt(r)
	msg, err := h.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     h.model,
		MaxTokens: 80,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("haiku: messages.new: %w", err)
	}
	for _, block := range msg.Content {
		if block.Type == "text" {
			text := strings.TrimSpace(block.Text)
			if text == "" {
				continue
			}
			if idx := strings.IndexByte(text, '\n'); idx >= 0 {
				text = text[:idx]
			}
			return strings.TrimSpace(text), nil
		}
	}
	return "", fmt.Errorf("haiku: no text content in response")
}

// buildHaikuPrompt renders the one-line-summary prompt for one def.
// Kind/receiver hint helps the model produce accurate summaries (a
// method summary reads differently than a top-level function). No
// preamble/quotes/trailing period so the returned line drops straight
// into rendered read output.
func buildHaikuPrompt(r Request) string {
	kind := r.Kind
	if r.Receiver != "" {
		kind = "method on " + r.Receiver
	}
	return fmt.Sprintf(
		"Summarize this Go %s in ONE sentence, max 20 words. "+
			"Focus on WHAT it does, not HOW. "+
			"No preamble, no quotes, no trailing period.\n\n"+
			"Name: %s\nModule: %s\n\n"+
			"```go\n%s\n```",
		kind, r.Name, r.ModulePath, r.Body,
	)
}
