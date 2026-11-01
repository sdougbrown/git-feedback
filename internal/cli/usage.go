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
  Full machine contract: docs/contract.md; the parseable interface (commands,
  flags, statuses, exit codes) is git-feedback schema.

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
