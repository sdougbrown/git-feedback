# git-feedback jq recipes

Variables: `SNAP` is an exported snapshot file, `INBOX` an `inbox` or `wait` envelope (for a multi-page delta, merge that snapshot's pages first: `jq -s '{events: [.[].events[]]}' inbox-<SNAPSHOT_ID>*.json > inbox-merged.json`), `LEDGER_IDS` a file of handled object IDs, one per line.

## Objects named by a batch

Extracts only the threads, reviews, and comments the events reference. Target events (`object_kind: "target"`) carry no snapshot object.

```bash
jq --slurpfile ev "$INBOX" '
  ($ev[0].events
    | map(select(.object_kind != "target"))
    | group_by(.object_kind)
    | map({(.[0].object_kind): map(.object_id)})
    | add // {}) as $ids
  | {head,
     threads:  [.threads[]  | select(.id as $i | ($ids.thread  // []) | index($i))],
     reviews:  [.reviews[]  | select(.id as $i | ($ids.review  // []) | index($i))],
     comments: [.comments[] | select(.id as $i | ($ids.comment // []) | index($i))]}
' "$SNAP" > delta.json
```

For triage, trim threads to what a reply needs:

```bash
jq '.threads[] | {id, root_comment_database_id, resolution_state, is_outdated, path, line,
                  comments: [.comments[] | {database_id, author, body}]}' delta.json
```

`not_observed` events name objects that disappeared, so they have no snapshot entry. Record them in the ledger from the event alone.

## Batch summary without bodies

```bash
jq -r '.events[] | [.id, .kind, .object_kind, .object_id, (.author // "-")] | @tsv' "$INBOX"
```

## Coverage check at pass close

Find unresolved threads the ledger has not handled. Empty output means coverage is complete. Both checks skip objects authored by the stream's own account, because the inbox never delivers them.

```bash
jq -r '.account as $me | .threads[] | select(.author != $me and .resolution_state == "unresolved") | .id' "$SNAP" \
  | sort | comm -23 - <(sort -u "$LEDGER_IDS")
```

Find reviews with a decision or a non-empty body that the ledger has not handled:

```bash
jq -r '.account as $me | .reviews[] | select(.author != $me) | select(.state != "COMMENTED" or (.body | test("\\S"))) | .id' "$SNAP" \
  | sort | comm -23 - <(sort -u "$LEDGER_IDS")
```

## Bot-authored threads

```bash
jq '.threads[] | select(.author | endswith("[bot]"))' "$SNAP"
```

Bot replies inside human threads: `select(any(.comments[]; .author | endswith("[bot]")))`.
