package adapters

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/justinstimatze/defn/bench/retrieval/benchtype"
	"github.com/justinstimatze/defn/bench/retrieval/normalize"
)

// Grep implements benchtype.Adapter as the baseline: raw ripgrep search.
// This represents what an AI agent does today without any code intelligence tool.
type Grep struct{}

func NewGrep() *Grep { return &Grep{} }

func (a *Grep) Name() string { return "grep" }

func (a *Grep) Index(_ string) (int64, error) { return 0, nil }

func (a *Grep) Retrieve(repoPath string, task benchtype.Task, _ int) (benchtype.RetrievalResult, error) {
	start := time.Now()

	keywords := extractKeywords(task.Description)
	if len(keywords) == 0 {
		return benchtype.RetrievalResult{System: "grep", TaskID: task.ID}, nil
	}

	seen := make(map[string]bool)
	var symbols []benchtype.RetrievedSymbol
	rank := 1

	for _, kw := range keywords {
		if len(symbols) >= 20 {
			break
		}

		// Search for symbol definitions matching this keyword
		cmd := exec.Command("rg",
			"--no-heading", "-n", "--max-count", "10",
			"-e", fmt.Sprintf(`(func|def|class|type|interface|struct)\s+\w*%s\w*`, kw),
			repoPath,
		)
		output, err := cmd.Output()
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(strings.NewReader(string(output)))
		for scanner.Scan() {
			if len(symbols) >= 20 {
				break
			}
			line := scanner.Text()
			norm := normalize.Symbol(line)
			if norm == "" || seen[norm] {
				continue
			}
			seen[norm] = true
			symbols = append(symbols, benchtype.RetrievedSymbol{
				QualifiedName: norm,
				Normalized:    norm,
				Rank:          rank,
			})
			rank++
		}
	}

	latency := time.Since(start).Milliseconds()
	tokensUsed := len(symbols) * 15 // rough estimate: ~15 tokens per symbol result line

	return benchtype.RetrievalResult{
		System:     "grep",
		TaskID:     task.ID,
		Symbols:    symbols,
		TokensUsed: tokensUsed,
		LatencyMs:  latency,
	}, nil
}

func (a *Grep) SupportsLearning() bool { return false }

func (a *Grep) RecordFeedback(_ string, _ benchtype.Task, _ []string) error { return nil }

func (a *Grep) Reset(_ string) error { return nil }

func extractKeywords(description string) []string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "it": true,
		"to": true, "for": true, "in": true, "on": true, "at": true,
		"by": true, "with": true, "from": true, "of": true, "and": true,
		"or": true, "not": true, "that": true, "this": true, "be": true,
		"are": true, "was": true, "were": true, "been": true, "have": true,
		"has": true, "had": true, "do": true, "does": true, "did": true,
		"will": true, "would": true, "should": true, "could": true,
		"can": true, "may": true, "need": true, "want": true, "add": true,
		"fix": true, "change": true, "update": true, "implement": true,
		"refactor": true, "when": true, "how": true, "what": true,
		"where": true, "which": true, "new": true, "make": true,
	}

	words := strings.Fields(strings.ToLower(description))
	var keywords []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'()[]{}#")
		if len(w) < 3 || stopWords[w] {
			continue
		}
		keywords = append(keywords, w)
	}
	if len(keywords) > 5 {
		keywords = keywords[:5]
	}
	return keywords
}
