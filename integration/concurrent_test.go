package integration

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// decodeOr decodes an envelope buffer, failing the test when it is not one
// JSON value.
func decodeOr(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v (%q)", err, raw)
	}
	return env
}

// TestSignalCancellation: a wait killed by SIGINT exits 130 and by SIGTERM
// exits 143, in both cases with no stdout output and nothing acknowledged
// (the store gains no acknowledgements and no new events).
func TestSignalCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		code int
	}{
		{"SIGINT", syscall.SIGINT, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStub(t)
			release := stub.hangVerification()
			var once sync.Once
			letThrough := func() { once.Do(func() { close(release) }) }
			t.Cleanup(letThrough)
			env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
			dir := testState(t)

			tagged := filepath.Join(repoRoot, "bin", "git-feedback-test")
			cmd := exec.Command(tagged, waitArgs(dir, "--timeout", "30s")...)
			cmd.Env = env
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}

			// Signal only once the process is blocked inside verification.
			stub.waitForCount(t, "verify", 1)
			time.Sleep(100 * time.Millisecond)
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signal: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				code := 0
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else if err != nil {
					t.Fatalf("wait: %v", err)
				}
				if code != tc.code {
					t.Fatalf("exit code = %d, want %d", code, tc.code)
				}
				if out.Len() != 0 {
					t.Fatalf("stdout = %q, want empty (no envelope after a signal)", out.String())
				}
			case <-time.After(15 * time.Second):
				letThrough()
				_ = cmd.Process.Kill()
				<-done
				t.Fatal("process did not exit after the signal")
			}

			// Nothing was acknowledged: the store never even gained a
			// stream, so a local read must still report the failure.
			after := runCLI(boundedCtx(t), t, tagged, env, inboxArgs(dir)...)
			if statusOf(t, after.Env) != "error" {
				t.Fatalf("inbox after signal status = %q, want error", statusOf(t, after.Env))
			}
		})
	}
}

// TestConcurrentProcesses: two reconcile processes on one store; the first
// holds bootstrap admission and collects, the second is busy. Only one
// collection ever reaches the remote.
func TestConcurrentProcesses(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	tagged := filepath.Join(repoRoot, "bin", "git-feedback-test")

	release := stub.hangVerification()
	var once sync.Once
	letThrough := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letThrough)

	first := exec.Command(tagged, reconcileArgs(dir)...)
	first.Env = env
	var firstOut, firstErr bytes.Buffer
	first.Stdout, first.Stderr = &firstOut, &firstErr
	if err := first.Start(); err != nil {
		t.Fatalf("start first: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Wait() }()

	// The first process is now blocked inside verification, holding
	// bootstrap admission; the second must be refused as busy.
	stub.waitForCount(t, "verify", 1)
	second := runCLI(boundedCtx(t), t, tagged, env, reconcileArgs(dir)...)

	letThrough()
	if err := <-firstDone; err != nil {
		t.Fatalf("first process: %v (stderr=%q)", err, firstErr.String())
	}

	if code := second.Code; code != 0 {
		t.Fatalf("second process exit = %d, want 0 (busy envelope, stderr=%q)", code, second.Stderr)
	}
	if s := statusOf(t, second.Env); s != "busy" {
		t.Fatalf("second process status = %q, want busy", s)
	}
	if s := statusOf(t, decodeOr(t, firstOut.Bytes())); s != "updated" {
		t.Fatalf("first process status = %q, want updated", s)
	}
	if got := stub.count("threads"); got != 1 {
		t.Fatalf("collections = %d, want exactly 1", got)
	}
}
