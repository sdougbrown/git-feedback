//go:build !testhooks

package cli

// TestHooks is inert in the default build: no environment overrides are
// honored.
type TestHooks struct{}

// ReadTestHooks is a no-op in the default build.
func ReadTestHooks() TestHooks { return TestHooks{} }
