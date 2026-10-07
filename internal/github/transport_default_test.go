package github

import (
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
)

// TestNewTransportDefaultAPIBase checks that an empty API base selects the
// GitHub API default, so the production build works without test overrides,
// while a malformed non-empty base still fails.
func TestNewTransportDefaultAPIBase(t *testing.T) {
	tr, err := NewTransport("", "tok", NewFixedPacer(&clock.Fake{}, time.Second), &MemoryGate{}, &clock.Fake{})
	if err != nil {
		t.Fatalf("NewTransport(\"\"): %v", err)
	}
	if tr.apiURL.Host != "api.github.com" {
		t.Fatalf("apiURL.Host = %q, want api.github.com", tr.apiURL.Host)
	}

	if _, err := NewTransport("://bad", "tok", NewFixedPacer(&clock.Fake{}, time.Second), &MemoryGate{}, &clock.Fake{}); err == nil {
		t.Fatal("NewTransport(\"://bad\"): want error, got nil")
	}
}
