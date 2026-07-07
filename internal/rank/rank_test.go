package rank

import (
	"reflect"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Render", []string{"render"}},
		{"render_html", []string{"render", "html"}},
		{"RenderHTML", []string{"render", "html"}},
		{"HTTPHandler", []string{"http", "handler"}},
		{"Server.Render", []string{"server", "render"}},
		{"renderHTMLPage", []string{"render", "html", "page"}},
		{"*Server", []string{"server"}},
		{"  whitespace\there  ", []string{"whitespace", "here"}},
	}
	for _, c := range cases {
		got := tokenize(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNameMatchExactBeatsPrefix(t *testing.T) {
	q := tokenize("Render")
	exact := nameMatch(q, "Render")
	prefix := nameMatch(q, "RenderHTML")
	if exact <= prefix {
		t.Errorf("exact match (%.2f) should beat prefix match (%.2f)", exact, prefix)
	}
}

func TestNameMatchShortVariantBeatsLongVariant(t *testing.T) {
	q := tokenize("render")
	short := nameMatch(q, "RenderHTML")   // 1 of 2 def tokens match
	long := nameMatch(q, "preRenderHook") // 1 of 3 def tokens match
	if short <= long {
		t.Errorf("short variant (%.2f) should beat long variant (%.2f)", short, long)
	}
}

func TestNameMatchZeroWhenAbsent(t *testing.T) {
	q := tokenize("authenticate")
	if got := nameMatch(q, "RenderHTML"); got != 0 {
		t.Errorf("want 0 for unrelated names, got %.2f", got)
	}
}

func TestNameMatchMultiTokenQueryRewardsFullCoverage(t *testing.T) {
	q := tokenize("render html")
	full := nameMatch(q, "RenderHTML") // both qt tokens covered
	half := nameMatch(q, "Render")     // 1 of 2 qt tokens covered
	if full <= half {
		t.Errorf("full-coverage match (%.2f) should beat partial (%.2f)", full, half)
	}
}

func TestReceiverMatchPositiveOnly(t *testing.T) {
	q := tokenize("Handler")
	if got := receiverMatch(q, "Handler"); got == 0 {
		t.Errorf("receiver type token in query should boost")
	}
	// No penalty when query doesn't mention receiver type.
	q2 := tokenize("render html")
	if got := receiverMatch(q2, "Server"); got != 0 {
		t.Errorf("verb-noun query should yield 0, not penalty; got %.2f", got)
	}
}

func TestCallerCountMonotonic(t *testing.T) {
	if callerCountScore(0) != 0 {
		t.Errorf("0 callers should score 0")
	}
	if !(callerCountScore(1) < callerCountScore(10) && callerCountScore(10) < callerCountScore(100)) {
		t.Errorf("caller score should be monotonic")
	}
}

func TestBodyOverlapWithConstIDF(t *testing.T) {
	q := tokenize("render html")
	body := "func RenderHTML(ctx context.Context) { html := buildHTML(); _ = html }"
	got := bodyOverlap(q, body, constIDF(1.0))
	// "render" and "html" both appear → 2 * 1.0
	if got != 2.0 {
		t.Errorf("body overlap on two-token match should be 2.0, got %.2f", got)
	}
}

func TestRankSortsByScoreDesc(t *testing.T) {
	cands := []Candidate{
		{Def: store.Definition{Name: "RenderHTML", Body: "render html"}, CallerCount: 10},
		{Def: store.Definition{Name: "Authenticate", Body: "auth token"}, CallerCount: 100},
		{Def: store.Definition{Name: "Render", Body: "render"}, CallerCount: 50},
	}
	out := Rank("render html", cands, PlaceholderIDF, DefaultWeights)
	if len(out) != 3 {
		t.Fatalf("expected 3 results, got %d", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].Score < out[i].Score {
			t.Errorf("results out of order at %d: %.2f < %.2f", i, out[i-1].Score, out[i].Score)
		}
	}
	if out[0].Def.Name == "Authenticate" {
		t.Errorf("unrelated name should not top results despite high caller count; got %s", out[0].Def.Name)
	}
}
