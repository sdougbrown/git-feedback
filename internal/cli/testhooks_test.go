//go:build testhooks

package cli

import (
	"testing"
	"time"
)

// TestAPIOverrideRejectsInvalidBase exercises the APIOverride rejection
// branches that integration tests never hit: a non-loopback host, a URL
// with no port, and an unparseable URL. A regression that silently
// accepted a non-loopback API base (SSRF-prone) would fail here.
func TestAPIOverrideRejectsInvalidBase(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"non-loopback host", "http://10.0.0.1:8080"},
		{"missing port", "https://api.github.com"},
		{"unparseable url", "://bad"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_FEEDBACK_API_BASE_URL", tc.value)
			h := ReadTestHooks()
			if _, err := h.APIOverride(); err == nil {
				t.Fatalf("APIOverride(%q) = nil error, want error", tc.value)
			}
		})
	}
}

// TestAPIOverrideAcceptsLoopback verifies a valid loopback base is accepted
// and returned unchanged.
func TestAPIOverrideAcceptsLoopback(t *testing.T) {
	const want = "http://127.0.0.1:8080"
	t.Setenv("GIT_FEEDBACK_API_BASE_URL", want)
	h := ReadTestHooks()
	got, err := h.APIOverride()
	if err != nil {
		t.Fatalf("APIOverride(%q) = %v, want nil", want, err)
	}
	if got != want {
		t.Fatalf("APIOverride = %q, want %q", got, want)
	}
}

// TestIntervalRejectsInvalidDuration exercises the Interval parse-error
// branch that integration tests never hit.
func TestIntervalRejectsInvalidDuration(t *testing.T) {
	t.Setenv("GIT_FEEDBACK_MIN_INTERVAL", "not-a-duration")
	h := ReadTestHooks()
	if _, err := h.Interval(); err == nil {
		t.Fatal("Interval(not-a-duration) = nil error, want error")
	}
}

// TestIntervalAcceptsValidDuration verifies a valid cadence override parses
// to the expected duration.
func TestIntervalAcceptsValidDuration(t *testing.T) {
	t.Setenv("GIT_FEEDBACK_MIN_INTERVAL", "2s")
	h := ReadTestHooks()
	got, err := h.Interval()
	if err != nil {
		t.Fatalf("Interval(2s) = %v, want nil", err)
	}
	if got != 2*time.Second {
		t.Fatalf("Interval = %v, want 2s", got)
	}
}
