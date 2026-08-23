package mcp

import (
	"os"
	"testing"
	"time"
)

// TestServeIdleTimeout_DefaultsTo45m guards #351: an unset
// DEFN_SERVE_IDLE_TIMEOUT must default to a real, positive timeout
// (45m) rather than accidentally disabling the idle-shutdown check --
// the whole point of the feature is that a forgotten defn serve process
// self-terminates without any configuration required.
func TestServeIdleTimeout_DefaultsTo45m(t *testing.T) {
	t.Setenv("DEFN_SERVE_IDLE_TIMEOUT", "")
	os.Unsetenv("DEFN_SERVE_IDLE_TIMEOUT")
	got := serveIdleTimeout()
	if got != 45*time.Minute {
		t.Errorf("serveIdleTimeout() with unset env = %v, want 45m", got)
	}
}

func TestServeIdleTimeout_ExplicitValueHonored(t *testing.T) {
	t.Setenv("DEFN_SERVE_IDLE_TIMEOUT", "10m")
	got := serveIdleTimeout()
	if got != 10*time.Minute {
		t.Errorf("serveIdleTimeout() = %v, want 10m", got)
	}
}

// TestServeIdleTimeout_ZeroOrNegativeDisables guards the explicit
// opt-out path: DEFN_SERVE_IDLE_TIMEOUT=0 (or any non-positive
// duration) must disable the check entirely (returns 0), not be
// treated as "shut down immediately."
func TestServeIdleTimeout_ZeroOrNegativeDisables(t *testing.T) {
	for _, v := range []string{"0", "0s", "-1m"} {
		t.Setenv("DEFN_SERVE_IDLE_TIMEOUT", v)
		if got := serveIdleTimeout(); got != 0 {
			t.Errorf("serveIdleTimeout() with %q = %v, want 0 (disabled)", v, got)
		}
	}
}

func TestServeIdleTimeout_UnparseableDisables(t *testing.T) {
	t.Setenv("DEFN_SERVE_IDLE_TIMEOUT", "not-a-duration")
	if got := serveIdleTimeout(); got != 0 {
		t.Errorf("serveIdleTimeout() with garbage value = %v, want 0 (disabled)", got)
	}
}

// TestIdleCheckInterval_BoundedBothWays guards #351's poll-interval
// picker: too-short a check interval on a long timeout is harmless but
// wasteful; too-long a check interval on a short timeout (e.g. a fast
// test) would make idle-shutdown itself sluggish to observe.
func TestIdleCheckInterval_BoundedBothWays(t *testing.T) {
	if got := idleCheckInterval(1 * time.Second); got != 10*time.Second {
		t.Errorf("idleCheckInterval(1s) = %v, want floor 10s", got)
	}
	if got := idleCheckInterval(1 * time.Hour); got != 5*time.Minute {
		t.Errorf("idleCheckInterval(1h) = %v, want ceiling 5m", got)
	}
	if got := idleCheckInterval(100 * time.Minute); got != 5*time.Minute {
		t.Errorf("idleCheckInterval(100m) = %v, want ceiling 5m (100m/10=10m exceeds it)", got)
	}
	if got := idleCheckInterval(45 * time.Minute); got != 4*time.Minute+30*time.Second {
		t.Errorf("idleCheckInterval(45m) = %v, want 4m30s (45m/10, within bounds)", got)
	}
}
