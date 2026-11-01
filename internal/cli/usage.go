package cli

// Usage is the root command's long help, rendered above the generated
// Usage/Available Commands/Flags sections. The Typical flow and Contract
// blocks are the agent-facing surface: they describe the replay flow and
// the envelope/exit-code contract that registry and harness callers key on.
const Usage = `git-feedback observes GitHub review feedback and delivers replayable events.

Typical flow:
  reconcile → wait → ack
  wait delivers pending events; ack records their receipt. wait itself
  acknowledges nothing, so an interrupted consumer replays the same events.

Targets:
  <URL> is a GitHub.com pull request URL (/pull/N) or issue URL (/issues/N).
  An issue target collects its conversation comments only: observed_head and
  expected_head stay null, and --head, --skip-head-changed, and
  --skip-empty-reviews have nothing to act on.

Contract:
  Every subcommand writes exactly one JSON envelope with a "status" field to
  stdout; diagnostics go to stderr. Operational errors exit 1, usage errors
  exit 2. Registry and harness callers should key on "status", not the exit
  code. The one exception is opt-in and wait-only: --exit-on-events exits 3
  when it delivers events and 4 when a pinned head moved, so a process
  monitor reads the outcome from the exit status alone. timeout stays 0.
  Full machine contract: docs/contract.md.

inbox and wait exclude events authored by the stream's own account by
default. --include-self opts back in; the legacy --exclude-self spelling is
accepted as a no-op and reports a deprecation notice on stderr.
`

// StoreUsage is the store command's long help. The store is a disposable
// cache, so both subcommands are one-liners; the flow and envelope contract
// live in the bare help and docs/contract.md.
const StoreUsage = `git-feedback store — disposable store utilities.

The store is a cache: delete it freely and the next collection starts from a
new baseline.

Each subcommand writes exactly one JSON envelope with a "status" field;
usage errors exit 2. Full machine contract: docs/contract.md.
`

// commandShort is the one-line description shown in the command listing and
// as the subcommand's Short help. The wording matches the hand-written help
// these descriptions replaced.
var commandShort = map[string]string{
	"reconcile": "collect feedback and establish the baseline",
	"snapshot":  "export a stored collection to a file",
	"inbox":     "list pending events for a consumer",
	"show":      "fetch one event's stored object content (body, state, comments)",
	"ack":       "acknowledge delivered event IDs",
	"wait":      "block until new events arrive or --timeout elapses (bounded polling)",
	"store":     "disposable store utilities",
}

// storeSubShort is the one-line description for the store subcommands.
var storeSubShort = map[string]string{
	"path":  "print the resolved store directory; never opens the store",
	"reset": "delete the store, wholly or scoped to one --target URL",
}

// flagUsage is the generated flag documentation: each entry renders in the
// Flags section of the commands that accept the flag.
var flagUsage = map[string]string{
	"json":               "no-op; output is always a single JSON envelope",
	"account":            "GitHub account login for the stream; defaults to the gh CLI account",
	"state-dir":          "override the state directory (defaults to a per-user cache directory)",
	"head":               "expected head SHA; report head_changed when the observed head no longer matches",
	"snapshot":           "ID of the stored snapshot to export",
	"output":             "file path for the exported snapshot JSON",
	"consumer":           "name identifying this reader of the stream",
	"limit":              "maximum events to return (1-200, default 50)",
	"after":              "cursor from a previous inbox page; fetch the next page",
	"ids-only":           "return event IDs only, without full event records",
	"include-self":       "include events authored by the stream's own account (excluded by default)",
	"exclude-self":       "deprecated no-op; self-authored events are already excluded",
	"event":              "event ID to acknowledge; repeat the flag for multiple IDs",
	"events-from":        "read event IDs from a file, or - for stdin",
	"show.event":         "event ID to display",
	"timeout":            "maximum wait duration (default 30m)",
	"skip-empty-reviews": "skip review events whose review body and comments are empty",
	"skip-head-changed":  "suppress head_changed events and keep waiting",
	"exit-on-events":     "exit 3 when events are delivered and 4 when a pinned head moved",
	"target":             "scope the reset to one target URL",
}
