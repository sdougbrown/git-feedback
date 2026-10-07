# git-feedback contract

This document reproduces the Result contract and the event/cursor definitions
verbatim from the implementation plan, and shows the two supported integration
modes side by side. It is the reference for harnesses that drive
`git-feedback` without a resident service.

## Result contract

Every subcommand writes exactly one JSON value to stdout. Go types live in
`internal/cli/result.go`.

```json
{
  "schema": "git-feedback/v1",
  "command": "reconcile|snapshot|inbox|ack|wait",
  "status": "updated|unchanged|deferred|busy|head_changed|ok|events|timeout|error",
  "target": {"id": "", "url": "", "forge": "", "host": "", "repo": "", "number": 0},
  "account": "<login>|null",
  "observed_head": "<sha>|null",
  "expected_head": "<sha>|null",
  "snapshot": {"id": "", "collected_start": "", "collected_end": "", "complete": true, "object_counts": {"thread": 0, "review": 0, "comment": 0}},
  "attempt": {"at": "", "ok": true, "complete": true, "error_code": null, "next_due": "<rfc3339>"},
  "freshness": {"snapshot_observed_at": "", "stale": false},
  "reviewer_completion": "unknown",
  "events": [],
  "has_more": false,
  "next_cursor": null,
  "export": {"path": "", "digest": "", "counts": {}},
  "error": null
}
```

- `reconcile` returns `updated`, `unchanged`, `deferred`, `busy`, or `head_changed`. `snapshot`, `inbox`, and `ack` return `ok`. `wait` returns `events` or `timeout`. All of these exit 0.
- `error` is `{"code": "", "message": "", "retryable": false}` with `status: "error"`. Operational errors exit 1; usage errors exit 2. Both still print the envelope.
- `snapshot` may be null when no complete snapshot exists. `attempt` is nullable and reports the latest attempt separately from `snapshot.complete`. Deferred admission or failed refresh must never report a successful current attempt just because cached state exists.
- `freshness.snapshot_observed_at` is the last successful complete observation for the returned snapshot, including unchanged collections. Set `stale` after the collection interval elapses or a subsequent refresh fails/drifts. Immutable snapshot collection timestamps remain unchanged.
- `reviewer_completion` is always `"unknown"` in v1. `TestEnvelopeFields` asserts its presence on every command.
- Diagnostics go to stderr. Secrets never appear in either stream.
- Registry callers inspect `status`, not exit 0, as a readiness gate.

## Events

- Event kinds: `initial_observation`, `revision`, `not_observed`, `head_changed`. A feedback object reappearing after `not_observed` gets a new `revision`.
- Event fields: `id, kind, object_kind, object_id, revision, url, snapshot_id, observed_at`; head events also carry `head_before` and `head_after`. Counters are per `(stream_id, object_kind, object_id)`, starting at 1 and increasing for every observed occurrence, including disappearance/reappearance.
- Event ID is `e<seq>` where `seq` is `INTEGER PRIMARY KEY AUTOINCREMENT` on `events`. IDs are opaque to callers and never content hashes.
- Initial publication emits a target `initial_observation` containing the head. A subsequent complete stable-head collection emits a target `head_changed` event when the head differs, even with identical feedback. Commit it with the new snapshot so lost output remains replayable. Head drift during collection and pinned-head mismatches instead persist incomplete-attempt metadata; they do not publish mixed-head snapshots.

## Inbox cursor

- Cursor is base64url of JSON `{"t":"<Target.ID>","a":"<account>","c":"<consumer>","hw":<seq>,"after":<seq>}`. Bind it to the selected account/target/consumer and validate `0 <= after <= hw <= current stream maximum`. `hw` fixes the sequence's high-water mark; `after` is the last returned seq. Reject malformed or mismatched cursors before reading events.
- Pending events are the selected stream's events minus that consumer's acknowledgements, with `seq > after` and `seq <= hw`, ordered ascending. Without `--after`, capture that stream's current maximum seq. An empty stream has high-water mark 0. Later events stay pending for the next page sequence.
- `--limit` defaults to 50; valid range is 1–200. A malformed or cross-stream/consumer cursor is a usage error (exit 2).
- Consumer names match `^[A-Za-z0-9._-]{1,64}$`. A new consumer name sees all history.

## Integration modes

Both modes replay unacknowledged events, and both treat an acknowledgement as a
delivery receipt, not a triage disposition.

### Controller mode

A controller schedules the registered one-shot `reconcile` and `inbox` commands,
delivers the returned events to an agent, then calls `ack` after receipt.

- The controller owns the polling cadence: it invokes `reconcile` on its own
  schedule and reads `inbox` for the consumer it names.
- `reconcile` performs one collection cycle and returns; it contains no polling
  loop. `inbox` reads unacknowledged events and acknowledges nothing.
- After the agent receives the delivered events, the controller calls `ack` with
  the exact event IDs to record delivery.
- The registered tools are `reconcile`, `snapshot`, `inbox`, and `ack`
  (`examples/cli-registry/git-feedback.json`). `wait` is not registered.

### Monitor mode

A process monitor launches bounded `wait`; the CLI owns polling and returns one
JSON result on events or deadline. The receiving harness calls `ack` separately.

- The monitor invokes `wait` with a `--timeout` deadline. `wait` owns the
  polling loop, reusing the shared reconciliation engine and its persisted
  schedule, cadence, and rate gates.
- `wait` returns a single JSON result: `status: "events"` with the pending
  events, or `status: "timeout"` when the deadline elapses.
- After the harness receives the result, it calls `ack` separately with the event
  IDs it wants to record as delivered. `wait` itself acknowledges nothing.

### Monitor loop idiom

Operating a monitor mode loop in practice surfaces rules that the envelope
alone does not capture. These are conventions for harness authors, not tool
behavior.

- **Detect edges, not levels.** A level-based probe ("pending > 0 → act")
  fires once and then dedupes identical output, so a second round of findings
  never pings. Poll a cheap read — for example `inbox --consumer canary --json
  | jq '.events | length'` — and act on a *change* in the value, or run `wait`
  and treat each returned batch as its own edge.
- **Use a dedicated canary consumer for the watcher.** Its unacknowledged
  count then stays independent of the orchestrator's paging state, and the raw
  edge detector never loses events to the orchestrator's acknowledgements.
- **Ack after handling to reset the baseline.** Acknowledgement is a delivery
  receipt, not a triage disposition; once the delivered events are handled,
  the pending count returns to its baseline and the next change is a clean
  edge.
- **Treat a ping as "inventory changed", never "a reviewer spoke".** The
  orchestrator's own actions — replies, re-resolutions, summary comments —
  generate events too. v1 does not classify own-actions; the
  ack-after-handling baseline is what keeps self-noise from compounding.
- **Do not run ad-hoc reconciles while the monitor holds the edge.** A manual
  reconcile between polls can consume the change the watcher was about to
  report. If polling is needed, use `wait` (or a reconcile schedule that is
  self-gating) rather than out-of-band refreshes.
- **An inbox-only watcher is blind.** The local store never refreshes by
  itself; a watcher that only polls `inbox` sees nothing new. Poll with
  `reconcile`/`wait` so collection happens on the watch cadence — reconcile is
  self-gating (deferred when not due), so polling is rate-safe.
- **`reviewer_completion` stays `"unknown"`; completion is a convention.**
  Whether a reviewer is done is an agreement between the reviewer and the
  monitor (for example a reaction on a serviced review request), not tool
  state. The tool deliberately stays out of it.
- **The watcher lives until the last request is serviced.** Standing the
  monitor down while a review request is still pending is how rounds get
  missed. Keep the loop armed until the pending request is consumed or the
  work merges.

`examples/watch-loop.sh` is a minimal wrapper that removes the most common
manual step: it runs `wait`, acknowledges the delivered event IDs, and
respawns, so a harness cannot forget the respawn.

### Why `wait` is not registered

Registering `wait` is technically possible, but it is excluded by design to avoid
blocking tool calls and nested polling. A registered `wait` would hold a tool
call open for up to its deadline while the CLI polls, and a harness that also
scheduled `reconcile` would nest a second polling loop inside the first. The
controller mode uses the one-shot `reconcile` + `inbox` pair instead; the monitor
mode runs `wait` as a bounded process outside the registry. In both modes,
acknowledgement records delivery, not triage, and unacknowledged events are
replayed.
