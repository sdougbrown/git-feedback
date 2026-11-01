// Package cli implements the git-feedback command boundary: argv parsing for
// the subcommands, the result envelope, and exit-code/signal plumbing.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
		flags:  []flagSpec{{"consumer", flagString}, {"limit", flagString}, {"after", flagString}, {"ids-only", flagBool}, {"include-self", flagBool}, {"exclude-self", flagBool}, {"skip-empty-reviews", flagBool}}, // exclude-self is a legacy no-op; exclusion is the default.
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
		flags:  []flagSpec{{"consumer", flagString}, {"timeout", flagString}, {"head", flagString}, {"limit", flagString}, {"include-self", flagBool}, {"exclude-self", flagBool}, {"skip-empty-reviews", flagBool}, {"skip-head-changed", flagBool}, {"exit-on-events", flagBool}}, // exclude-self is a legacy no-op; exclusion is the default.
		handle: handleWait,
	},
	"store": {
		flags:  []flagSpec{{"target", flagString}},
		handle: handleStore,
	},
}

// version is the build version reported by --version; main sets it from the
// build-time ldflags value.
var version = "dev"

// SetVersion records the build version reported by --version.
func SetVersion(v string) { version = v }

// execution collects what one command execution produced. The same cobra
// tree serves two modes: dispatch (handlers run, results are captured) and
// parse-only (parseInvocation runs it to validate argv and capture the
// Invocation without executing anything).
type execution struct {
	inv    Invocation
	result Result
	err    error
	ran    bool
}

// asUsageError guarantees the usage-envelope error code for every
// dispatch-level failure: unknown commands, flag errors, and positional
// validation all leave the tree as caller errors, whatever error type
// cobra raised internally.
func asUsageError(err error) error {
	var ue *usageError
	if errors.As(err, &ue) {
		return ue
	}
	return &usageError{err.Error()}
}

// addSpecFlags registers one spec's flags on a flag set with the generated
// usage strings. A command-scoped "<command>.<flag>" entry wins over the
// plain flag name, so one flag can document differently per command
// (ack's repeatable --event vs show's single --event).
func addSpecFlags(fs *pflag.FlagSet, name string, flags []flagSpec) {
	for _, f := range flags {
		usage, ok := flagUsage[name+"."+f.name]
		if !ok {
			usage = flagUsage[f.name]
		}
		switch f.kind {
		case flagBool:
			fs.Bool(f.name, false, usage)
		case flagRepeat:
			fs.StringArray(f.name, nil, usage)
		default:
			fs.String(f.name, "", usage)
		}
	}
}

// exactlyOneURL validates the URL positionals for URL-taking commands.
func exactlyOneURL(name string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return &usageError{fmt.Sprintf("%s: missing <URL>", name)}
		case len(args) > 1:
			return &usageError{fmt.Sprintf("%s: unexpected argument %q", name, args[1])}
		}
		return nil
	}
}

// noArgs validates that a nested subcommand took no positionals.
func noArgs(name string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return &usageError{fmt.Sprintf("%s: unexpected argument %q", name, args[0])}
		}
		return nil
	}
}

// invocationFrom builds the Invocation from one parsed cobra command.
func invocationFrom(cmd *cobra.Command, name, sub string, args []string) (Invocation, error) {
	spec := specs[name]
	inv := Invocation{
		Command: name,
		Sub:     sub,
		URL:     "",
		Flags:   map[string]string{},
		Bools:   map[string]bool{},
		handler: spec.handle,
	}
	if name != "store" {
		inv.URL = args[0] // validated by exactlyOneURL
	}
	fs := cmd.Flags()
	for _, f := range append(append([]flagSpec{}, commonFlags...), spec.flags...) {
		// Nested subcommands share the parent's spec: skip spec flags the
		// subcommand does not carry (store path has no --target).
		if fs.Lookup(f.name) == nil {
			continue
		}
		switch f.kind {
		case flagBool:
			b, err := fs.GetBool(f.name)
			if err != nil {
				return Invocation{}, err
			}
			inv.Bools[f.name] = b
		case flagRepeat:
			arr, err := fs.GetStringArray(f.name)
			if err != nil {
				return Invocation{}, err
			}
			inv.Events = arr
		default:
			s, err := fs.GetString(f.name)
			if err != nil {
				return Invocation{}, err
			}
			inv.Flags[f.name] = s
		}
	}
	return inv, nil
}

// timeoutDuration parses the --timeout value once for both validation and
// the deadline: ok is false when --timeout is absent, and a malformed or
// non-positive value is a caller error.
func timeoutDuration(inv Invocation) (time.Duration, bool, error) {
	raw := inv.Flags["timeout"]
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, true, &usageError{fmt.Sprintf("invalid --timeout %q", raw)}
	}
	return d, true, nil
}

// deprecationNotice reports the legacy --exclude-self spelling on stderr.
func deprecationNotice(inv Invocation, stderr io.Writer) {
	if inv.Bools["exclude-self"] {
		fmt.Fprintln(stderr, "--exclude-self is deprecated and is a no-op; use --include-self to deliver your own events")
	}
}

// dispatchRunE builds the RunE shared by every dispatching command: capture
// the Invocation, and in dispatch mode run the handler and record the result.
func dispatchRunE(exec *execution, runHandlers bool, name, sub string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		inv, err := invocationFrom(cmd, name, sub, args)
		if err != nil {
			return err
		}
		exec.inv = inv
		exec.ran = true
		if !runHandlers {
			return nil
		}
		deprecationNotice(inv, cmd.ErrOrStderr())
		// A caller error (bad --timeout) propagates: Execute surfaces it as
		// the usage envelope with exit 2.
		d, hasTimeout, terr := timeoutDuration(inv)
		if terr != nil {
			return terr
		}
		ctx := cmd.Context()
		if hasTimeout {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		} else if inv.Command == "wait" {
			// Apply the pinned default deadline.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, waitDefaultTimeout)
			defer cancel()
		}
		result, herr := specs[inv.Command].handle(ctx, inv)
		if herr != nil {
			result = errorResult(inv.Command, herr)
		}
		exec.result = result
		exec.err = herr
		return nil
	}
}

// newRootCommand builds the cobra command tree from specs. The specs table
// stays the single source of truth: every flag, positional rule, and handler
// binding below is derived from it.
func newRootCommand(exec *execution, runHandlers bool) *cobra.Command {
	root := &cobra.Command{
		Use:     "git-feedback",
		Short:   "Observe GitHub review feedback and deliver replayable events",
		Long:    Usage,
		Version: version,
		// Dispatch never returns operational failures: everything that
		// escapes the tree is a caller error, so keep cobra silent and map
		// errors to the usage envelope in Run.
		SilenceErrors:      true,
		SilenceUsage:       true,
		DisableSuggestions: true,
		// The generated completion command is not part of the documented
		// surface; the hidden __complete protocol stays available.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			return &usageError{"missing subcommand"}
		},
	}
	// Flag errors and unknown commands are caller errors: code them as
	// usage errors so the envelope reports code "usage".
	// Match the historical `git-feedback <version>` output.
	root.SetVersionTemplate("git-feedback {{.Version}}\n")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{fmt.Sprintf("%s: %v", cmd.Name(), err)}
	})
	// The default help command reports unknown topics on stdout and exits 0,
	// breaching the one-JSON-envelope contract; this replacement routes
	// unknown help topics to the usage envelope instead.
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := root.Find(args)
			if err != nil || (len(args) > 0 && target == root) {
				return &usageError{fmt.Sprintf("unknown help topic %q", strings.Join(args, " "))}
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	})
	addSpecFlags(root.PersistentFlags(), "", commonFlags)

	for name, spec := range specs {
		if name == "store" {
			root.AddCommand(newStoreCommand(exec, runHandlers, spec))
			continue
		}
		cmd := &cobra.Command{
			Use:   name + " <URL>",
			Short: commandShort[name],
			Args:  exactlyOneURL(name),
			RunE:  dispatchRunE(exec, runHandlers, name, ""),
		}
		addSpecFlags(cmd.Flags(), name, spec.flags)
		root.AddCommand(cmd)
	}
	return root
}

// newStoreCommand builds the store command and its nested path/reset
// subcommands. The envelope's command field stays "store"; Sub selects the
// nested subcommand.
func newStoreCommand(exec *execution, runHandlers bool, spec commandSpec) *cobra.Command {
	store := &cobra.Command{
		Use:   "store",
		Short: commandShort["store"],
		Long:  StoreUsage,
		RunE: func(cmd *cobra.Command, args []string) error {
			return &usageError{`store requires a subcommand: "path" or "reset"`}
		},
	}
	path := &cobra.Command{
		Use:   "path",
		Short: storeSubShort["path"],
		Args:  noArgs("store path"),
		RunE:  dispatchRunE(exec, runHandlers, "store", "path"),
	}
	reset := &cobra.Command{
		Use:   "reset",
		Short: storeSubShort["reset"],
		Args:  noArgs("store reset"),
		RunE:  dispatchRunE(exec, runHandlers, "store", "reset"),
	}
	addSpecFlags(reset.Flags(), "store", spec.flags)
	store.AddCommand(path, reset)
	return store
}

// init cross-checks the help metadata against the specs table: a command or
// flag added to specs without a matching help entry would otherwise render
// blank help text instead of failing here.
func init() { checkHelpMetadata() }

// checkHelpMetadata panics when any spec command or flag lacks its help
// entry. Extracted from init so the invariant itself is testable.
func checkHelpMetadata() {
	for name, spec := range specs {
		if commandShort[name] == "" {
			panic(fmt.Sprintf("cli: command %q has no help short", name))
		}
		for _, f := range spec.flags {
			if flagUsage[name+"."+f.name] == "" && flagUsage[f.name] == "" {
				panic(fmt.Sprintf("cli: flag %q on command %q has no usage string", f.name, name))
			}
		}
	}
	for _, f := range commonFlags {
		if flagUsage[f.name] == "" {
			panic(fmt.Sprintf("cli: common flag %q has no usage string", f.name))
		}
	}
	for _, sub := range []string{"path", "reset"} {
		if storeSubShort[sub] == "" {
			panic(fmt.Sprintf("cli: store subcommand %q has no help short", sub))
		}
	}
}

// parseInvocation parses argv (without the program name) for one subcommand.
// It executes the cobra tree in parse-only mode: handlers do not run, and
// flags and positionals are validated by the same Args functions that
// dispatch uses. Both `git-feedback <sub> <URL> [flags]` and
// `git-feedback <sub> [flags] <URL>` are accepted: pflag parses flags
// interspersed with positionals, and exactly one remaining positional URL
// is required. Other interleavings and extra positionals are rejected as
// usage errors.
func parseInvocation(argv []string) (Invocation, error) {
	if len(argv) == 0 {
		return Invocation{}, &usageError{"missing subcommand"}
	}
	exec := &execution{}
	root := newRootCommand(exec, false)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(argv)
	if err := root.Execute(); err != nil {
		return Invocation{}, asUsageError(err)
	}
	if !exec.ran {
		// Help paths (bare --help, the help command) dispatch nothing; a
		// parse-only caller asking for an Invocation is a caller error.
		return Invocation{}, &usageError{"missing subcommand"}
	}
	return exec.inv, nil
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

	exec := &execution{}
	root := newRootCommand(exec, true)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(argv)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := root.ExecuteContext(ctx); err != nil {
		// Help never reaches here (cobra writes plain help and returns
		// nil); every error that does is a caller error with the usage
		// envelope and exit 2.
		err = asUsageError(err)
		writeResult(stdout, errorResult(command, err))
		fmt.Fprintln(stderr, err.Error())
		return ExitUsage
	}
	// Help paths (bare --help, the help command, <command> --help) print
	// plain text and exit 0 without an envelope; ran is set only when a
	// subcommand dispatched.
	if !exec.ran {
		return ExitOK
	}
	result := exec.result

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

	// The envelope is the machine-readable record; this line keeps the
	// stderr diagnostic promise for handler failures.
	if exec.err != nil {
		fmt.Fprintln(stderr, exec.err.Error())
	}

	if err := writeResult(stdout, result); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitOperational
	}
	// One-line status summary on the diagnostics channel: monitor commands
	// point at `wait` and read the outcome from the process output without
	// parsing the envelope.
	if result.Command == "wait" &&
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
		if exec.err != nil && errors.As(exec.err, &ue) {
			return ExitUsage
		}
		return ExitOperational
	}
	// Opt-in status-driven codes: a monitor that signals on process exit
	// separates delivered events from the deadline expiry without parsing
	// stderr or the envelope. Errors and signals above keep their own codes.
	if exec.inv.Command == "wait" && exec.inv.Bools["exit-on-events"] {
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
