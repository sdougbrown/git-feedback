// Package cli implements the git-feedback command boundary: argv parsing for
// the subcommands, the result envelope, and exit-code/signal plumbing.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// handlerFunc runs one parsed subcommand invocation.
type handlerFunc func(ctx context.Context, inv Invocation) (Result, error)

// Invocation carries one parsed subcommand invocation.
type Invocation struct {
	Command string
	// Sub is the nested subcommand for commands that have one (store path,
	// store reset); empty otherwise.
	Sub string
	URL string
	// Flags holds single-valued flag values by name; absent flags are "".
	Flags map[string]string
	// Bools holds boolean flags by name; absent flags are false.
	Bools  map[string]bool
	Events []string // repeated --event values

	handler handlerFunc
}

// usageError marks a caller error: exit 2 with the error envelope.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// statusError is a handler-reported operational failure.
type statusError struct {
	code      string
	message   string
	retryable bool
	err       error
}

func (e *statusError) Error() string { return e.message }
func (e *statusError) Unwrap() error { return e.err }

type flagKind int

const (
	flagString flagKind = iota
	flagBool
	flagRepeat
)

type flagSpec struct {
	name string
	kind flagKind
}

type commandSpec struct {
	flags  []flagSpec
	handle handlerFunc
}

// commonFlags are accepted by every subcommand.
var commonFlags = []flagSpec{
	{"json", flagBool}, // accepted everywhere; output is always JSON
	{"account", flagString},
	{"state-dir", flagString},
}

var specs = map[string]commandSpec{
	"reconcile": {
		flags:  []flagSpec{{"head", flagString}},
		handle: handleReconcile,
	},
	"snapshot": {
		flags:  []flagSpec{{"snapshot", flagString}, {"output", flagString}},
		handle: handleSnapshot,
	},
	"inbox": {
		flags:  []flagSpec{{"consumer", flagString}, {"limit", flagString}, {"after", flagString}, {"ids-only", flagBool}, {"exclude-self", flagBool}, {"skip-empty-reviews", flagBool}},
		handle: handleInbox,
	},
	"ack": {
		flags:  []flagSpec{{"consumer", flagString}, {"event", flagRepeat}, {"events-from", flagString}},
		handle: handleAck,
	},
	"show": {
		flags:  []flagSpec{{"event", flagString}},
		handle: handleShow,
	},
	"wait": {
		flags:  []flagSpec{{"consumer", flagString}, {"timeout", flagString}, {"head", flagString}, {"limit", flagString}, {"exclude-self", flagBool}, {"skip-empty-reviews", flagBool}, {"skip-head-changed", flagBool}, {"exit-on-events", flagBool}},
		handle: handleWait,
	},
	"store": {
		flags:  []flagSpec{{"target", flagString}},
		handle: handleStore,
	},
}

// parseInvocation parses argv (without the program name) for one subcommand.
// Both `git-feedback <sub> <URL> [flags]` and `git-feedback <sub> [flags]
// <URL>` are accepted: for URL-first input the leading positional is removed
// before flag parsing; otherwise flags are parsed first and exactly one
// remaining positional URL is required. Other interleavings and extra
// positionals are rejected as usage errors.
func parseInvocation(argv []string) (Invocation, error) {
	if len(argv) == 0 {
		return Invocation{}, &usageError{"missing subcommand"}
	}
	cmd, rest := argv[0], argv[1:]
	spec, ok := specs[cmd]
	if !ok {
		return Invocation{}, &usageError{fmt.Sprintf("unknown subcommand %q", cmd)}
	}

	// The store command takes a nested subcommand instead of a URL:
	// `store path` or `store reset [--target <URL>]`.
	sub := ""
	if cmd == "store" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return Invocation{}, &usageError{`store requires a subcommand: "path" or "reset"`}
		}
		sub = rest[0]
		rest = rest[1:]
		if sub != "path" && sub != "reset" {
			return Invocation{}, &usageError{fmt.Sprintf("unknown store subcommand %q (want %q or %q)", sub, "path", "reset")}
		}
	}

	inv := Invocation{Command: cmd, Sub: sub, Flags: map[string]string{}, Bools: map[string]bool{}, handler: spec.handle}

	// The store command takes no URL positional; its arguments are flags only.
	urlFirst := cmd != "store" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-")
	if urlFirst {
		inv.URL = rest[0]
		rest = rest[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stringVals := map[string]*string{}
	boolVals := map[string]*bool{}
	var events *EventFlags
	all := append(append([]flagSpec{}, commonFlags...), spec.flags...)
	for _, f := range all {
		switch f.kind {
		case flagBool:
			boolVals[f.name] = fs.Bool(f.name, false, "")
		case flagRepeat:
			ev := &EventFlags{}
			fs.Var(ev, f.name, "")
			events = ev
		default:
			s := ""
			fs.StringVar(&s, f.name, "", "")
			stringVals[f.name] = &s
		}
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Invocation{}, &usageError{fmt.Sprintf("%s: help is documented as git-feedback --help", cmd)}
		}
		return Invocation{}, &usageError{fmt.Sprintf("%s: %v", cmd, err)}
	}

	if cmd == "store" {
		// The store command never takes a URL positional.
		if fs.NArg() != 0 {
			return Invocation{}, &usageError{fmt.Sprintf("%s: unexpected argument %q", cmd, fs.Arg(0))}
		}
	} else if urlFirst {
		if fs.NArg() != 0 {
			return Invocation{}, &usageError{fmt.Sprintf("%s: unexpected argument %q", cmd, fs.Arg(0))}
		}
	} else {
		switch {
		case fs.NArg() == 0:
			return Invocation{}, &usageError{fmt.Sprintf("%s: missing <URL>", cmd)}
		case fs.NArg() > 1:
			return Invocation{}, &usageError{fmt.Sprintf("%s: unexpected argument %q", cmd, fs.Arg(1))}
		default:
			inv.URL = fs.Arg(0)
		}
	}

	for name, p := range stringVals {
		inv.Flags[name] = *p
	}
	for name, p := range boolVals {
		inv.Bools[name] = *p
	}
	if events != nil {
		inv.Events = []string(*events)
	}
	return inv, nil
}

// Run executes one CLI invocation: it parses argv, wires the signal- and
// timeout-derived context, runs the handler, writes exactly one JSON
// envelope to stdout, and returns the process exit code. Diagnostics go to
// stderr.
func Run(argv []string, stdout, stderr io.Writer) int {
	command := ""
	if len(argv) > 0 {
		command = argv[0]
	}

	// --help on the store command prints the store-specific help on stdout
	// and exits 0; plain `store` without --help keeps the missing-subcommand
	// usage error.
	if command == "store" && hasHelpFlag(argv[1:]) {
		fmt.Fprint(stdout, StoreUsage)
		return ExitOK
	}

	inv, err := parseInvocation(argv)
	if err != nil {
		writeResult(stdout, errorResult(command, err))
		fmt.Fprintln(stderr, err.Error())
		return ExitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if raw := inv.Flags["timeout"]; raw != "" {
		d, derr := time.ParseDuration(raw)
		if derr != nil || d <= 0 {
			writeResult(stdout, errorResult(inv.Command, &usageError{fmt.Sprintf("invalid --timeout %q", raw)}))
			fmt.Fprintf(stderr, "invalid --timeout %q\n", raw)
			return ExitUsage
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	if inv.Command == "wait" && inv.Flags["timeout"] == "" {
		// Apply the pinned default deadline; an explicit --timeout above
		// already derived the context.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, waitDefaultTimeout)
		defer cancel()
	}

	result, herr := inv.handler(ctx, inv)
	if herr != nil {
		result = errorResult(inv.Command, herr)
		// The envelope is the machine-readable record; this line keeps the
		// stderr diagnostic promise for handler failures.
		fmt.Fprintln(stderr, herr.Error())
	}

	// A terminating signal exits with its shell code and no stdout output.
	select {
	case sig := <-sigCh:
		return SignalExitCode(sig)
	default:
	}

	// Test-only crash point: after the result is computed, before the
	// envelope is written. No-op in the default build.
	CrashBeforeOutput()

	// Test-only: inject a pending signal before the envelope write to
	// exercise the post-write signal re-check. No-op in the default build.
	if sig, ok := testPendingSignalFn(); ok {
		select {
		case sigCh <- sig:
		default:
		}
	}

	if err := writeResult(stdout, result); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitOperational
	}
	// One-line status summary on the diagnostics channel: monitor commands
	// point at `wait` and read the outcome from the process output without
	// parsing the envelope.
	if inv.Command == "wait" &&
		(result.Status == StatusEvents || result.Status == StatusTimeout) {
		fmt.Fprintf(stderr, "git-feedback wait: status=%s events=%d has_more=%t\n",
			result.Status, len(result.Events), result.HasMore)
	}
	// A signal that arrived during the envelope write still wins on exit
	// code, even though the envelope may already be written.
	select {
	case sig := <-sigCh:
		return SignalExitCode(sig)
	default:
	}
	if result.Error != nil {
		var ue *usageError
		if errors.As(herr, &ue) {
			return ExitUsage
		}
		return ExitOperational
	}
	// Opt-in status-driven codes: a monitor that signals on process exit
	// separates delivered events from the deadline expiry without parsing
	// stderr or the envelope. Errors and signals above keep their own codes.
	if inv.Command == "wait" && inv.Bools["exit-on-events"] {
		switch result.Status {
		case StatusEvents:
			return ExitEvents
		case StatusHeadChange:
			return ExitHeadChanged
		}
	}
	return ExitOK
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

// hasHelpFlag reports whether args contain a help request (--help or -h).
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// errorResult builds the error envelope for a failure.
func errorResult(command string, err error) Result {
	r := Result{Command: command, Status: StatusError}
	var se *statusError
	var ue *usageError
	switch {
	case errors.As(err, &se):
		r.Error = &Error{Code: se.code, Message: se.message, Retryable: se.retryable}
	case errors.As(err, &ue):
		r.Error = &Error{Code: "usage", Message: ue.msg}
	default:
		r.Error = &Error{Code: "internal", Message: err.Error()}
	}
	return r
}
