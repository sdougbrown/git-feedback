package cli

import (
	"os"
	"syscall"
)

// Process exit codes.
const (
	// ExitOK reports success (including envelope statuses that are not
	// errors, such as deferred, busy, or timeout).
	ExitOK = 0
	// ExitOperational reports an operational failure (error envelope).
	ExitOperational = 1
	// ExitUsage reports a caller error such as a bad flag or URL (error
	// envelope still printed).
	ExitUsage = 2
	// ExitEvents and ExitHeadChanged report a wait outcome to a monitor that
	// can only observe process exit. They apply only to `wait --exit-on-events`.
	// Delivered events and a moved pinned head are not failures, so their codes
	// sit above the error codes and never collide with them. The envelope stays
	// authoritative; these codes are a signalling aid.
	ExitEvents      = 3
	ExitHeadChanged = 4
)

// SignalExitCode maps terminating signals to shell-style exit codes:
// SIGINT exits 130 and SIGTERM exits 143.
func SignalExitCode(sig os.Signal) int {
	switch sig {
	case os.Interrupt:
		return 130
	case syscall.SIGTERM:
		return 143
	default:
		return ExitOperational
	}
}
