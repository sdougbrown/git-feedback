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
