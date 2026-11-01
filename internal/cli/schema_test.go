package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// parseSchema runs the schema command and decodes its one JSON document.
func parseSchema(t *testing.T) *SchemaDoc {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"schema"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("schema: exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	// No envelope leaks onto stderr and the stderr channel stays empty:
	// diagnostics are reserved for failures.
	if stderr.Len() != 0 {
		t.Errorf("schema: stderr = %q, want empty", stderr.String())
	}
	doc := &SchemaDoc{}
	if err := json.Unmarshal(stdout.Bytes(), doc); err != nil {
		t.Fatalf("schema: stdout is not one JSON value: %v (%q)", err, stdout.String())
	}
	return doc
}

// TestSchemaCommand pins the machine-readable interface contract: it is
// derived from the same specs table the help renders from, so a new command
// or flag appears in both or fails the help-metadata cross-check.
func TestSchemaCommand(t *testing.T) {
	doc := parseSchema(t)

	if doc.Schema != "git-feedback/cli-schema/v1" {
		t.Errorf("schema = %q", doc.Schema)
	}
	if doc.Docs != "docs/contract.md" {
		t.Errorf("docs = %q", doc.Docs)
	}
	if len(doc.Flow) != 3 || doc.Flow[0] != "reconcile" || doc.Flow[2] != "ack" {
		t.Errorf("typical_flow = %v", doc.Flow)
	}

	// Every spec command is present, with its short and flag kinds.
	for name, spec := range specs {
		cmd, ok := doc.Commands[name]
		if !ok {
			t.Fatalf("schema is missing command %q", name)
		}
		if cmd.Short != commandShort[name] {
			t.Errorf("command %q: short = %q", name, cmd.Short)
		}
		if cmd.URL != (name != "store") {
			t.Errorf("command %q: url_positional = %v", name, cmd.URL)
		}
		if name == "store" {
			continue // flags are checked via the nested subcommands below
		}
		for _, f := range spec.flags {
			got, ok := cmd.Flags[f.name]
			if !ok {
				t.Errorf("command %q: schema is missing flag %q", name, f.name)
				continue
			}
			switch f.kind {
			case flagBool:
				if got.Kind != "bool" {
					t.Errorf("command %q flag %q: kind = %q, want bool", name, f.name, got.Kind)
				}
			case flagRepeat:
				if got.Kind != "repeat" {
					t.Errorf("command %q flag %q: kind = %q, want repeat", name, f.name, got.Kind)
				}
			default:
				if got.Kind != "string" {
					t.Errorf("command %q flag %q: kind = %q, want string", name, f.name, got.Kind)
				}
			}
			if got.Usage == "" {
				t.Errorf("command %q flag %q: empty usage", name, f.name)
			}
		}
	}

	// The store subcommands nest, with --target scoped to reset.
	store, ok := doc.Commands["store"]
	if !ok {
		t.Fatal("schema is missing the store command")
	}
	if store.URL {
		t.Error("store: url_positional = true, want false")
	}
	if store.Flags != nil {
		t.Errorf("store: flags = %v, want nil (flags live on subcommands)", store.Flags)
	}
	reset, ok := store.Subcommands["reset"]
	if !ok {
		t.Fatal("schema is missing the store reset subcommand")
	}
	if _, ok := reset.Flags["target"]; !ok {
		t.Error("store reset: schema is missing the target flag")
	}
	path, ok := store.Subcommands["path"]
	if !ok {
		t.Fatal("schema is missing the store path subcommand")
	}
	if _, ok := path.Flags["target"]; ok {
		t.Error("store path: schema exposes the target flag")
	}

	// The exit-code and signal conventions are the documented ones.
	for name, want := range map[string]int{
		"ok": ExitOK, "operational": ExitOperational, "usage": ExitUsage,
		"events": ExitEvents, "head_changed": ExitHeadChanged,
	} {
		if doc.ExitCodes[name] != want {
			t.Errorf("exit_codes[%q] = %d, want %d", name, doc.ExitCodes[name], want)
		}
	}
	if doc.Signals["SIGINT"] != 130 || doc.Signals["SIGTERM"] != 143 {
		t.Errorf("signals = %v", doc.Signals)
	}

	// The envelope contract: field list and per-command statuses.
	wantFields := []string{
		"schema", "command", "status", "target", "account", "observed_head",
		"expected_head", "snapshot", "attempt", "freshness",
		"reviewer_completion", "events", "has_more", "next_cursor", "export",
		"store", "object", "error",
	}
	if len(doc.Envelope.Fields) != len(wantFields) {
		t.Fatalf("envelope fields = %v, want %v", doc.Envelope.Fields, wantFields)
	}
	for i, f := range wantFields {
		if doc.Envelope.Fields[i] != f {
			t.Errorf("envelope field %d = %q, want %q", i, doc.Envelope.Fields[i], f)
		}
	}
	if got := doc.Envelope.Statuses["reconcile"]; len(got) != 5 {
		t.Errorf("reconcile statuses = %v, want the five documented statuses", got)
	}
	if got := doc.Envelope.Statuses["wait"]; len(got) != 3 {
		t.Errorf("wait statuses = %v, want events/timeout/head_changed", got)
	}
	if got := doc.Envelope.Statuses["store reset"]; len(got) != 2 {
		t.Errorf("store reset statuses = %v, want updated/unchanged", got)
	}

	// The document is deterministic: two runs render identically.
	again := parseSchema(t)
	first, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("schema output is not deterministic across runs")
	}

	// Stray positionals are caller errors, like every other command.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"schema", "extra"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("schema extra: exit code = %d, want 2", code)
	}
}

// TestSchemaInHelp pins that the schema command is discoverable from the
// bare help listing.
func TestSchemaInHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--help"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("--help: exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "print the machine-readable interface contract") {
		t.Error("bare help does not list the schema command")
	}
}
