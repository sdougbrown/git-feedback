package cli

// Usage is the full help text printed for bare `git-feedback --help`. The
// Commands and Contract blocks are the agent-facing surface: they describe
// each command's role, the replay flow, and the envelope/exit-code contract
// that registry and harness callers key on.
const Usage = `git-feedback observes GitHub review feedback and delivers replayable events.

Usage:
  git-feedback reconcile <URL> [--head <SHA>] [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback snapshot <URL> --snapshot <ID> --output <FILE> [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback inbox <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--limit 50] [--after <CURSOR>] [--ids-only] [--include-self] [--skip-empty-reviews] [--json]
  git-feedback ack <URL> --consumer <NAME> --event=<ID> [--event=<ID>...] [--events-from -] [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback show <URL> --event <ID> [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback wait <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--timeout 30m] [--head <SHA>] [--limit 50] [--include-self] [--skip-empty-reviews] [--skip-head-changed] [--exit-on-events] [--json]
  git-feedback store path [--state-dir <DIR>]
  git-feedback store reset [--target <URL>] [--state-dir <DIR>]

Commands:
  reconcile     collect feedback and establish the baseline
  snapshot      export a stored collection to a file
  inbox         list pending events for a consumer
  show          fetch one event's stored object content (body, state, comments)
  ack           acknowledge delivered event IDs
  wait          block until new events arrive or --timeout elapses (bounded polling)
  store path    print the resolved store directory; never opens the store
  store reset   delete the store, wholly or scoped to one --target URL

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

Flags:
  --version    print version and exit
  --help       show this help

  inbox and wait exclude events authored by the stream's own account by
  default. --include-self opts back in; the legacy --exclude-self spelling is
  accepted as a no-op.
`

// StoreUsage is the help text printed for `store --help`. The store is a
// disposable cache, so both subcommands are one-liners; the flow and envelope
// contract live in the bare help and docs/contract.md.
const StoreUsage = `git-feedback store — disposable store utilities.

The store is a cache: delete it freely and the next collection starts from a
new baseline.

Usage:
  git-feedback store path [--state-dir <DIR>]
      print the resolved store directory; never opens the store
  git-feedback store reset [--target <URL>] [--state-dir <DIR>]
      delete the store, wholly or scoped to one --target URL

Each subcommand writes exactly one JSON envelope with a "status" field;
usage errors exit 2. Full machine contract: docs/contract.md.
`
