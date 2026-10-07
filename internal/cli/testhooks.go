//go:build testhooks

package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// TestHooks carries environment overrides that exist only in test builds.
// They let the harness redirect API traffic, substitute a fake gh binary,
// tighten pacing, and trigger crash points. The default build ignores all
// overrides.
type TestHooks struct {
	APIBaseURL  string
	GHBin       string
	MinInterval string
	CrashAt     string
}

// ReadTestHooks reads the test-only environment overrides.
func ReadTestHooks() TestHooks {
	return TestHooks{
		APIBaseURL:  os.Getenv("GIT_FEEDBACK_API_BASE_URL"),
		GHBin:       os.Getenv("GIT_FEEDBACK_GH_BIN"),
		MinInterval: os.Getenv("GIT_FEEDBACK_MIN_INTERVAL"),
		CrashAt:     os.Getenv("GIT_FEEDBACK_CRASH_AT"),
	}
}

// APIOverride returns the validated loopback API base override, or "".
func (h TestHooks) APIOverride() (string, error) {
	if h.APIBaseURL == "" {
		return "", nil
	}
	u, err := url.Parse(h.APIBaseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid GIT_FEEDBACK_API_BASE_URL %q: not an absolute URL", h.APIBaseURL)
	}
	if u.Hostname() != "127.0.0.1" || u.Port() == "" {
		return "", fmt.Errorf("invalid GIT_FEEDBACK_API_BASE_URL %q: host must be 127.0.0.1 with a port", h.APIBaseURL)
	}
	return h.APIBaseURL, nil
}

// GHRunner returns a CommandRunner that executes the fake gh path, or nil.
func (h TestHooks) GHRunner() github.CommandRunner {
	if h.GHBin == "" {
		return nil
	}
	return ghBinRunner{path: h.GHBin}
}

// ghBinRunner redirects the gh credential lookup to a fake gh binary.
type ghBinRunner struct{ path string }

func (r ghBinRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "gh" {
		name = r.path
	}
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// Interval returns the parsed cadence override, or 0.
func (h TestHooks) Interval() (time.Duration, error) {
	if h.MinInterval == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(h.MinInterval)
	if err != nil {
		return 0, fmt.Errorf("invalid GIT_FEEDBACK_MIN_INTERVAL %q: %v", h.MinInterval, err)
	}
	return d, nil
}

// ApplyStoreCrash wires the after-commit crash point onto the store.
func (h TestHooks) ApplyStoreCrash(st *store.Store) {
	if h.CrashAt == "after_commit" {
		st.Hooks.AfterPublishCommit = func() {
			panic("GIT_FEEDBACK_CRASH_AT=after_commit")
		}
	}
}

// CrashBeforeOutput implements the before-output crash point: the result is
// computed but the envelope is never written.
func CrashBeforeOutput() {
	if os.Getenv("GIT_FEEDBACK_CRASH_AT") == "before_output" {
		panic("GIT_FEEDBACK_CRASH_AT=before_output")
	}
}

// testPendingSignal is a test-only injection: when non-nil, Run injects it
// into the signal channel before the envelope write, exercising the
// post-write signal re-check. Only exists in testhooks builds.
var testPendingSignal os.Signal

// testPendingSignalFn returns the pending signal to inject, if a test set it.
func testPendingSignalFn() (os.Signal, bool) {
	if testPendingSignal == nil {
		return nil, false
	}
	return testPendingSignal, true
}
