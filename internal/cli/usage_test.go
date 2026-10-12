package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestUsageContent pins the agent-facing content of the bare help: the
// per-command descriptions, the typical flow, and the envelope/exit-code
// contract that registry and harness callers key on.
func TestUsageContent(t *testing.T) {
	for _, want := range []string{
		"git-feedback observes GitHub review feedback",
		"reconcile <URL>",
		"snapshot <URL>",
		"inbox <URL>",
		"ack <URL>",
		"wait <URL>",
		"store path",
		"store reset",
		"reconcile     collect feedback and establish the baseline",
		"wait          block until new events arrive or --timeout elapses",
		"store path    print the resolved store directory; never opens the store",
		"reconcile → wait → ack",
		"wait itself\n  acknowledges nothing",
		`exactly one JSON envelope with a "status" field`,
		"Operational errors exit 1, usage errors\n  exit 2",
		"key on \"status\", not the exit\n  code",
		"docs/contract.md",
		"--version",
		"--help",
	} {
		if !strings.Contains(Usage, want) {
			t.Errorf("Usage is missing %q", want)
		}
	}
}

// TestStoreHelpRouting verifies that --help on the store command prints the
// store-specific help on stdout and exits 0, while plain `store` keeps the
// missing-subcommand usage error (exit 2, error envelope).
func TestStoreHelpRouting(t *testing.T) {
	for _, argv := range [][]string{{"store", "--help"}, {"store", "-h"}} {
		var stdout, stderr bytes.Buffer
		code := Run(argv, &stdout, &stderr)
		if code != ExitOK {
			t.Fatalf("%v: exit code = %d, want 0 (stderr: %s)", argv, code, stderr.String())
		}
		if stdout.String() != StoreUsage {
			t.Errorf("%v: stdout is not the store help text: %q", argv, stdout.String())
		}
		for _, want := range []string{"store path", "store reset", "never opens the store", "docs/contract.md"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%v: store help is missing %q", argv, want)
			}
		}
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"store"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("plain store: exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("plain store: stdout is not one JSON value: %v (%q)", err, stdout.String())
	}
	if envelope["status"] != StatusError {
		t.Errorf("plain store: status = %v, want error", envelope["status"])
	}
}
