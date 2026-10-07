package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestRegistryArgvOrdering(t *testing.T) {
	const url = "https://github.com/owner/repo/pull/7"

	t.Run("flags-first with prefix json, account, and head", func(t *testing.T) {
		inv, err := parseInvocation([]string{
			"reconcile", "--json", "--account", "Alice", "--head", "abc123", url,
		})
		if err != nil {
			t.Fatalf("parseInvocation: %v", err)
		}
		if inv.Command != "reconcile" || inv.URL != url {
			t.Fatalf("inv = %+v", inv)
		}
		if inv.Flags["account"] != "Alice" {
			t.Errorf("account = %q", inv.Flags["account"])
		}
		if inv.Flags["head"] != "abc123" {
			t.Errorf("head = %q", inv.Flags["head"])
		}
		if inv.handler == nil {
			t.Error("handler not bound")
		}
	})

	t.Run("url-first ordering", func(t *testing.T) {
		inv, err := parseInvocation([]string{
			"reconcile", url, "--head", "abc123", "--json",
		})
		if err != nil {
			t.Fatalf("parseInvocation: %v", err)
		}
		if inv.URL != url || inv.Flags["head"] != "abc123" {
			t.Fatalf("inv = %+v", inv)
		}
	})

	t.Run("repeated event flags", func(t *testing.T) {
		inv, err := parseInvocation([]string{
			"ack", "--consumer", "ci", "--event=e1", "--event=e2", "--account", "alice", url,
		})
		if err != nil {
			t.Fatalf("parseInvocation: %v", err)
		}
		if inv.URL != url || inv.Flags["consumer"] != "ci" || inv.Flags["account"] != "alice" {
			t.Fatalf("inv = %+v", inv)
		}
		if len(inv.Events) != 2 || inv.Events[0] != "e1" || inv.Events[1] != "e2" {
			t.Fatalf("events = %v", inv.Events)
		}
	})

	t.Run("wait with timeout, consumer, and limit", func(t *testing.T) {
		inv, err := parseInvocation([]string{
			"wait", "--timeout", "5m", "--consumer", "ci", "--limit", "10", url,
		})
		if err != nil {
			t.Fatalf("parseInvocation: %v", err)
		}
		if inv.URL != url || inv.Flags["timeout"] != "5m" || inv.Flags["consumer"] != "ci" || inv.Flags["limit"] != "10" {
			t.Fatalf("inv = %+v", inv)
		}
	})

	t.Run("snapshot flags-first", func(t *testing.T) {
		inv, err := parseInvocation([]string{
			"snapshot", "--snapshot", "s1", "--output", "/tmp/snap.json", url,
		})
		if err != nil {
			t.Fatalf("parseInvocation: %v", err)
		}
		if inv.URL != url || inv.Flags["snapshot"] != "s1" || inv.Flags["output"] != "/tmp/snap.json" {
			t.Fatalf("inv = %+v", inv)
		}
	})
}

func TestDispatchUsageErrorsExit2(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{"missing subcommand", nil},
		{"unknown subcommand", []string{"bogus"}},
		{"reconcile missing url", []string{"reconcile", "--json"}},
		{"two positionals", []string{"reconcile", "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"}},
		{"trailing positional after url-first", []string{"reconcile", "https://github.com/o/r/pull/1", "extra", "--json"}},
		{"unknown flag", []string{"reconcile", "--bogus", "https://github.com/o/r/pull/1"}},
		{"missing flag value url-first", []string{"inbox", "https://github.com/o/r/pull/1", "--consumer"}},
		{"unknown flag url-first", []string{"inbox", "https://github.com/o/r/pull/1", "--bogus"}},
		{"invalid timeout", []string{"wait", "--timeout", "soon", "https://github.com/o/r/pull/1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tc.argv, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("stdout is not one JSON value: %v (%q)", err, stdout.String())
			}
			if envelope["status"] != "error" {
				t.Errorf("status = %v, want error", envelope["status"])
			}
			errObj, ok := envelope["error"].(map[string]any)
			if !ok || errObj["code"] != "usage" {
				t.Errorf("error envelope = %v, want code usage", envelope["error"])
			}
			if envelope["reviewer_completion"] != ReviewerCompletionUnknown {
				t.Errorf("reviewer_completion = %v, want unknown", envelope["reviewer_completion"])
			}
		})
	}
}

func TestEnvelopeFields(t *testing.T) {
	// Every command must emit every contract field, including
	// reviewer_completion "unknown". Implemented commands fail here before
	// any network access: reconcile on an unsupported host, the local
	// commands on a store with no initialized stream, and wait without the
	// required --consumer (a usage error, exit 2).
	base := t.TempDir()
	tests := []struct {
		name string
		argv []string
		code string
		exit int
	}{
		{"reconcile", []string{"reconcile", "--state-dir", base, "https://gitlab.com/o/r/pull/1"}, "unsupported_host", 1},
		{"snapshot", []string{"snapshot", "--snapshot", "s1", "--output", "/tmp/x", "--state-dir", base, "https://github.com/o/r/pull/1"}, "unknown_stream", 1},
		{"inbox", []string{"inbox", "--consumer", "ci", "--state-dir", base, "https://github.com/o/r/pull/1"}, "unknown_stream", 1},
		{"ack", []string{"ack", "--consumer", "ci", "--event", "e1", "--state-dir", base, "https://github.com/o/r/pull/1"}, "unknown_stream", 1},
		{"wait", []string{"wait", "--state-dir", base, "https://github.com/o/r/pull/1"}, "usage", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			code := Run(tc.argv, &stdout, &bytes.Buffer{})
			if code != tc.exit {
				t.Fatalf("%s: exit code = %d, want %d", tc.name, code, tc.exit)
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for _, key := range []string{
				"schema", "command", "status", "target", "account", "observed_head",
				"expected_head", "snapshot", "attempt", "freshness", "reviewer_completion",
				"events", "has_more", "next_cursor", "export", "error",
			} {
				if _, ok := envelope[key]; !ok {
					t.Errorf("%s: missing contract field %q", tc.name, key)
				}
			}
			if string(envelope["schema"]) != `"git-feedback/v1"` {
				t.Errorf("%s: schema = %s", tc.name, envelope["schema"])
			}
			if string(envelope["reviewer_completion"]) != `"unknown"` {
				t.Errorf("%s: reviewer_completion = %s, want unknown", tc.name, envelope["reviewer_completion"])
			}
			if string(envelope["events"]) != "[]" {
				t.Errorf("%s: events = %s, want []", tc.name, envelope["events"])
			}
			if string(envelope["command"]) != fmt.Sprintf("%q", tc.name) {
				t.Errorf("%s: command = %s, want %q", tc.name, envelope["command"], tc.name)
			}
			errObj := map[string]any{}
			if err := json.Unmarshal(envelope["error"], &errObj); err != nil {
				t.Fatalf("%s: error field: %v", tc.name, err)
			}
			if errObj["code"] != tc.code {
				t.Errorf("%s: error.code = %v, want %s", tc.name, errObj["code"], tc.code)
			}
			for _, key := range []string{"code", "message", "retryable"} {
				if _, ok := errObj[key]; !ok {
					t.Errorf("%s: error missing field %q", tc.name, key)
				}
			}
		})
	}

	// A fully populated result exposes every nested contract field.
	full := Result{
		Command: "reconcile",
		Status:  StatusUpdated,
		Target:  &Target{ID: "github:github.com:o/r:1", URL: "u", Forge: "github", Host: "github.com", Repo: "o/r", Number: 1},
		Snapshot: &Snapshot{
			ID: "s1", CollectedStart: "cs", CollectedEnd: "ce", Complete: true,
			ObjectCounts: map[string]int{"thread": 1},
		},
		Attempt: &Attempt{At: "at", OK: true, Complete: true, NextDue: "nd"},
		Export:  &Export{Path: "p", Digest: "d", Counts: map[string]int{}},
		Error:   &Error{Code: "c", Message: "m", Retryable: true},
	}
	var buf bytes.Buffer
	if err := Write(&buf, full); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &rawTop); err != nil {
		t.Fatal(err)
	}
	nested := map[string][]string{
		"target":    {"id", "url", "forge", "host", "repo", "number"},
		"snapshot":  {"id", "collected_start", "collected_end", "complete", "object_counts"},
		"attempt":   {"at", "ok", "complete", "error_code", "next_due"},
		"export":    {"path", "digest", "counts"},
		"error":     {"code", "message", "retryable"},
		"freshness": {"snapshot_observed_at", "stale"},
	}
	for obj, keys := range nested {
		var got map[string]any
		if err := json.Unmarshal(rawTop[obj], &got); err != nil {
			t.Fatalf("%s: %v", obj, err)
		}
		for _, key := range keys {
			if _, ok := got[key]; !ok {
				t.Errorf("%s: missing field %q", obj, key)
			}
		}
	}
}
