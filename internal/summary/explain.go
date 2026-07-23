package summary

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ExplainClient wraps the Anthropic SDK for the #186 co-processor
// role: given a natural-language question and assembled Go source
// context, produce a concise synthesized answer. Distinct from
// haikuBackend because the parameters differ (longer MaxTokens, longer
// request timeout, no per-request semaphore since caller controls
// parallelism) and the model tier differs (Sonnet, not Haiku — Haiku's
// multi-def code understanding is too shaky per 2026-07-23 discussion).
type ExplainClient struct {
	client *anthropic.Client
	model  anthropic.Model
}

// DefaultExplainModel is the model id used when [ExplainOptions.Model]
// is empty. Sonnet 4.6 is the highest available in SDK v1.50.1; bump
// to Sonnet 5 with a one-line change when the SDK carries the
// constant. See #186.
const DefaultExplainModel = anthropic.ModelClaudeSonnet4_6

// ExplainOptions configures [NewExplain]. APIKey is required; empty
// yields nil so the caller can surface a clear error path.
type ExplainOptions struct {
	APIKey     string
	Model      anthropic.Model
	BaseURL    string
	HTTPClient *http.Client
	// MaxTokens caps the co-processor's response. Default 500 —
	// enough for a 3-5 sentence explanation. Larger risks the model
	// re-emitting whole bodies verbatim.
	MaxTokens int
	// Timeout bounds one Messages request wall. Default 45s;
	// synthesizing across ~5 defs takes ~10-20s in practice.
	Timeout time.Duration
}

// NewExplain returns an [ExplainClient], or nil when APIKey is empty
// — the caller should surface a "set ANTHROPIC_API_KEY" error path.
// Distinct from [NewHaiku]'s Stub fallback because the explain op is
// user-invoked and a silent no-op would be baffling; whereas the
// Haiku summary backend runs opportunistically and Stub-degrading is
// harmless.
func NewExplain(opts ExplainOptions) *ExplainClient {
	if opts.APIKey == "" {
		return nil
	}
	model := opts.Model
	if model == "" {
		model = DefaultExplainModel
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	if opts.HTTPClient != nil {
		clientOpts = append(clientOpts, option.WithHTTPClient(opts.HTTPClient))
	}
	clientOpts = append(clientOpts, option.WithRequestTimeout(timeout))
	client := anthropic.NewClient(clientOpts...)
	return &ExplainClient{
		client: &client,
		model:  model,
	}
}

// buildExplainPrompt assembles the QA prompt sent to the co-processor.
// Layout: system-style guardrails at the top (grounded-in-source,
// concise, admit ignorance), the question next, then the assembled
// source blocks. Keeping the question above the source pins the
// model's attention to it before the long context.
func buildExplainPrompt(question, sourceContext string) string {
	return fmt.Sprintf(`You are answering a question about Go source code. Rules:
1. Only use the source below. Do NOT speculate about behavior that isn't visible in the code.
2. If the source doesn't contain the answer, say "The provided source does not contain the answer" and briefly note what additional definitions would be needed.
3. Be concise: 3-5 sentences unless the question genuinely needs more.
4. Reference specific function/type names when explaining, so a reader can verify.

QUESTION: %s

SOURCE:
%s`, question, sourceContext)
}

// Explain sends one QA request to the co-processor. sourceContext is
// concatenated def bodies (with headers naming each), question is the
// user's natural-language question. Returns the model's text answer.
func (e *ExplainClient) Explain(ctx context.Context, question, sourceContext string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("explain: client not configured (set ANTHROPIC_API_KEY)")
	}
	msg, err := e.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     e.model,
		MaxTokens: 500,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(buildExplainPrompt(question, sourceContext))),
		},
	})
	if err != nil {
		return "", fmt.Errorf("explain: messages.new: %w", err)
	}
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("explain: no text content in response")
	}
	return text, nil
}
