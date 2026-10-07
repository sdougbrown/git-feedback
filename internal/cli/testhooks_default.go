//go:build !testhooks

package cli

import (
	"os"
	"time"

	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// TestHooks is inert in the default build: no environment overrides are
// honored.
type TestHooks struct{}

// ReadTestHooks is a no-op in the default build.
func ReadTestHooks() TestHooks { return TestHooks{} }

// APIOverride reports no override.
func (TestHooks) APIOverride() (string, error) { return "", nil }

// GHRunner reports no override.
func (TestHooks) GHRunner() github.CommandRunner { return nil }

// Interval reports no override.
func (TestHooks) Interval() (time.Duration, error) { return 0, nil }

// ApplyStoreCrash is a no-op in the default build.
func (TestHooks) ApplyStoreCrash(*store.Store) {}

// CrashBeforeOutput is a no-op in the default build.
func CrashBeforeOutput() {}

// testPendingSignalFn is a nil-safe no-op in the default build.
func testPendingSignalFn() (os.Signal, bool) { return nil, false }
