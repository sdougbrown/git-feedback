package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// renderHelp runs one help request through Run and returns the plain help
// text on stdout. Help exits 0 without an envelope.
func renderHelp(t *testing.T, argv []string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(argv, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("%v: exit code = %d, want 0 (stderr: %s)", argv, code, stderr.String())
	}
	return stdout.String()
}

// TestUsageContent pins the agent-facing content of the bare help: the
// per-command descriptions, the typical flow, and the envelope/exit-code
// contract that registry and harness callers key on. The generated Usage
// and Flags sections come from the cobra command tree, so this also guards
// the specs-to-help wiring.
func TestUsageContent(t *testing.T) {
	help := renderHelp(t, []string{"--help"})
	for _, want := range []string{
		"git-feedback observes GitHub review feedback",
		"collect feedback and establish the baseline",
		"export a stored collection to a file",
		"list pending events for a consumer",
		"fetch one event's stored object content",
		"acknowledge delivered event IDs",
		"block until new events arrive or --timeout elapses",
		"disposable store utilities",
		"reconcile → wait → ack",
		"issue URL (/issues/N)",
		"--skip-head-changed, and",
		"--exit-on-events",
		"when it delivers events and 4 when a pinned head moved",
		"acknowledges nothing",
		`exactly one JSON envelope with a "status" field`,
		"Operational errors exit 1, usage errors",
		"key on \"status\",",
		"docs/contract.md",
		"--version",
		"--help",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("Usage is missing %q", want)
		}
	}
}

// TestSubcommandHelp pins the per-subcommand help surface: every command
// answers --help on its own with its synopsis and generated flag docs, and
// exits 0 without an envelope.
func TestSubcommandHelp(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"inbox", "--help"}, []string{"inbox <URL>", "list pending events for a consumer", "--consumer", "name identifying this reader", "--limit", "--ids-only", "--after", "--include-self", "--state-dir"}},
		{[]string{"ack", "--help"}, []string{"ack <URL>", "acknowledge delivered event IDs", "--event", "event ID to acknowledge", "--events-from", "read event IDs from a file, or - for stdin"}},
		{[]string{"wait", "--help"}, []string{"wait <URL>", "--timeout", "maximum wait duration (default 30m)", "--skip-head-changed", "exit-on-events", "exit 3 when events are delivered"}},
		{[]string{"reconcile", "--help"}, []string{"reconcile <URL>", "collect feedback and establish the baseline", "--head", "expected head SHA"}},
		{[]string{"store", "reset", "--help"}, []string{"store reset", "--target", "scope the reset to one target URL"}},
	} {
		help := renderHelp(t, tc.argv)
		for _, want := range tc.want {
			if !strings.Contains(help, want) {
				t.Errorf("%v: help is missing %q", tc.argv, want)
			}
		}
		// Help is plain text: no JSON envelope leaks into stdout.
		if strings.HasPrefix(strings.TrimSpace(help), "{") {
			t.Errorf("%v: help output starts with a JSON envelope", tc.argv)
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
		for _, want := range []string{
			"path",
			"reset",
			"print the resolved store directory; never opens the store",
			"delete the store, wholly or scoped to one --target URL",
			"The store is a cache",
			"docs/contract.md",
		} {
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
	errObj, ok := envelope["error"].(map[string]any)
	if !ok || errObj["code"] != "usage" {
		t.Errorf("plain store: error envelope = %v, want code usage", envelope["error"])
	}
}
