//go:build testhooks

package cli

import "os"

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
