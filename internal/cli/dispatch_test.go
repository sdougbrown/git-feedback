//go:build testhooks

package cli

import (
	"bytes"
	"context"
	"os"
	"syscall"
	"testing"
)

// TestSignalPendingAfterHandlerWinsExitCode: a signal that is pending after
// the handler returns (injected just before the envelope write, so the
// pre-write check misses it) still wins on exit code via the post-write
// re-check, even though the envelope may already be written.
func TestSignalPendingAfterHandlerWinsExitCode(t *testing.T) {
	// A handler that succeeds, so the injected signal is the only thing
	// that should drive the exit code.
	// Stubbing specs is process-global: safe only because no test in this
	// package uses t.Parallel(). Keep it that way, or make the stub local.
	old := specs["reconcile"]
	stub := old
	stub.handle = func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{Command: "reconcile", Status: StatusUpdated}, nil
	}
	specs["reconcile"] = stub
	t.Cleanup(func() { specs["reconcile"] = old })

	for _, tc := range []struct {
		name string
		sig  os.Signal
		code int
	}{
		{"SIGINT", os.Interrupt, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testPendingSignal = tc.sig
			defer func() { testPendingSignal = nil }()

			var stdout, stderr bytes.Buffer
			code := Run([]string{"reconcile", "https://github.com/o/r/pull/1"}, &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, tc.code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestWaitExitOnEventsCodes: --exit-on-events is the pinned status-driven exit
// convention for process monitors. It applies to wait only. The envelope and
// the stderr summary stay unchanged, and no error code is reused.
func TestWaitExitOnEventsCodes(t *testing.T) {
	// Stubbing specs is process-global: safe only because no test in this
	// package uses t.Parallel().
	old := specs["wait"]
	stub := old
	status := ""
	stub.handle = func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{Command: "wait", Status: status}, nil
	}
	specs["wait"] = stub
	t.Cleanup(func() { specs["wait"] = old })

	for _, tc := range []struct {
		name   string
		status string
		flag   bool
		want   int
	}{
		{"events", StatusEvents, true, ExitEvents},
		{"head_changed", StatusHeadChange, true, ExitHeadChanged},
		{"timeout", StatusTimeout, true, ExitOK},
		{"events without the flag", StatusEvents, false, ExitOK},
		{"head_changed without the flag", StatusHeadChange, false, ExitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status = tc.status
			argv := []string{"wait", "https://github.com/o/r/pull/1", "--consumer", "c"}
			if tc.flag {
				argv = append(argv, "--exit-on-events")
			}
			var stdout, stderr bytes.Buffer
			code := Run(argv, &stdout, &stderr)
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, tc.want, stdout.String(), stderr.String())
			}
			// The code is a signalling aid: the envelope is still the record.
			if stdout.Len() == 0 {
				t.Fatalf("no envelope written for status %q", tc.status)
			}
		})
	}

	// The error codes keep their meaning whatever the status says.
	status = StatusEvents
	stub.handle = func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{}, &usageError{"wait requires --consumer <NAME>"}
	}
	specs["wait"] = stub
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"wait", "https://github.com/o/r/pull/1", "--exit-on-events"}, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("usage error exit = %d, want %d", code, ExitUsage)
	}
}

// TestWaitExitOnEventsYieldsToSignals: a terminating signal wins over the
// status-driven code. A killed wait must never read as delivered events.
func TestWaitExitOnEventsYieldsToSignals(t *testing.T) {
	// Stubbing specs is process-global: safe only because no test in this
	// package uses t.Parallel().
	old := specs["wait"]
	stub := old
	stub.handle = func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{Command: "wait", Status: StatusEvents}, nil
	}
	specs["wait"] = stub
	t.Cleanup(func() { specs["wait"] = old })

	for _, tc := range []struct {
		name string
		sig  os.Signal
		code int
	}{
		{"SIGINT", os.Interrupt, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testPendingSignal = tc.sig
			defer func() { testPendingSignal = nil }()

			var stdout, stderr bytes.Buffer
			code := Run([]string{"wait", "https://github.com/o/r/pull/1", "--consumer", "c", "--exit-on-events"}, &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, tc.code, stdout.String(), stderr.String())
			}
		})
	}
}
