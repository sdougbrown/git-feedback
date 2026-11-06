package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/sdougbrown/cli-contract"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// SchemaDoc is the machine-readable interface contract printed by the
// schema command. The command and flag surface derives from the declared
// commands through the clicontract library; the envelope shape, statuses,
// and exit-code conventions are declared facts that tests pin
// independently. docs/contract.md remains the prose reference; this
// document is the parseable one.
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

// CommandSchema describes one command as data: its help short, its Use
// string, whether it takes a URL positional, its flags, and (for nested
// commands) its subcommands.
type CommandSchema = clicontract.CommandSchema

// FlagSchema describes one flag as data: the value kind, the usage string,
// and the default.
type FlagSchema = clicontract.FlagSchema

// EnvelopeSchema describes the result envelope: its schema id, the ordered
// field list every command emits, and the statuses each command can report.
// StatusKeyedBy names the keying rule for the statuses; UniversalStatuses
// names the status every command reports on failure; ErrorCodes names the
// stable error codes. Documentation points at the prose contract.
type EnvelopeSchema struct {
	Schema            string              `json:"schema"`
	Fields            []string            `json:"fields"`
	Statuses          map[string][]string `json:"statuses"`
	StatusKeyedBy     string              `json:"status_keyed_by"`
	UniversalStatuses []string            `json:"universal_statuses"`
	ErrorCodes        []string            `json:"error_codes"`
	Documentation     string              `json:"documentation"`
}

// buildSchema derives the interface contract.
func buildSchema() *SchemaDoc {
	return &SchemaDoc{
		Schema:      "git-feedback/cli-schema/v1",
		CliVersion:  version,
		Docs:        "docs/contract.md",
		Flow:        []string{"reconcile", "wait", "ack"},
		CommonFlags: app().CommonFlagSchemas(),
		Commands:    app().Schemas(),
		ExitCodes: map[string]int{
			"ok":           ExitOK,
			"operational":  ExitOperational,
			"usage":        ExitUsage,
			"events":       ExitEvents,
			"head_changed": ExitHeadChanged,
		},
		Signals: map[string]int{
			"SIGINT":  clicontract.SignalExitCode(os.Interrupt),
			"SIGTERM": clicontract.SignalExitCode(syscall.SIGTERM),
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
				"baseline":    {StatusOK},
				"watch":       {StatusEvents, StatusTimeout},
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
				"internal", "busy", "output_error",
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
}

// handleSchemaRaw writes the interface contract as one JSON document. The
// schema command's output is not an envelope; HTML escaping stays off so
// synopsis markers like <URL> render literally.
func handleSchemaRaw(ctx context.Context, inv Invocation, stdout io.Writer) error {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(buildSchema()); err != nil {
		return &usageError{Message: fmt.Sprintf("schema: %v", err)}
	}
	return nil
}
