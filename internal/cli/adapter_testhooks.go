//go:build testhooks

package cli

import "os"

// TestSeams on the adapter: the crash point and signal injection exist only
// in testhooks builds; production builds skip them.
func (cliAdapter) BeforeWrite() {
	CrashBeforeOutput()
}

func (cliAdapter) PendingSignal() (os.Signal, bool) {
	return testPendingSignalFn()
}
