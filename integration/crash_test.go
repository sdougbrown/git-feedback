package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// crashEnv returns the test environment with the crash point enabled.
func crashEnv(fakeGH, apiURL, crashAt string) []string {
	return testEnv(fakeGH, apiURL, map[string]string{"GIT_FEEDBACK_CRASH_AT": crashAt})
}

// runCrash runs one bounded subprocess invocation expected to die.
func runCrash(t *testing.T, bin string, env []string, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return runCLI(ctx, t, bin, env, args...)
}

// assertCrashed fails unless the run died without any stdout output.
func assertCrashed(t *testing.T, res cliResult, what string) {
	t.Helper()
	if res.Code == 0 {
		t.Fatalf("%s exited 0, want a crash (stdout=%q)", what, res.Stdout)
	}
	if res.Stdout != "" {
		t.Fatalf("%s wrote stdout before dying: %q", what, res.Stdout)
	}
}

// TestCrashAfterCommitReplays: a process that dies right after the durable
// commit (its output lost entirely) leaves the events replayable: a second
// process receives the same event.
func TestCrashAfterCommitReplays(t *testing.T) {
	stub := newStub(t)
	env := crashEnv(fakeGHPath(t), stub.srv.URL, "after_commit")
	dir := testState(t)

	res := runCrash(t, filepath.Join(repoRoot, "bin", "git-feedback-test"), env, reconcileArgs(dir)...)
	assertCrashed(t, res, "after_commit crash")

	// The lost output is irrelevant: the commit is durable and replayable.
	deliver := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	envOut := runOK(t, deliver, 30*time.Second, waitArgs(dir, "--timeout", "5s")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events replayed after the crash", s)
	}
	if ids := eventIDs(t, envOut); len(ids) == 0 {
		t.Fatalf("wait delivered no events after the crash")
	}
}

// TestCrashBeforeOutput: the before-output crash point dies after the result
// is computed but before the envelope is written; the committed events are
// still replayable.
func TestCrashBeforeOutput(t *testing.T) {
	stub := newStub(t)
	env := crashEnv(fakeGHPath(t), stub.srv.URL, "before_output")
	dir := testState(t)

	res := runCrash(t, filepath.Join(repoRoot, "bin", "git-feedback-test"), env, reconcileArgs(dir)...)
	assertCrashed(t, res, "before_output crash")

	deliver := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	envOut := runOK(t, deliver, 30*time.Second, waitArgs(dir, "--timeout", "5s")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events replayed after the crash", s)
	}
}

// TestDefaultBuildIgnoresHooks: the production binary must ignore every
// override: no request reaches the fake server, no crash point fires, and
// the command fails plainly (the credential helper is absent from PATH).
func TestDefaultBuildIgnoresHooks(t *testing.T) {
	stub := newStub(t)
	prod, _ := binPaths(t)
	dir := testState(t)

	// Empty PATH keeps the real gh unresolvable, so the run can never
	// reach the real GitHub API either.
	env := append(testEnv(fakeGHPath(t), stub.srv.URL, map[string]string{
		"GIT_FEEDBACK_CRASH_AT": "after_commit",
	}), "PATH=")

	res := runCrash(t, prod, env, reconcileArgs(dir)...)

	if res.Code != 1 {
		t.Fatalf("default build exit = %d, want 1 (operational error, no crash)", res.Code)
	}
	for key := range stub.counts {
		if got := stub.count(key); got != 0 {
			t.Fatalf("default build sent %d request(s) to the fake server (%s); overrides are not ignored", got, key)
		}
	}
	if s := statusOf(t, res.Env); s != "error" {
		t.Fatalf("default build status = %q, want error", s)
	}
}
