package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
		"reconcile     collect feedback and establish the baseline",
		"ack           acknowledge delivered event IDs",
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
