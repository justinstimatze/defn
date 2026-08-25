package summary

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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
// Err -- the worker skips persistence for those, so a bad batch never
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
			line, crux, err := h.callOne(ctx, r)
			if err != nil {
				out[i] = Result{DefID: r.DefID, BodyHash: r.BodyHash, Model: string(h.model), Err: err}
				return
			}
			out[i] = Result{
				DefID:    r.DefID,
				OneLine:  line,
				Crux:     crux,
				BodyHash: r.BodyHash,
				Model:    string(h.model),
			}
		}(i, r)
	}
	wg.Wait()
	return out
}

// callOne issues one Messages request and returns the summary's first
// line plus the crux excerpt parsed from the second (see
// buildHaikuPrompt/extractCrux). SDK handles retry/backoff internally.
func (h *haikuBackend) callOne(ctx context.Context, r Request) (line string, crux string, err error) {
	prompt := buildHaikuPrompt(r)
	msg, err := h.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     h.model,
		MaxTokens: 120,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("haiku: messages.new: %w", err)
	}
	for _, block := range msg.Content {
		if block.Type == "text" {
			text := strings.TrimSpace(block.Text)
			if text == "" {
				continue
			}
			parts := strings.SplitN(text, "\n", 2)
			summary := strings.TrimSpace(parts[0])
			if summary == "" {
				continue
			}
			var cruxText string
			if len(parts) > 1 {
				cruxText = extractCrux(r.Body, parts[1])
			}
			return summary, cruxText, nil
		}
	}
	return "", "", fmt.Errorf("haiku: no text content in response")
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
			"Then, on a SECOND line, name the single most important contiguous "+
			"span of lines in the code below (the core branch, guard, or state "+
			"change a reader must see to understand the logic) as "+
			"\"CRUX: <start>-<end>\" using 1-based line numbers counting the "+
			"first line inside the ```go fence as line 1. At most 8 lines. If "+
			"there is no single focal span (a trivial getter, a plain data "+
			"holder, or logic spread evenly), write exactly \"CRUX: NONE\" "+
			"instead -- that IS the answer, do not guess a span anyway.\n\n"+
			"Name: %s\nModule: %s\n\n"+
			"```go\n%s\n```",
		kind, r.Name, r.ModulePath, r.Body,
	)
}

// cruxLineRe matches the "CRUX: <start>-<end>" line buildHaikuPrompt asks
// for. "CRUX: NONE" (or anything else that doesn't match) yields no crux.
var cruxLineRe = regexp.MustCompile(`^CRUX:\s*(\d+)\s*-\s*(\d+)\s*$`)

// extractCrux slices the model-named line span verbatim out of body. Never
// trusts the model's line numbers beyond body's own bounds, and caps the
// span at 8 lines even if the model overshoots -- this is a display
// excerpt, not a full re-transmission of the body.
func extractCrux(body, cruxLine string) string {
	m := cruxLineRe.FindStringSubmatch(strings.TrimSpace(cruxLine))
	if m == nil {
		return ""
	}
	start, err1 := strconv.Atoi(m[1])
	end, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || start < 1 || end < start {
		return ""
	}
	lines := strings.Split(body, "\n")
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end-start+1 > 8 {
		end = start + 7
	}
	return strings.Join(lines[start-1:end], "\n")
}
