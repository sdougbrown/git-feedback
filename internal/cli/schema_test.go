package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sdougbrown/cli-contract"

	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// parseSchema runs the schema command and decodes its one JSON document.
func parseSchema(t *testing.T) *SchemaDoc {
	doc, _ := parseSchemaRaw(t)
	return doc
}

// parseSchemaRaw also returns the raw rendered stdout bytes.
func parseSchemaRaw(t *testing.T) (*SchemaDoc, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"schema"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("schema: exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	// The stderr channel stays empty: diagnostics are reserved for failures.
	if stderr.Len() != 0 {
		t.Errorf("schema: stderr = %q, want empty", stderr.String())
	}
	doc := &SchemaDoc{}
	if err := json.Unmarshal(stdout.Bytes(), doc); err != nil {
		t.Fatalf("schema: stdout is not one JSON value: %v (%q)", err, stdout.String())
	}
	return doc, stdout.String()
}

// TestSchemaCommand pins the machine-readable interface contract: it is
// derived from the same declarations the help renders from, so a new
// command or flag appears in both or fails startup validation.
func TestSchemaCommand(t *testing.T) {
	doc, raw := parseSchemaRaw(t)

	if doc.Schema != "git-feedback/cli-schema/v1" {
		t.Errorf("schema = %q", doc.Schema)
	}
	if doc.Docs != "docs/contract.md" {
		t.Errorf("docs = %q", doc.Docs)
	}
	wantFlow := []string{"reconcile", "wait", "ack"}
	if len(doc.Flow) != len(wantFlow) {
		t.Fatalf("typical_flow = %v, want %v", doc.Flow, wantFlow)
	}
	for i, step := range wantFlow {
		if doc.Flow[i] != step {
			t.Errorf("typical_flow[%d] = %q, want %q", i, doc.Flow[i], step)
		}
	}

	// Every declared command is present, with its short and flag kinds.
	for _, decl := range commands {
		name := decl.Name
		cmd, ok := doc.Commands[name]
		if !ok {
			t.Fatalf("schema is missing command %q", name)
		}
		if cmd.Short != decl.Short {
			t.Errorf("command %q: short = %q", name, cmd.Short)
		}
		// Independent expectation: the six URL commands take exactly one
		// required positional; nothing else does.
		wantURL := map[string]bool{
			"reconcile": true, "snapshot": true, "inbox": true,
			"ack": true, "show": true, "wait": true,
		}
		if cmd.URL != wantURL[name] {
			t.Errorf("command %q: url_positional = %v, want %v", name, cmd.URL, wantURL[name])
		}
		if len(decl.Subcommands) > 0 {
			// Flags and subcommands are checked via the nested entries
			// below.
			continue
		}
		for _, f := range decl.Flags {
			got, ok := cmd.Flags[f.Name]
			if !ok {
				t.Errorf("command %q: schema is missing flag %q", name, f.Name)
				continue
			}
			if got.Usage != f.Usage {
				t.Errorf("command %q flag %q: usage = %q, want the declared usage", name, f.Name, got.Usage)
			}
			if got.Required != f.Required {
				t.Errorf("command %q flag %q: required = %v, want %v", name, f.Name, got.Required, f.Required)
			}
			if got.Kind != f.Kind.KindSchema() {
				t.Errorf("command %q flag %q: kind = %q, want %q", name, f.Name, got.Kind, f.Kind.KindSchema())
			}
			// Defaults: the declared effective default when set, the flag's
			// zero value otherwise.
			switch {
			case f.Default != nil:
				if got.Default != f.Default {
					t.Errorf("command %q flag %q: default = %v, want %v", name, f.Name, got.Default, f.Default)
				}
			case f.Kind == clicontract.Bool:
				if got.Default != false {
					t.Errorf("command %q flag %q: default = %v, want false", name, f.Name, got.Default)
				}
			case f.Kind == clicontract.Repeat:
				arr, ok := got.Default.([]any)
				if !ok || len(arr) != 0 {
					t.Errorf("command %q flag %q: default = %v, want an empty array", name, f.Name, got.Default)
				}
			default:
				if got.Default != "" {
					t.Errorf("command %q flag %q: default = %v, want an empty string", name, f.Name, got.Default)
				}
			}
		}
	}

	// The store subcommands nest, with --target scoped to reset.
	storeEntry, ok := doc.Commands["store"]
	if !ok {
		t.Fatal("schema is missing the store command")
	}
	if storeEntry.URL {
		t.Error("store: url_positional = true, want false")
	}
	if storeEntry.Flags != nil {
		t.Errorf("store: flags = %v, want nil (flags live on subcommands)", storeEntry.Flags)
	}
	reset, ok := storeEntry.Subcommands["reset"]
	if !ok {
		t.Fatal("schema is missing the store reset subcommand")
	}
	if _, ok := reset.Flags["target"]; !ok {
		t.Error("store reset: schema is missing the target flag")
	}
	path, ok := storeEntry.Subcommands["path"]
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
	// The exact statuses are the contract; pins use the Status constants.
	wantStatuses := map[string][]string{
		"reconcile":   {StatusUpdated, StatusUnchanged, StatusDeferred, StatusBusy, StatusHeadChange},
		"snapshot":    {StatusOK},
		"inbox":       {StatusOK},
		"show":        {StatusOK},
		"ack":         {StatusOK},
		"wait":        {StatusEvents, StatusTimeout, StatusHeadChange},
		"store path":  {StatusOK},
		"store reset": {StatusUpdated, StatusUnchanged},
	}
	for name, want := range wantStatuses {
		got, ok := doc.Envelope.Statuses[name]
		if !ok {
			t.Errorf("envelope statuses missing %q", name)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("statuses[%q] = %v, want %v", name, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statuses[%q][%d] = %q, want %q", name, i, got[i], want[i])
			}
		}
	}
	if len(doc.Envelope.UniversalStatuses) != 1 || doc.Envelope.UniversalStatuses[0] != StatusError {
		t.Errorf("universal_statuses = %v, want [error]", doc.Envelope.UniversalStatuses)
	}
	if doc.Envelope.StatusKeyedBy != "full command path" {
		t.Errorf("status_keyed_by = %q, want \"full command path\"", doc.Envelope.StatusKeyedBy)
	}
	// The error codes are the full stable set: CLI-declared codes plus every
	// store constant that mapStoreError passes through.
	wantCodes := map[string]bool{
		"usage": true, "unsupported_host": true, "ambiguous_account": true,
		"auth": true, "not_found": true, "rate_limited": true,
		"collection_error": true, "state_dir": true, "internal": true,
		store.CodeStore: true, store.CodeStoreCorrupt: true,
		store.CodeStoreNewer: true, store.CodeStoreDataOlder: true,
		store.CodeUnknownStream: true, store.CodeUnknownEvent: true,
		store.CodeUnknownSnapshot: true, store.CodeInvalidLimit: true,
		store.CodeInvalidInput: true, store.CodeFenceLost: true,
		store.CodeLeaseBusy: true, store.CodeDestinationExists: true,
		store.CodeExportStateDir: true, store.CodeExportUnsupported: true,
		store.CodeInvalidDestination: true,
	}
	// The three caller-mistake codes are remapped to "usage" by
	// mapStoreError and so never appear under their original code.
	for _, remapped := range []string{
		store.CodeInvalidCursor, store.CodeInvalidConsumer, store.CodeTargetKindMismatch,
	} {
		if wantCodes[remapped] {
			t.Errorf("error_codes unexpectedly includes remapped code %q", remapped)
		}
	}
	if len(doc.Envelope.ErrorCodes) != len(wantCodes) {
		t.Errorf("error_codes has %d entries, want %d: %v", len(doc.Envelope.ErrorCodes), len(wantCodes), doc.Envelope.ErrorCodes)
	}
	for _, c := range doc.Envelope.ErrorCodes {
		if !wantCodes[c] {
			t.Errorf("error_codes has unexpected entry %q", c)
		}
	}

	// The document is deterministic: the raw rendered bytes are identical
	// across runs, keep synopsis markers literal, and are not HTML-escaped.
	var stdout2, stderr2 bytes.Buffer
	if code := Run([]string{"schema"}, &stdout2, &stderr2); code != ExitOK {
		t.Fatalf("second schema run: exit code = %d", code)
	}
	if raw != stdout2.String() {
		t.Error("schema output is not byte-identical across runs")
	}
	if !strings.Contains(raw, "reconcile <URL>") {
		t.Error("schema output lost the literal synopsis marker <URL>")
	}
	if strings.Contains(raw, "\\u003c") {
		t.Error("schema output is HTML-escaped")
	}

	// Stray positionals are caller errors; the envelope assertions live in
	// TestDispatchUsageErrorsExit2.
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

// TestSchemaMatchesCommandTree cross-checks the schema document against the
// real cobra tree the CLI registers: same commands, same Use strings and
// shorts, same flag sets. Derivation drift between schema and help fails
// here instead of shipping two diverging surfaces.
func TestSchemaMatchesCommandTree(t *testing.T) {
	doc := parseSchema(t)
	root := app().Tree()

	// Walk the registered tree and require a matching schema entry for
	// every command.
	var walk func(cmd *cobra.Command, path string)
	walk = func(cmd *cobra.Command, path string) {
		for _, sub := range cmd.Commands() {
			if sub.Name() == "help" {
				continue // cobra's help command is not part of the contract
			}
			full := sub.Name()
			parentPath := ""
			if path != "" {
				full = path + " " + sub.Name()
				parentPath = path
			}
			var entry CommandSchema
			var ok bool
			if parentPath == "" {
				entry, ok = doc.Commands[sub.Name()]
			} else {
				parent := doc.Commands[parentPath]
				entry, ok = parent.Subcommands[sub.Name()]
			}
			if !ok {
				t.Errorf("schema is missing command %q", full)
				continue
			}
			if entry.Use != sub.Use {
				t.Errorf("command %q: schema use = %q, cobra Use = %q", full, entry.Use, sub.Use)
			}
			if entry.Short != sub.Short {
				t.Errorf("command %q: schema short = %q, cobra short = %q", full, entry.Short, sub.Short)
			}
			// Every non-persistent flag registered on the command appears
			// in the schema entry.
			sub.Flags().VisitAll(func(f *pflag.Flag) {
				if f.Name == "help" {
					return // cobra's implicit help flag
				}
				if _, ok := entry.Flags[f.Name]; !ok {
					t.Errorf("command %q: schema is missing flag %q", full, f.Name)
				}
			})
			walk(sub, full)
		}
	}
	walk(root, "")

	// Conversely, no schema entry exists for a command the tree lacks.
	for name := range doc.Commands {
		found, _, err := root.Find([]string{name})
		if err != nil || found.Name() != name {
			t.Errorf("schema documents command %q that the tree does not register", name)
		}
	}
}

// TestSchemaParseOnlyExecution pins the parse-only behavior of the schema
// command: Parse returns the raw command's Invocation without error, and
// Run exits 0 after the raw handler writes its own output — no result
// envelope is emitted on top.
func TestSchemaParseOnlyExecution(t *testing.T) {
	inv, err := parseInvocation([]string{"schema"})
	if err != nil {
		t.Fatalf("parseInvocation(schema): err = %v, want nil", err)
	}
	if inv.Command != "schema" {
		t.Fatalf("parseInvocation(schema): command = %q, want \"schema\"", inv.Command)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"schema"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run(schema): exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	// The raw handler writes the schema document itself; Run adds no
	// result envelope on top, so stdout is exactly one JSON document with
	// the schema document's own schema id.
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("Run(schema): stdout is not one JSON document: %v\n%s", err, stdout.String())
	}
	if doc["schema"] != "git-feedback/cli-schema/v1" {
		t.Errorf("Run(schema): stdout document schema = %v, want git-feedback/cli-schema/v1", doc["schema"])
	}
	if dec.More() {
		t.Errorf("Run(schema): stdout carries output after the schema document:\n%s", stdout.String())
	}
}
