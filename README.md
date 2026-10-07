# git-feedback

`git-feedback` observes GitHub review feedback on a pull request, commits
complete inventories and changes, and delivers replayable events to independent
consumers. It works without a clone, repository administration, a GitHub App,
or a harness-specific runtime. Install it on `PATH` to expose both
`git-feedback` and `git feedback`.

An empty delivery inbox does not mean findings are triaged, threads are
resolved, fixes are correct, or a reviewer has finished.

## Install

```sh
make install
```

This runs `go install ./cmd/git-feedback` and places `git-feedback` on your Go
`bin` directory. Ensure that directory is on `PATH`.

## Invocation

Run the binary directly, or through Git's subcommand mechanism:

```sh
git-feedback <subcommand> [flags] <URL>
git feedback <subcommand> [flags] <URL>
```

Both forms are equivalent; `git feedback` is resolved by Git to the
`git-feedback` binary on `PATH`. Use `git-feedback --help` for usage (Git
intercepts `git feedback --help` before it reaches the tool).

```
git-feedback reconcile <URL> [--head <SHA>] [--account <LOGIN>] [--state-dir <DIR>] [--json]
git-feedback snapshot <URL> --snapshot <ID> --output <FILE> [--account <LOGIN>] [--state-dir <DIR>] [--json]
git-feedback inbox <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--limit 50] [--after <CURSOR>] [--json]
git-feedback ack <URL> --consumer <NAME> --event=<ID> [--event=<ID>...] [--account <LOGIN>] [--state-dir <DIR>] [--json]
git-feedback wait <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--timeout 30m] [--head <SHA>] [--limit 50] [--json]
```

Every subcommand writes exactly one JSON envelope to stdout (see
`docs/contract.md`). `--json` is accepted everywhere and is a no-op: output is
always JSON. Operational errors exit 1 and usage errors exit 2; both still print
the envelope.

## Commands

- `reconcile` — attempt one complete collection and return a compact summary,
  immutable snapshot ID, and scheduling/failure metadata. It contains no polling
  loop.
- `snapshot` — export one stored immutable snapshot to a private file. Returns
  its ID, path, counts, and digest. It never silently substitutes another
  snapshot ID.
- `inbox` — read unacknowledged events for one target and consumer. Reading
  acknowledges nothing.
- `ack` — acknowledge exactly the supplied event IDs for that consumer and
  target, transactionally and idempotently. Unknown or out-of-scope IDs reject
  the whole request. There is no acknowledge-all or acknowledge-latest
  operation.
- `wait` — bounded waiting. It owns polling only when used standalone; a
  controller uses `reconcile` and `inbox` instead.

## Shared state and account scope

`--state-dir <DIR>` selects one shared SQLite store. The default is
`os.UserConfigDir()/git-feedback/state`. Separate stores cannot deduplicate each
other.

Observations are partitioned by authenticated account and host. Different
accounts never silently combine private feedback in one target stream; each
`(target, account)` pair is its own stream. `--account <LOGIN>` selects an
account. Remote reconciliation otherwise verifies the active `gh` account. Local
commands infer an account only when exactly one stored account matches the
target; ambiguity is an error.

## Snapshot reading

`snapshot` reads one stored immutable snapshot and writes it to a private file
(mode 0600) with atomic no-replace semantics. Exported snapshots contain complete
bodies, sorted by object ID. The result envelope reports the snapshot ID, file
path, object counts, and a `sha256` digest of the exported bytes. A bounded
display never truncates the stored inventory.

## Inbox pagination

`inbox` returns unacknowledged events for the selected target and consumer,
ordered ascending, with `has_more` and `next_cursor`. `--limit` defaults to 50
(valid range 1–200). `--after <CURSOR>` continues a page sequence with a fixed
high-water mark; newer events remain pending for the next sequence. The cursor is
base64url of `{"t":"<Target.ID>","a":"<account>","c":"<consumer>","hw":<seq>,"after":<seq>}`
and is bound to the selected account, target, and consumer. A malformed or
cross-stream/cross-consumer cursor is a usage error (exit 2). A new consumer name
sees all history.

## Explicit acknowledgement

`ack` acknowledges exactly the event IDs you supply, for that consumer and
target, transactionally and idempotently. It is the only way an event stops
being pending for a consumer. **Acknowledgements are delivery receipts, not
triage dispositions**: acknowledging an event records that a consumer received
it. It does not record that a finding was triaged, a thread resolved, a fix is
correct, or a reviewer has finished.

## Deadlines

`wait` is bounded by `--timeout` (default 30m). It retries transient network,
5xx, and rate-limit outcomes until the deadline, then returns `timeout` with the
last attempt in `attempt`. It returns immediately on fatal errors (auth, not
found, account mismatch, unsupported host, or a pinned-head mismatch). SIGINT
exits 130 and SIGTERM exits 143, with no stdout and no acknowledgements. There is
no resident service; stopping the waiting process stops network work.

## Read-only guarantees

`git-feedback` uses user-authenticated read-only GitHub APIs. It makes no remote
mutations: it does not create a PR, comment, review, resolution, subscription, or
review request. Any non-GET REST method is rejected by the transport, and GraphQL
is limited to read queries. `gh` is a credential helper only. Secrets never
appear in stdout, stderr, the database, or snapshot exports.

## cli-registry integration

`examples/cli-registry/git-feedback.json` registers the one-shot `reconcile`,
`snapshot`, `inbox`, and `ack` tools (not `wait`). See `docs/contract.md` for the
controller and monitor modes, the monitor loop idiom, and the rationale for
excluding `wait`. `examples/watch-loop.sh` is a ready-made wrapper that runs
`wait`, acknowledges the delivered event IDs, and respawns until interrupted.

The manifest declares an explicit `env` of `{"HOME": "env:HOME", "PATH":
"env:PATH"}`. On a machine where `gh` reads a non-default config location or a
specific token, forward that variable by adding it to the tool's `env` (for
example `GH_CONFIG_DIR` or `XDG_CONFIG_HOME`, or a selected token variable). Do
not require optional variables that may be unset; the registry treats a missing
`env:NAME` as a load error. When integrating callers with different config
environments, use an explicit `--state-dir` so each caller selects its own store.

## Registry smoke test

`make registry-test` runs the cli-registry smoke gate (`integration/registry_test.go`),
exercising the shipped manifest through the reference `cli-registry` binary. It is
kept out of `make verify` because it depends on an external repo. Run it when you
change the manifest or want to confirm the registry integration end-to-end. It
builds `cli-registry` from `~/Code/cli-registry` when `CLI_REGISTRY_BIN` is unset;
set `CLI_REGISTRY_BIN` to use a prebuilt binary instead.
