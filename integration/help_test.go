package integration

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTopLevelHelp verifies that the production binary prints the
// self-describing help: the flow block, the envelope contract, and the
// contract pointer appear on stdout, and the exit code is 0.
func TestTopLevelHelp(t *testing.T) {
	prod, _ := binPaths(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := runCLI(ctx, t, prod, nil, "--help")
	if res.Code != 0 {
		t.Fatalf("exit code = %d, want 0", res.Code)
	}
	for _, want := range []string{
		"reconcile     collect feedback and establish the baseline",
		"ack           acknowledge delivered event IDs",
		"reconcile → wait → ack",
		"wait itself",
		"Operational errors exit 1, usage errors",
		"docs/contract.md",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}
