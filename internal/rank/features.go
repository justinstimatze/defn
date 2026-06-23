package rank

import (
	"math"
	"strings"
	"unicode"
)

// tokenize splits a string into lowercase tokens, splitting on non-letter/digit
// runs AND on camelCase boundaries. "HTTPHandler" → ["http", "handler"];
// "render_html" → ["render", "html"]; "Server.Render" → ["server", "render"].
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		// Split on case boundary: lower→Upper, or Upper→Upper followed by lower
		// (so "HTTPHandler" splits as "HTTP" + "Handler").
		if cur.Len() > 0 {
			prev := runes[i-1]
			if unicode.IsLower(prev) && unicode.IsUpper(r) {
				flush()
			} else if i+1 < len(runes) && unicode.IsUpper(prev) && unicode.IsUpper(r) && unicode.IsLower(runes[i+1]) {
				flush()
			}
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// nameMatch scores how well a candidate's identifier name matches the query.
// For each def token, take the best similarity against any query token; sum,
// then divide by max(qLen, dLen). This penalizes both extra def tokens (so
// "Render" beats "RenderHTML" for query "render") and unmatched query tokens
// (so "Render" loses ground to "RenderHTML" for query "render html").
func nameMatch(qTokens []string, name string) float64 {
	if name == "" || len(qTokens) == 0 {
		return 0
	}
	defTokens := tokenize(name)
	if len(defTokens) == 0 {
		return 0
	}
	sum := 0.0
	for _, dt := range defTokens {
		best := 0.0
		for _, qt := range qTokens {
			if s := tokenSimilarity(qt, dt); s > best {
				best = s
			}
		}
		sum += best
	}
	n := len(defTokens)
	if len(qTokens) > n {
		n = len(qTokens)
	}
	return sum / float64(n)
}

func tokenSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1.0
	}
	if strings.HasPrefix(b, a) || strings.HasPrefix(a, b) {
		return 0.7
	}
	if strings.Contains(b, a) || strings.Contains(a, b) {
		return 0.4
	}
	return 0
}

// callerCountScore is a log-scaled boost. Definitions that are called from
// many places are more likely to be the entry point a query is looking for.
// log1p so a hot def with 100 callers scores ~4.6, a leaf with 1 caller ~0.7,
// uncalled defs (often dead code) score 0.
func callerCountScore(n int) float64 {
	if n <= 0 {
		return 0
	}
	return math.Log1p(float64(n))
}

// testCountScore boosts well-tested definitions on the assumption that they
// are load-bearing. Log-scaled like callerCountScore for the same reason.
func testCountScore(n int) float64 {
	if n <= 0 {
		return 0
	}
	return math.Log1p(float64(n))
}

// bodyOverlap is TF-IDF style: sum of IDF scores for query tokens that
// appear in the body. Doesn't penalize long bodies — a def with three
// distinct query terms in its body is more relevant than one with one term,
// even if the long-body def's term frequency is diluted.
//
// History: a sqrt(body length) length-normalization was tried 2026-06-23
// and dropped — it demoted handler functions (the right answers on
// caddy tasks) in favor of small mentions in vars/types. NDCG fell
// 0.113→0.091 on test split. Length normalization stays out unless a
// bigger corpus shows it helps.
func bodyOverlap(qTokens []string, body string, idf IDF) float64 {
	if body == "" || len(qTokens) == 0 {
		return 0
	}
	bodyTokens := tokenize(body)
	if len(bodyTokens) == 0 {
		return 0
	}
	bodySet := make(map[string]struct{}, len(bodyTokens))
	for _, t := range bodyTokens {
		bodySet[t] = struct{}{}
	}
	sum := 0.0
	for _, qt := range qTokens {
		if _, ok := bodySet[qt]; ok {
			sum += idf.Score(qt)
		}
	}
	return sum
}

// receiverMatch is positive-only: boost if any query token matches the
// receiver type, else zero. No penalty when query is verb-noun and doesn't
// mention a type — that's the dominant query shape and a penalty would
// systematically downrank methods on it.
func receiverMatch(qTokens []string, receiver string) float64 {
	if receiver == "" || len(qTokens) == 0 {
		return 0
	}
	// receiver looks like "*Server" or "Server"; tokenize handles both.
	rTokens := tokenize(receiver)
	if len(rTokens) == 0 {
		return 0
	}
	rSet := make(map[string]struct{}, len(rTokens))
	for _, t := range rTokens {
		rSet[t] = struct{}{}
	}
	for _, qt := range qTokens {
		if _, ok := rSet[qt]; ok {
			return 1.0
		}
	}
	return 0
}
