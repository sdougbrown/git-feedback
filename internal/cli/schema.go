package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// SchemaDoc is the machine-readable interface contract printed by the
// schema command. Every agent- or harness-facing fact about the CLI lives
// here as data: the command and flag surface (derived from the same specs
// table the help renders from), the envelope shape, the statuses each
// command can report, and the exit-code convention. docs/contract.md
// remains the prose reference; this document is the parseable one.
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
// field list every command emits, the statuses each command can report
// (plus the universal error status), the error codes, and where the prose
// contract lives.
type EnvelopeSchema struct {
	Schema        string              `json:"schema"`
	Fields        []string            `json:"fields"`
	Statuses      map[string][]string `json:"statuses"`
	ErrorCodes    []string            `json:"error_codes"`
	Documentation string              `json:"documentation"`
}

// flagSchemaFor builds the FlagSchema for one flagSpec, with the
// command-scoped usage lookup shared with help rendering.
func flagSchemaFor(name string, f flagSpec) FlagSchema {
	usage, ok := flagUsage[name+"."+f.name]
	if !ok {
		usage = flagUsage[f.name]
	}
	switch f.kind {
	case flagBool:
		return FlagSchema{Kind: "bool", Usage: usage, Default: false}
	case flagRepeat:
		return FlagSchema{Kind: "repeat", Usage: usage, Default: []string{}}
	default:
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
// from the specs table and the help metadata; the envelope shape, statuses,
// and exit codes are declared here because they are contract facts that
// tests pin independently.
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
			"SIGINT":  130,
			"SIGTERM": 143,
		},
		Envelope: EnvelopeSchema{
			Schema: Schema,
			Fields: []string{
				"schema", "command", "status", "target", "account",
				"observed_head", "expected_head", "snapshot", "attempt",
				"freshness", "reviewer_completion", "events", "has_more",
				"next_cursor", "export", "store", "object", "error",
			},
			Statuses: map[string][]string{
				"reconcile":   {"updated", "unchanged", "deferred", "busy", "head_changed"},
				"snapshot":    {"ok"},
				"inbox":       {"ok"},
				"show":        {"ok"},
				"ack":         {"ok"},
				"wait":        {"events", "timeout", "head_changed"},
				"store path":  {"ok"},
				"store reset": {"updated", "unchanged"},
			},
			ErrorCodes:    []string{"usage", "unsupported_host", "unknown_stream", "internal"},
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
					Use:   "git-feedback store path [flags]",
				},
				"reset": {
					Short: storeSubShort["reset"],
					Use:   "git-feedback store reset [flags]",
					Flags: flagsSchema("store", spec.flags),
				},
			}
			cmd.Flags = nil
		}
		doc.Commands[name] = cmd
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
// without writing an envelope.
func newSchemaCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "print the machine-readable interface contract as JSON",
		Args:  noArgs("schema"),
		RunE: func(cmd *cobra.Command, args []string) error {
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
