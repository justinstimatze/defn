package mcp

import (
	"os"
	"testing"
)

// TestEnvDisabled_TruthyValues covers #201's envDisabled helper: the
// falsy spellings (case-insensitive) that opt a graduated-LLM flag
// out, and everything else (unset, "1", "true", garbage) that must
// leave the feature enabled -- these are opt-OUT flags, so absence
// is the enabled-by-default state.
func TestEnvDisabled_TruthyValues(t *testing.T) {
	disabling := []string{"0", "false", "off", "no", "FALSE", "Off", "NO"}
	for _, v := range disabling {
		t.Setenv("DEFN_TEST_FLAG", v)
		if !envDisabled("DEFN_TEST_FLAG") {
			t.Errorf("envDisabled(%q) = false, want true", v)
		}
	}

	enabling := []string{"", "1", "true", "on", "yes", "garbage"}
	for _, v := range enabling {
		t.Setenv("DEFN_TEST_FLAG", v)
		if envDisabled("DEFN_TEST_FLAG") {
			t.Errorf("envDisabled(%q) = true, want false", v)
		}
	}
}

// TestEnvDisabled_UnsetMeansEnabled confirms the actually-unset case
// (not just empty string) also means "not disabled" -- the default,
// no-env-vars-set state every existing defn install is in today.
func TestEnvDisabled_UnsetMeansEnabled(t *testing.T) {
	os.Unsetenv("DEFN_TEST_FLAG_UNSET")
	if envDisabled("DEFN_TEST_FLAG_UNSET") {
		t.Error("envDisabled on an unset var should be false (not disabled)")
	}
}
