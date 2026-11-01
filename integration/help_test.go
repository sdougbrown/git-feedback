package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestVersionFlag verifies that the production binary prints the plain
// version line: the "git-feedback <version>" shape on stdout, exit 0, no
// JSON envelope. This exercises the cmd/git-feedback wiring (the
// cli.SetVersion call) through the real binary; the ldflags value itself is
// not assertable from tests.
func TestVersionFlag(t *testing.T) {
	prod, _ := binPaths(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prod, "--version")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	stdout := string(out)
	if !strings.HasPrefix(stdout, "git-feedback ") {
		t.Errorf("--version output = %q, want the git-feedback prefix", stdout)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Error("--version printed a JSON envelope")
	}
}

// TestTopLevelHelp verifies that the production binary prints the
// self-describing help: the flow block, the envelope contract, and the
// contract pointer appear on stdout, and the exit code is 0. The help output
// is plain text, so the run does not go through the envelope-parsing helper.
func TestTopLevelHelp(t *testing.T) {
	prod, _ := binPaths(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prod, "--help")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	stdout := string(out)
	for _, want := range []string{
		"reconcile",
		"collect feedback and establish the baseline",
		"acknowledge delivered event IDs",
		"reconcile → wait → ack",
		"acknowledges nothing",
		"Operational errors exit 1, usage errors",
		"docs/contract.md",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help output missing %q", want)
		}
	}

}
