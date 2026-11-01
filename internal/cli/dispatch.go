// Package cli implements the git-feedback command boundary: the declared
// command surface, the result envelope, and exit-code/signal plumbing. The
// cobra tree, argv parsing, and dispatch derive from the declarations
// through the clicontract library; this file carries the git-feedback
// vocabulary the library deliberately does not know about: the envelope
// types, the exit-code conventions, and the wait deadline.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sdougbrown/cli-contract"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// Invocation carries one parsed command invocation.
type Invocation = clicontract.Invocation

// envelope adapts a handler returning the envelope Result to the library's
// any-returning HandleFunc.
func envelope(fn func(ctx context.Context, inv Invocation) (Result, error)) clicontract.HandleFunc {
	return func(ctx context.Context, inv Invocation) (any, error) { return fn(ctx, inv) }
}

// usageError marks a caller error: exit 2 with the error envelope. The
// library type is aliased so handlers and ExitCode share one vocabulary.
type usageError = clicontract.UsageError

// statusError is a handler-reported operational failure.
type statusError struct {
	code      string
	message   string
	retryable bool
	err       error
}

func (e *statusError) Error() string { return e.message }
func (e *statusError) Unwrap() error { return e.err }

// version is the build version reported by --version; main sets it from the
// build-time ldflags value.
var version = "dev"

// SetVersion records the build version reported by --version.
func SetVersion(v string) { version = v }

// commands declares the command surface. Handle funcs are resolved at
// dispatch time through these declarations; tests stub a handle by swapping
// the field on the declared command.
var commands []*clicontract.Command

func init() {
	commands = buildCommands()
}

// commonFlags are accepted by every subcommand.
var commonFlags = []clicontract.Flag{
	{Name: "json", Kind: clicontract.Bool, Usage: "no-op; output is always a single JSON envelope"},
	{Name: "account", Kind: clicontract.String, Usage: "GitHub account login for the stream; defaults to the gh CLI account"},
	{Name: "state-dir", Kind: clicontract.String, Usage: "override the state directory (defaults to a per-user cache directory)"},
}

// buildCommands declares every command. The declaration is the single
// source of truth for the cobra tree, the help text, and the schema
// document; the validate step in the library fails startup on drift.
func buildCommands() []*clicontract.Command {
	return []*clicontract.Command{
		{
			Name:  "reconcile",
			Short: "collect feedback and establish the baseline",
			URL:   true,
			Flags: []clicontract.Flag{
				{Name: "head", Kind: clicontract.String, Usage: "expected head SHA; report head_changed when the observed head no longer matches"},
			},
			Handle: envelope(handleReconcile),
		},
		{
			Name:  "snapshot",
			Short: "export a stored collection to a file",
			URL:   true,
			Flags: []clicontract.Flag{
				{Name: "snapshot", Kind: clicontract.String, Usage: "ID of the stored snapshot to export"},
				{Name: "output", Kind: clicontract.String, Usage: "file path for the exported snapshot JSON"},
			},
			Handle: envelope(handleSnapshot),
		},
		{
			Name:  "inbox",
			Short: "list pending events for a consumer",
			URL:   true,
			Flags: []clicontract.Flag{
				{Name: "consumer", Kind: clicontract.String, Usage: "name identifying this reader of the stream"},
				{Name: "limit", Kind: clicontract.String, Usage: "maximum events to return (1-200, default 50)", Default: strconv.Itoa(store.DefaultInboxLimit)},
				{Name: "after", Kind: clicontract.String, Usage: "cursor from a previous inbox page; fetch the next page"},
				{Name: "ids-only", Kind: clicontract.Bool, Usage: "return event IDs only, without full event records"},
				{Name: "include-self", Kind: clicontract.Bool, Usage: "include events authored by the stream's own account (excluded by default)"},
				{Name: "exclude-self", Kind: clicontract.Bool, Usage: "deprecated no-op; self-authored events are already excluded"},
				{Name: "skip-empty-reviews", Kind: clicontract.Bool, Usage: "skip review events whose review body and comments are empty"},
			},
			Handle: envelope(handleInbox),
		},
		{
			Name:  "ack",
			Short: "acknowledge delivered event IDs",
			URL:   true,
			Flags: []clicontract.Flag{
				{Name: "consumer", Kind: clicontract.String, Usage: "name identifying this reader of the stream"},
				{Name: "event", Kind: clicontract.Repeat, Usage: "event ID to acknowledge; repeat the flag for multiple IDs"},
				{Name: "events-from", Kind: clicontract.String, Usage: "read event IDs from a file, or - for stdin"},
			},
			Handle: envelope(handleAck),
		},
		{
			Name:  "show",
			Short: "fetch one event's stored object content (body, state, comments)",
			URL:   true,
			Flags: []clicontract.Flag{
				{Name: "event", Kind: clicontract.String, Usage: "event ID to display"},
			},
			Handle: envelope(handleShow),
		},
		{
			Name:  "wait",
			Short: "block until new events arrive or --timeout elapses (bounded polling)",
			URL:   true,
			Flags: []clicontract.Flag{
				{Name: "consumer", Kind: clicontract.String, Usage: "name identifying this reader of the stream"},
				{Name: "timeout", Kind: clicontract.String, Usage: "maximum wait duration (default 30m)", Default: waitDefaultTimeout.String()},
				{Name: "head", Kind: clicontract.String, Usage: "expected head SHA; report head_changed when the observed head no longer matches"},
				{Name: "limit", Kind: clicontract.String, Usage: "maximum events to return (1-200, default 50)", Default: strconv.Itoa(store.DefaultInboxLimit)},
				{Name: "include-self", Kind: clicontract.Bool, Usage: "include events authored by the stream's own account (excluded by default)"},
				{Name: "exclude-self", Kind: clicontract.Bool, Usage: "deprecated no-op; self-authored events are already excluded"},
				{Name: "skip-empty-reviews", Kind: clicontract.Bool, Usage: "skip review events whose review body and comments are empty"},
				{Name: "skip-head-changed", Kind: clicontract.Bool, Usage: "suppress head_changed events and keep waiting"},
				{Name: "exit-on-events", Kind: clicontract.Bool, Usage: "exit 3 when events are delivered and 4 when a pinned head moved"},
			},
			Handle: envelope(handleWait),
		},
		{
			Name:  "store",
			Short: "disposable store utilities",
			Long:  StoreUsage,
			Subcommands: []*clicontract.Command{
				{
					Name:   "path",
					Short:  "print the resolved store directory; never opens the store",
					Handle: envelope(handleStorePath),
				},
				{
					Name:  "reset",
					Short: "delete the store, wholly or scoped to one --target URL",
					Flags: []clicontract.Flag{
						{Name: "target", Kind: clicontract.String, Usage: "scope the reset to one target URL"},
					},
					Handle: envelope(handleStoreReset),
				},
			},
		},
		{
			Name:  "schema",
			Short: "print the machine-readable interface contract as JSON",
			Raw:   handleSchemaRaw,
		},
	}
}

// app assembles the declared CLI for the library.
func app() *clicontract.App {
	return &clicontract.App{
		Name:        "git-feedback",
		Version:     version,
		RootShort:   "Observe GitHub review feedback and deliver replayable events",
		RootLong:    Usage,
		Commands:    commands,
		CommonFlags: commonFlags,
	}
}

// cliAdapter implements the result vocabulary: the envelope types, the exit
// codes, and the wait deadline are git-feedback conventions the library
// deliberately does not know about.
type cliAdapter struct{}

func (cliAdapter) ErrorResult(command string, err error) any {
	return errorResult(command, err)
}

func (cliAdapter) Write(w io.Writer, result any) error {
	return writeResult(w, result.(Result))
}

func (cliAdapter) ExitCode(inv Invocation, result any, handlerErr error) int {
	if result == nil {
		// Dispatch-level failure: no result was written beyond the error
		// envelope, and dispatch-level failures are caller errors.
		return ExitUsage
	}
	r := result.(Result)
	if r.Error != nil {
		var ue *usageError
		if handlerErr != nil && errors.As(handlerErr, &ue) {
			return ExitUsage
		}
		return ExitOperational
	}
	// Opt-in status-driven codes: a monitor that signals on process exit
	// separates delivered events from the deadline expiry without parsing
	// stderr or the envelope. Errors and signals elsewhere keep their own
	// codes.
	if inv.Command == "wait" && inv.Bools["exit-on-events"] {
		switch r.Status {
		case StatusEvents:
			return ExitEvents
		case StatusHeadChange:
			return ExitHeadChanged
		}
	}
	return ExitOK
}

func (cliAdapter) Validate(inv Invocation) error {
	// The --timeout duration contract: a present --timeout must parse to a
	// positive duration.
	if raw := inv.Flags["timeout"]; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return &usageError{Message: fmt.Sprintf("invalid --timeout %q", raw)}
		}
	}
	return nil
}

func (cliAdapter) Deadline(ctx context.Context, inv Invocation) (context.Context, context.CancelFunc) {
	if raw := inv.Flags["timeout"]; raw != "" {
		// Validation ran in Validate; parse here for the deadline.
		d, _ := time.ParseDuration(raw)
		return context.WithTimeout(ctx, d)
	}
	if inv.Command == "wait" {
		// Apply the pinned default deadline.
		return context.WithTimeout(ctx, waitDefaultTimeout)
	}
	return ctx, func() {}
}

func (cliAdapter) BeforeHandler(inv Invocation, stderr io.Writer) {
	if inv.Bools["exclude-self"] {
		fmt.Fprintln(stderr, "--exclude-self is deprecated and is a no-op; use --include-self to deliver your own events")
	}
}

func (cliAdapter) AfterWrite(inv Invocation, result any, stderr io.Writer) {
	r := result.(Result)
	// One-line status summary on the diagnostics channel: monitor commands
	// point at `wait` and read the outcome from the process output without
	// parsing the envelope.
	if r.Command == "wait" &&
		(r.Status == StatusEvents || r.Status == StatusTimeout) {
		fmt.Fprintf(stderr, "git-feedback wait: status=%s events=%d has_more=%t\n",
			r.Status, len(r.Events), r.HasMore)
	}
}

// Run executes one CLI invocation: it parses argv, runs the handler, writes
// exactly one JSON envelope to stdout, and returns the process exit code.
// Diagnostics go to stderr.
func Run(argv []string, stdout, stderr io.Writer) int {
	return clicontract.Run(app(), cliAdapter{}, argv, stdout, stderr)
}

// parseInvocation parses argv (without the program name) without running
// anything: the library validates against the same tree dispatch uses.
func parseInvocation(argv []string) (Invocation, error) {
	return clicontract.Parse(app(), argv)
}

// errorResult builds the error envelope for a failure.
func errorResult(command string, err error) Result {
	r := Result{Command: command, Status: StatusError}
	var se *statusError
	var ue *usageError
	var ce *clicontract.UsageError
	switch {
	case errors.As(err, &se):
		r.Error = &Error{Code: se.code, Message: se.message, Retryable: se.retryable}
	case errors.As(err, &ue):
		r.Error = &Error{Code: "usage", Message: ue.Message}
	case errors.As(err, &ce):
		r.Error = &Error{Code: "usage", Message: ce.Message}
	default:
		r.Error = &Error{Code: "internal", Message: err.Error()}
	}
	return r
}

// writeResult emits exactly one JSON envelope to w. The fallback is written
// only when the primary write failed before any bytes reached w; a partial write is reported via stderr and the error return instead.
func writeResult(w io.Writer, r Result) error {
	var emitted int64
	err := Write(&countingWriter{w: w, n: &emitted}, r)
	if err == nil {
		return nil
	}
	if emitted > 0 {
		return err
	}
	fmt.Fprintln(w, `{"schema":"git-feedback/v1","status":"error","error":{"code":"internal","message":"envelope write failed"}}`)
	return err
}

// countingWriter counts bytes delivered to the underlying writer so a
// failed write can be told apart from a partial one.
type countingWriter struct {
	w io.Writer
	n *int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
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

// stubHandle swaps one declared command's handler for a test stub, restored
// at cleanup. Stubbing the shared declarations is process-global: safe only
// because no test in this package uses t.Parallel().
func stubHandle(t interface{ Cleanup(func()) }, name string, fn func(ctx context.Context, inv Invocation) (Result, error)) {
	for _, c := range commands {
		if c.Name == name {
			old := c.Handle
			c.Handle = func(ctx context.Context, inv Invocation) (any, error) {
				return fn(ctx, inv)
			}
			t.Cleanup(func() { c.Handle = old })
			return
		}
	}
	panic("no declared command named " + name)
}
