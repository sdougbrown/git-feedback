---
name: git-feedback
description: "Operate the git-feedback CLI to collect PR review feedback, read only what changed since the last pass, watch a PR for new feedback, and acknowledge delivered events. Use when a task needs PR review threads, reviews, or top-level comments; when watching a PR for reviewer activity; or when the user says 'check for new feedback', 'watch this PR', 'what changed on the PR', or mentions git-feedback. git-feedback is read-only toward GitHub: it does not post replies or resolve threads."
argument-hint: '[PR URL]'
compatibility: "Requires git-feedback 0.5.0 or later on PATH and an authenticated gh. Falls back to the forge API (gh pr view / gh api) when unavailable."
---

$ARGUMENTS: a PR URL.

`git-feedback` collects a PR's review threads, reviews, and top-level comments
into immutable snapshots and delivers per-object change events to named
consumers. It is read-only toward GitHub. Machine contract:
`docs/contract.md` in the git-feedback repository; read it only when an
envelope field below is not enough.

## Version gate

Check that `git-feedback --version` reports 0.5.0 or later. If it is older, or
the store directory is not writable, fetch the same data through the forge API
(`gh pr view --json reviews,comments`, `gh api`) and keep the same
triage-before-reply discipline by hand.

Help exists at the top level (`git-feedback --help`) and on the `store`
command (`git-feedback store --help`). Git intercepts `git feedback --help`,
and `--help` on any other subcommand returns a usage error.

## Rules that always apply

- **The store never refreshes itself.** Every pass and every watch cycle starts
  with `reconcile` or `wait`. A loop that only calls `inbox` sees nothing new.
- **Ack is a delivery receipt, not a triage decision.** Ack an event only after
  your ledger records it. Never ack a batch nobody read.
- **Ack complete pages.** If `inbox` returns `has_more: true`, page with
  `--after <next_cursor>` until it is `false`, then ack (step 6). `ack` rejects
  an envelope with `has_more: true`.
- **Pipe envelopes straight into `ack`.** `--events-from -` accepts a full
  `inbox`/`wait` envelope, event records, or bare IDs.
- **Key on `status`, not the exit code.** Every subcommand prints one JSON
  envelope.
- **Self-filter is on by default.** Your own comments, reviews, and replies are
  not delivered. `--exclude-self` turns the filter *off* (delivers everything).
  Your own thread resolutions still arrive as a `revision:thread` authored by
  the reviewer.
- Pass `--skip-empty-reviews` to `inbox` and `wait`. GitHub creates an empty
  `COMMENTED` review for every inline reply; they carry no content.

## Consumer names

A consumer is a named read position, scoped per PR. Names must match
`^[A-Za-z0-9._-]{1,64}$`. A new name sees the full stored event history —
baseline `initial_observation` events plus any `revision`/`not_observed`
events recorded before the consumer existed.

- Use one consumer per role that does passes (for example one for the triage
  pass, one for the fix-verification pass).
- Use one consumer per campaign for a watcher (for example `<campaign>-watch`).
- A watcher and the pass that handles its batches use different consumers, so
  the pass's acks never hide an edge from the watcher.

## Pass shape: read only what changed

1. **Collect.** `git-feedback reconcile <PR_URL> > <LEDGER_DIR>/reconcile.json`

   | `status` | Action |
   |---|---|
   | `updated`, `unchanged` | Use `snapshot.id`. |
   | `deferred` | Not due yet. If `snapshot` is non-null and `freshness.stale` is `false`, use `snapshot.id`; otherwise reconcile again at `attempt.next_due`. |
   | `busy`, `head_changed` | Reconcile again. `head_changed` means the head moved mid-collection. |
   | `error` | Pass incomplete. Report `error.code` and `error.message`. `store_data_older` / `store_newer`: the whole store is refused. Run `git-feedback store reset` (unscoped) and reconcile again. Every consumer on every PR re-baselines, including any armed watcher. |

   A snapshot with `complete: false` or `null` is not an inventory.

2. **Read the delta.** `git-feedback inbox <PR_URL> --consumer <NAME>
   --skip-empty-reviews --limit 200 > <LEDGER_DIR>/inbox-<SNAPSHOT_ID>.json`,
   paging with `--after` while `has_more` is `true`, writing page N to
   `inbox-<SNAPSHOT_ID>-N.json`.

3. **Export and extract.** Export the snapshot to a new file; `snapshot`
   refuses to overwrite an existing one. Then pull only the objects the events
   name, using `references/recipes.md`. Do not read the whole export into
   context.

   ```bash
   git-feedback snapshot <PR_URL> --snapshot <SNAPSHOT_ID> --output <LEDGER_DIR>/snapshot-<SNAPSHOT_ID>.json
   ```

   Event `object_id` equals `threads[].id`, `reviews[].id`, or `comments[].id`
   in the export. To read one event's object without a full export — typically
   verifying a review-body completion marker — `git-feedback show <PR_URL>
   --event <ID>` returns it from the local store: no network, acknowledges
   nothing, and works after ack.

4. **Build the worklist** from the extracted objects plus the ledger's
   still-open items. On the first pass for a consumer, the inbox names
   everything, so the extraction is the full inventory.

5. **Classify events before acking.**
   - A `head_changed` target event (no author) is head movement. Record the new
     head in the ledger, because fixes need re-verification on the new tip.
     Then ack it.
   - Thread, review, and comment events are feedback. Ack them only after the
     ledger records a disposition or an explicit `untriaged` entry.

6. **Ack** the delta: `git-feedback ack <PR_URL> --consumer <NAME>
   --events-from - < <LEDGER_DIR>/inbox-<SNAPSHOT_ID>.json`. When the delta
   spanned several pages, every page but the last has `has_more: true`, so
   merge the events first: `jq -s '[.[].events[]]' <LEDGER_DIR>/inbox-<SNAPSHOT_ID>*.json
   | git-feedback ack ... --events-from -`.

7. **Close the pass** after your replies and resolutions:
   1. Reconcile again. Accept `updated`/`unchanged` only when `attempt.at` is
      later than your last mutation; a `deferred` result is never acceptable
      for pass close — reconcile again at `attempt.next_due`.
   2. Export that snapshot.
   3. Run the coverage check in `references/recipes.md` against it. Unresolved
      threads the ledger lacks go back to triage. The check is a jq diff, not a
      re-read.

Record `snapshot.id`, the snapshot `head`, and `attempt.at` in the ledger as
the fetch identity.

## Watch shape: block until feedback changes

Run one bounded `wait` as a background process that re-invokes the agent when
it exits (a background shell task, a monitor, or a scheduler-provided respawn):

```bash
git-feedback wait <PR_URL> --consumer <WATCH_CONSUMER> --timeout 30m --skip-empty-reviews --skip-head-changed
```

| `status` | Action |
|---|---|
| `timeout` | Normal. Arm the next `wait`. |
| `events` | The inventory changed. Record the batch, act (run a pass with the pass consumer), ack the batch, re-arm. |
| `head_changed` | Only with `--head <SHA>`: the pinned head moved. Re-pin or drop the pin, re-arm. |
| `error` | Report it. Do not re-arm on auth, not-found, or account-mismatch errors. |

- `--skip-head-changed` drops all `head_changed` events at delivery, so pushes
  — yours or a reviewer's — do not wake the watcher on head movement alone;
  feedback events always deliver. The events stay pending for a reader without
  the flag.
- An `events` batch means "inventory changed", not "a reviewer finished".
  `reviewer_completion` is always `unknown`; use the reviewer's own completion
  signal. `show` reads a review's stored body from the local store, so a
  completion marker in a review body can be verified without the forge API.
- Do not run `reconcile` by hand while a `wait` is armed; it consumes the
  change the watcher would report.
- Do not use the repo's `examples/watch-loop.sh` in agent loops. It acks on
  delivery, before anything handles the batch.
- A new watch consumer's first `wait` returns immediately with the baseline.
  Handle it like any batch.

## Store

`git-feedback store path` prints the store location.
`store reset [--target <URL>]` deletes it, wholly or for one PR. Reset costs
acknowledgements: the next collection re-emits everything as
`initial_observation`. Use `--state-dir <DIR>` for experiments so they do not
touch the live store or a running watcher's edge.
