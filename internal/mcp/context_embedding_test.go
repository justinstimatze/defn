package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/store"
)

// TestContextEmbeddingCandidates_BelowThresholdExcluded confirms an
// unrelated question doesn't drag in unrelated defs just because the
// corpus is small.
func TestContextEmbeddingCandidates_BelowThresholdExcluded(t *testing.T) {
	s := setupEmbeddingFixture(t)
	cands := s.contextEmbeddingCandidates("compute a checksum for a binary artifact", map[int64]contextCandidate{})
	for _, c := range cands {
		if c.Def.Name == "VerifyLoginToken" {
			t.Errorf("expected unrelated question not to match VerifyLoginToken, got score %v", c.EmbeddingScore)
		}
	}
}

// TestContextEmbeddingCandidates_ExcludesAlreadySeen confirms defs
// already found by token-based search aren't duplicated.
func TestContextEmbeddingCandidates_ExcludesAlreadySeen(t *testing.T) {
	s := setupEmbeddingFixture(t)

	d, err := s.backend.GetDefinitionByName("VerifyLoginToken", "")
	if err != nil {
		t.Fatalf("GetDefinitionByName: %v", err)
	}
	seen := map[int64]contextCandidate{d.ID: {Def: *d, FromName: true}}

	cands := s.contextEmbeddingCandidates("verify a login token", seen)
	for _, c := range cands {
		if c.Def.ID == d.ID {
			t.Errorf("expected already-seen def to be excluded, got it back: %+v", c)
		}
	}
}

// TestContextEmbeddingCandidates_ExcludesTestDefs confirms _test.go
// defs never enter the candidate set via the embedding path, matching
// the exclusion token-based search already effectively applies via
// contextRank's test penalty (here excluded outright instead).
func TestContextEmbeddingCandidates_ExcludesTestDefs(t *testing.T) {
	s := setupEmbeddingFixture(t)
	// setupTestDB's fixture ships TestGreet / TestFarewell.
	cands := s.contextEmbeddingCandidates("greet farewell", map[int64]contextCandidate{})
	for _, c := range cands {
		if c.Def.Test {
			t.Errorf("expected no test defs among embedding candidates, got: %+v", c)
		}
	}
}

// TestContextEmbeddingCandidates_FindsSimilarDefNotInSeen is the #198
// regression: a def not present in the token-based candidate set
// (empty seen here, simulating "token search found nothing relevant")
// is still surfaced when its embedding is close enough to the
// question's.
func TestContextEmbeddingCandidates_FindsSimilarDefNotInSeen(t *testing.T) {
	s := setupEmbeddingFixture(t)

	cands := s.contextEmbeddingCandidates("verify a login token", map[int64]contextCandidate{})
	found := false
	for _, c := range cands {
		if c.Def.Name == "VerifyLoginToken" {
			found = true
			if !c.FromEmbedding {
				t.Error("expected FromEmbedding=true")
			}
			if c.EmbeddingScore < contextEmbeddingThreshold {
				t.Errorf("expected EmbeddingScore >= threshold, got %v", c.EmbeddingScore)
			}
		}
	}
	if !found {
		t.Errorf("expected VerifyLoginToken among embedding candidates, got: %+v", cands)
	}
}

// TestContextRank_EmbeddingBonusOnlyWithoutTokenOverlap locks
// contextRank's #198 scoring rule: the embedding bonus only applies
// when a candidate has zero token-based signal, so a def already
// found via name/summary/sig matching doesn't get an extra,
// noisier bump stacked on top of an already-strong score.
func TestContextRank_EmbeddingBonusOnlyWithoutTokenOverlap(t *testing.T) {
	tokens := []string{"greet"}
	cands := []contextCandidate{
		{
			Def:            store.Definition{Name: "EmbeddingOnly"},
			FromEmbedding:  true,
			EmbeddingScore: 0.8,
		},
		{
			Def:            store.Definition{Name: "GreetSomeone"},
			FromEmbedding:  true,
			EmbeddingScore: 0.8,
		},
	}
	ranked := contextRank(cands, tokens, nil)

	var embeddingOnlyScore, nameHitScore int
	for _, c := range ranked {
		switch c.Def.Name {
		case "EmbeddingOnly":
			embeddingOnlyScore = c.Score
		case "GreetSomeone":
			nameHitScore = c.Score
		}
	}
	if embeddingOnlyScore == 0 {
		t.Error("expected the embedding-only candidate to get a nonzero score bonus")
	}
	// GreetSomeone matches "greet" by name (8 points) -- the embedding
	// bonus must NOT stack on top of that, so its score should equal
	// the name-hit score alone (8), not 8 + int(0.8*8)=14.
	if nameHitScore != 8 {
		t.Errorf("expected name-hit score of 8 (no embedding bonus stacked), got %d", nameHitScore)
	}
}

// setupEmbeddingFixture seeds setupTestDB's project with a def whose
// name/doc use vocabulary a hand-picked test question shares only
// after camelCase decomposition, then re-ingests it.
func setupEmbeddingFixture(t *testing.T) *server {
	t.Helper()
	db, projDir := setupTestDB(t)
	t.Cleanup(func() { db.Close() })

	src := `package main

// VerifyLoginToken checks that a supplied login token is still valid
// before granting access to a protected resource.
func VerifyLoginToken(token string) bool {
	return len(token) > 0
}
`
	mainPath := filepath.Join(projDir, "auth.go")
	if err := os.WriteFile(mainPath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.IngestFile(db, projDir, mainPath); err != nil {
		t.Fatal("re-ingest:", err)
	}

	return &server{backend: db, projectDir: projDir}
}

// TestContextRank_RareTokenOutranksCommonMultiTokenMatch is the
// regression for a real v18 bench trajectory (prometheus-17395): a
// question about "add lightsail unit test" ranked
// TestAddTypeAndUnitLabels and several other totally unrelated defs
// ABOVE every actual Lightsail/EC2 def, because plain per-token
// counting gave "add"/"unit"/"test" (near-ubiquitous across any Go
// repo's test names) the same per-hit weight as "lightsail" (a
// handful of matches total) -- three generic hits outscored one
// perfectly on-topic rare one. tokenDF simulates that codebase-wide
// frequency; contextRank must weight rare tokens enough that the
// on-topic def still wins.
func TestContextRank_RareTokenOutranksCommonMultiTokenMatch(t *testing.T) {
	tokens := []string{"add", "unit", "test", "lightsail"}
	tokenDF := map[string]int{
		"add":       200,
		"unit":      150,
		"test":      500,
		"lightsail": 3,
	}
	cands := []contextCandidate{
		{Def: store.Definition{Name: "TestAddTypeAndUnitLabels", Test: true}},
		{Def: store.Definition{Name: "LightsailDiscovery"}},
	}

	ranked := contextRank(cands, tokens, tokenDF)

	if ranked[0].Def.Name != "LightsailDiscovery" {
		t.Fatalf("expected LightsailDiscovery (rare on-topic token) to rank first, got %q first (scores: %d, %d)",
			ranked[0].Def.Name, ranked[0].Score, ranked[1].Score)
	}
}
