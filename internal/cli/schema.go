package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"

	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/spf13/cobra"
)

// SchemaDoc is the machine-readable interface contract printed by the
// schema command. Every agent- or harness-facing fact about the CLI lives
// here as data. The command and flag surface derives from the same specs
// table the help renders from; the envelope shape, statuses, and exit-code
// convention are declared facts. docs/contract.md remains the prose
// reference; this document is the parseable one.
type SchemaDoc struct {
	Schema      string                   `json:"schema"`
	CliVersion  string                   `json:"cli_version"`
	Docs        string                   `json:"docs"`
	Flow        []string                 `json:"typical_flow"`
	CommonFlags map[string]FlagSchema    `json:"common_flags"`
	Commands    map[string]CommandSchema `json:"commands"`
	ExitCodes   map[string]int           `json:"exit_codes"`
	Signals     map[string]int           `json:"signals"`
	Envelope    EnvelopeSchema           `json:"envelope"`
}

// CommandSchema describes one command: its help short, its synopsis form,
// whether it takes a URL positional, its flags, and (for nested commands)
// its subcommands.
type CommandSchema struct {
	Short       string                   `json:"short"`
	Use         string                   `json:"use"`
	URL         bool                     `json:"url_positional"`
	Flags       map[string]FlagSchema    `json:"flags,omitempty"`
	Subcommands map[string]CommandSchema `json:"subcommands,omitempty"`
}

// FlagSchema describes one flag: its value kind, its usage string, and its
// default.
type FlagSchema struct {
	Kind    string `json:"kind"`
	Usage   string `json:"usage"`
	Default any    `json:"default"`
}

// EnvelopeSchema describes the result envelope: its schema id, the ordered
// field list every command emits, and the statuses each command can report.
// UniversalStatuses names the status every command reports on failure;
// ErrorCodes names the stable error codes. Documentation points at the
// prose contract.
type EnvelopeSchema struct {
	Schema            string              `json:"schema"`
	Fields            []string            `json:"fields"`
	Statuses          map[string][]string `json:"statuses"`
	StatusKeyedBy     string              `json:"status_keyed_by"`
	UniversalStatuses []string            `json:"universal_statuses"`
	ErrorCodes        []string            `json:"error_codes"`
	Documentation     string              `json:"documentation"`
}

// envelopeFields derives the ordered field list from Result's json tags:
// the struct is the single source of the envelope shape.
func envelopeFields() []string {
	t := reflect.TypeOf(Result{})
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fields = append(fields, name)
	}
	return fields
}

// flagDefault carries documented effective defaults. The flag value is a
// string and its zero value is empty, but the handler treats the documented
// default as the value when the flag is absent. Built from the constants
// the handlers actually apply.
var flagDefault = map[string]string{
	"inbox.limit":  strconv.Itoa(store.DefaultInboxLimit),
	"wait.limit":   strconv.Itoa(store.DefaultInboxLimit),
	"wait.timeout": waitDefaultTimeout.String(),
}

// flagUsageFor resolves the command-scoped usage string: a
// "<command>.<flag>" entry wins over the plain flag name. The single
// lookup both help rendering and the schema document share.
func flagUsageFor(command, flag string) string {
	if usage, ok := flagUsage[command+"."+flag]; ok {
		return usage
	}
	return flagUsage[flag]
}

// flagSchemaFor builds the FlagSchema for one flagSpec.
func flagSchemaFor(name string, f flagSpec) FlagSchema {
	usage := flagUsageFor(name, f.name)
	def, hasDefault := flagDefault[name+"."+f.name]
	switch f.kind {
	case flagBool:
		return FlagSchema{Kind: "bool", Usage: usage, Default: false}
	case flagRepeat:
		return FlagSchema{Kind: "repeat", Usage: usage, Default: []string{}}
	default:
		if hasDefault {
			return FlagSchema{Kind: "string", Usage: usage, Default: def}
		}
		return FlagSchema{Kind: "string", Usage: usage, Default: ""}
	}
}

// flagsSchema maps one command's flags by name.
func flagsSchema(name string, flags []flagSpec) map[string]FlagSchema {
	out := make(map[string]FlagSchema, len(flags))
	for _, f := range flags {
		out[f.name] = flagSchemaFor(name, f)
	}
	return out
}

// buildSchema derives the interface contract. Command and flag data come
// from the specs table and the help metadata. The envelope shape, statuses,
// and exit codes are declared here. They are contract facts that tests pin
// independently.
func buildSchema() *SchemaDoc {
	doc := &SchemaDoc{
		Schema:      "git-feedback/cli-schema/v1",
		CliVersion:  version,
		Docs:        "docs/contract.md",
		Flow:        []string{"reconcile", "wait", "ack"},
		CommonFlags: flagsSchema("", commonFlags),
		Commands:    map[string]CommandSchema{},
		ExitCodes: map[string]int{
			"ok":           ExitOK,
			"operational":  ExitOperational,
			"usage":        ExitUsage,
			"events":       ExitEvents,
			"head_changed": ExitHeadChanged,
		},
		Signals: map[string]int{
			"SIGINT":  SignalExitCode(os.Interrupt),
			"SIGTERM": SignalExitCode(syscall.SIGTERM),
		},
		Envelope: EnvelopeSchema{
			Schema: Schema,
			Fields: envelopeFields(),
			Statuses: map[string][]string{
				"reconcile":   {StatusUpdated, StatusUnchanged, StatusDeferred, StatusBusy, StatusHeadChange},
				"snapshot":    {StatusOK},
				"inbox":       {StatusOK},
				"show":        {StatusOK},
				"ack":         {StatusOK},
				"wait":        {StatusEvents, StatusTimeout, StatusHeadChange},
				"store path":  {StatusOK},
				"store reset": {StatusUpdated, StatusUnchanged},
			},
			// Statuses are keyed by the full command path: nested
			// subcommands flatten to "store path" / "store reset".
			StatusKeyedBy:     "full command path",
			UniversalStatuses: []string{StatusError},
			ErrorCodes: append([]string{
				// CLI-declared codes.
				"usage", "unsupported_host", "ambiguous_account", "auth",
				"not_found", "rate_limited", "collection_error", "state_dir",
				"internal",
			},
				// Store codes that reach the envelope verbatim: mapStoreError
				// passes operational store errors through under their constant
				// code. Caller-mistake codes (invalid cursor, invalid consumer,
				// target kind mismatch) are remapped to "usage" and so appear
				// there instead.
				store.CodeStore, store.CodeStoreCorrupt, store.CodeStoreNewer,
				store.CodeStoreDataOlder, store.CodeUnknownStream,
				store.CodeUnknownEvent, store.CodeUnknownSnapshot,
				store.CodeInvalidLimit, store.CodeInvalidInput,
				store.CodeFenceLost, store.CodeLeaseBusy,
				store.CodeDestinationExists, store.CodeExportStateDir,
				store.CodeExportUnsupported, store.CodeInvalidDestination),
			Documentation: "docs/contract.md",
		},
	}
	for name, spec := range specs {
		cmd := CommandSchema{
			Short: commandShort[name],
			Use:   commandUse(name),
			URL:   name != "store",
			Flags: flagsSchema(name, spec.flags),
		}
		if name == "store" {
			cmd.Subcommands = map[string]CommandSchema{
				"path": {
					Short: storeSubShort["path"],
					Use:   "path",
				},
				"reset": {
					Short: storeSubShort["reset"],
					Use:   "reset",
					Flags: flagsSchema("store", spec.flags),
				},
			}
			cmd.Flags = nil
		}
		doc.Commands[name] = cmd
	}
	// The document describes itself: schema is a real command a harness can
	// invoke, so a consumer enumerating doc.Commands discovers it.
	doc.Commands["schema"] = CommandSchema{
		Short: "print the machine-readable interface contract as JSON",
		Use:   "schema",
	}
	return doc
}

// commandUse mirrors the cobra Use string for one command.
func commandUse(name string) string {
	if name == "store" {
		return name
	}
	return name + " <URL>"
}

// newSchemaCommand builds the schema command. Its output is the one JSON
// schema document, not an envelope; exec.ran stays false so Run exits 0
// without writing an envelope. Parse-only executions (parseInvocation) skip
// the body entirely.
func newSchemaCommand(exec *execution, runHandlers bool) *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "print the machine-readable interface contract as JSON",
		Args:  noArgs("schema"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !runHandlers {
				return nil
			}
			// One JSON document on stdout; HTML escaping stays off so
			// synopsis markers like <URL> render literally.
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(buildSchema()); err != nil {
				return &usageError{fmt.Sprintf("schema: %v", err)}
			}
			return nil
		},
	}
}
