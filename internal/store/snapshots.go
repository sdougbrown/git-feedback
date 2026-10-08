package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// PublishInput describes one complete collected inventory to publish.
type PublishInput struct {
	Target   forge.Target
	Account  string
	Snapshot *forge.Snapshot
	// Fence, when non-nil, must still be valid (token matches, unexpired per
	// the store's clock) when the transaction commits.
	Fence *Fence
	// Finalize, when non-nil, runs inside the publish transaction after the
	// pre-commit fence recheck and immediately before commit. The tracker
	// uses it to update cadence, record the attempt, and release lease
	// ownership atomically with the publication.
	Finalize func(ctx context.Context, tx *sql.Tx) error
}

// PublishResult reports the outcome of one publication.
type PublishResult struct {
	// SnapshotID is "s<seq>" of the snapshot now current for the stream —
	// newly written on a change, the pre-existing snapshot when unchanged.
	SnapshotID string
	// Changed reports whether a new snapshot was written.
	Changed bool
}

// objectState is one object's observed state for fingerprinting, identity,
// and delivery filtering. url is the object's own URL for event delivery;
// it is empty for objects absent from the inventory.
type objectState struct {
	kind        forge.Kind
	id          string
	fingerprint string
	author      string
	url         string
}

// sha256hex hashes s and returns the lowercase hex digest.
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// collectObjects returns the feedback objects of a snapshot in deterministic
// (kind, id) order with their forge fingerprints.
func collectObjects(snap forge.Snapshot) []objectState {
	threads := append([]forge.Thread(nil), snap.Threads...)
	reviews := append([]forge.Review(nil), snap.Reviews...)
	comments := append([]forge.Comment(nil), snap.Comments...)
	sort.SliceStable(threads, func(i, j int) bool { return threads[i].ID < threads[j].ID })
	sort.SliceStable(reviews, func(i, j int) bool { return reviews[i].ID < reviews[j].ID })
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	objs := make([]objectState, 0, len(threads)+len(reviews)+len(comments))
	// The forge fingerprints already cover Author, so the author is part of
	// the identity; it is carried separately here only for delivery. The
	// author is canonicalized at this choke point so the delivery filter's
	// comparison against the canonical account is case-insensitive.
	for _, t := range threads {
		objs = append(objs, objectState{kind: forge.KindThread, id: t.ID, fingerprint: forge.FingerprintThread(t), author: forge.CanonicalAccount(t.Author), url: t.URL})
	}
	for _, r := range reviews {
		objs = append(objs, objectState{kind: forge.KindReview, id: r.ID, fingerprint: forge.FingerprintReview(r), author: forge.CanonicalAccount(r.Author), url: r.URL})
	}
	for _, c := range comments {
		objs = append(objs, objectState{kind: forge.KindComment, id: c.ID, fingerprint: forge.FingerprintComment(c), author: forge.CanonicalAccount(c.Author), url: c.URL})
	}
	return objs
}

// identityTuple is one (kind, id, fingerprint) entry of the snapshot identity.
type identityTuple struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

// identityDoc is the canonical identity document of a snapshot: the collected
// head plus the sorted feedback tuples and the synthetic target whose
// fingerprint hashes the head.
type identityDoc struct {
	Head    string          `json:"head"`
	Objects []identityTuple `json:"objects"`
}

// snapshotIdentity returns the identity fingerprint of a normalized snapshot.
func snapshotIdentity(snap forge.Snapshot, targetID string) string {
	doc := identityDoc{Head: snap.Head}
	doc.Objects = append(doc.Objects, identityTuple{
		Kind: objectKindTarget, ID: targetID, Fingerprint: sha256hex(snap.Head),
	})
	for _, o := range collectObjects(snap) {
		doc.Objects = append(doc.Objects, identityTuple{Kind: string(o.kind), ID: o.id, Fingerprint: o.fingerprint})
	}
	// encoding/json emits map-free structs in field order; the tuples are
	// grouped by kind in fixed order (threads, reviews, comments), id-sorted
	// within kind, as collectObjects guarantees.
	b, err := json.Marshal(doc)
	if err != nil {
		panic("store: identity marshal failed: " + err.Error())
	}
	return sha256hex(string(b))
}

// normalizeSnapshot returns a copy of snap with arrays sorted by object ID so
// the stored body is canonical.
func normalizeSnapshot(snap forge.Snapshot) forge.Snapshot {
	out := snap
	out.Threads = append([]forge.Thread(nil), snap.Threads...)
	out.Reviews = append([]forge.Review(nil), snap.Reviews...)
	out.Comments = append([]forge.Comment(nil), snap.Comments...)
	sort.SliceStable(out.Threads, func(i, j int) bool { return out.Threads[i].ID < out.Threads[j].ID })
	sort.SliceStable(out.Reviews, func(i, j int) bool { return out.Reviews[i].ID < out.Reviews[j].ID })
	sort.SliceStable(out.Comments, func(i, j int) bool { return out.Comments[i].ID < out.Comments[j].ID })
	return out
}

// objectRow is one objects-table row.
type objectRow struct {
	counter  int64
	present  bool
	lastFp   string
	kind     string
	provider string
}

// resolveOrCreateStream returns the stream_id and current_snapshot_id for
// (target, account), creating the target row on first sight.
func resolveOrCreateStream(ctx context.Context, tx *sql.Tx, in PublishInput, account string) (int64, int64, error) {
	var streamID, snapID int64
	err := tx.QueryRowContext(ctx,
		`SELECT stream_id, COALESCE(current_snapshot_id, 0) FROM targets WHERE target_id = ? AND account = ?`,
		in.Target.ID, account).Scan(&streamID, &snapID)
	switch {
	case err == nil:
		return streamID, snapID, nil
	case err != sql.ErrNoRows:
		return 0, 0, &Error{Code: CodeStore, Message: fmt.Sprintf("resolve stream: %v", err)}
	}
	tj, err := json.Marshal(in.Target)
	if err != nil {
		return 0, 0, &Error{Code: CodeStore, Message: fmt.Sprintf("marshal target: %v", err)}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO targets (target_id, account, host, target_json) VALUES (?, ?, ?, ?)`,
		in.Target.ID, account, forge.CanonicalHost(in.Target.Host), string(tj))
	if err != nil {
		return 0, 0, &Error{Code: CodeStore, Message: fmt.Sprintf("create stream: %v", err)}
	}
	streamID, err = res.LastInsertId()
	if err != nil {
		return 0, 0, &Error{Code: CodeStore, Message: fmt.Sprintf("create stream: %v", err)}
	}
	return streamID, 0, nil
}

// Publish durably records one complete inventory. It writes a new immutable
// snapshot when the head or feedback identity changed, appends only an
// observations row otherwise, emits the pinned events, and commits snapshot,
// revisions, events, and observation atomically.
func (s *Store) Publish(ctx context.Context, in PublishInput) (PublishResult, error) {
	if in.Target.ID == "" || in.Account == "" || in.Snapshot == nil {
		return PublishResult{}, &Error{Code: CodeInvalidInput, Message: "publish requires target, account, and snapshot"}
	}
	if in.Target.Host == "" {
		return PublishResult{}, &Error{Code: CodeInvalidInput, Message: "publish requires target host"}
	}
	account := forge.CanonicalAccount(in.Account)
	snap := normalizeSnapshot(*in.Snapshot)
	identityFP := snapshotIdentity(snap, in.Target.ID)
	body, err := json.Marshal(snap)
	if err != nil {
		return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("marshal snapshot body: %v", err)}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("begin publish: %v", err)}
	}
	defer tx.Rollback()
	now := s.Clock.Now().UTC()

	streamID, currentID, err := resolveOrCreateStream(ctx, tx, in, account)
	if err != nil {
		return PublishResult{}, err
	}
	if in.Fence != nil {
		if err := checkFence(ctx, tx, *in.Fence, now); err != nil {
			return PublishResult{}, err
		}
	}

	var currentFP, currentHead string
	if currentID > 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT fingerprint, head FROM snapshots WHERE id = ? AND stream_id = ?`,
			currentID, streamID).Scan(&currentFP, &currentHead); err != nil {
			return PublishResult{}, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("read current snapshot: %v", err)}
		}
	}

	// Unchanged complete collection: append an observation row referencing
	// the existing snapshot and emit nothing.
	if currentFP == identityFP {
		if err := recordObservation(ctx, tx, streamID, currentID, now); err != nil {
			return PublishResult{}, err
		}
		if err := s.finalize(ctx, tx, in, now); err != nil {
			return PublishResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit publish: %v", err)}
		}
		s.Hooks.callAfterPublishCommit()
		return PublishResult{SnapshotID: formatSnapshotID(currentID), Changed: false}, nil
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO snapshots (stream_id, head, fingerprint, body, collected_start, collected_end)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		streamID, snap.Head, identityFP, string(body),
		snap.CollectedStart.UTC().Format(time.RFC3339Nano),
		snap.CollectedEnd.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("insert snapshot: %v", err)}
	}
	snapID, err := res.LastInsertId()
	if err != nil {
		return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("insert snapshot: %v", err)}
	}

	if err := s.emitEvents(ctx, tx, streamID, in, snap, currentID > 0, currentHead, snapID, now); err != nil {
		return PublishResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE targets SET current_snapshot_id = ? WHERE stream_id = ?`, snapID, streamID); err != nil {
		return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("update stream: %v", err)}
	}
	if err := recordObservation(ctx, tx, streamID, snapID, now); err != nil {
		return PublishResult{}, err
	}
	if err := s.finalize(ctx, tx, in, now); err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublishResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit publish: %v", err)}
	}
	s.Hooks.callAfterPublishCommit()
	return PublishResult{SnapshotID: formatSnapshotID(snapID), Changed: true}, nil
}

// finalize runs the pre-commit seams: the BeforePublishCommit hook, the
// fence recheck, then the caller's Finalize (cadence update and lease
// release) immediately before commit. Running Finalize after the recheck is
// what allows it to release lease ownership: a release inside the
// transaction must not invalidate the fence the publication was validated
// against. Any error must roll back the whole publication.
func (s *Store) finalize(ctx context.Context, tx *sql.Tx, in PublishInput, now time.Time) error {
	if s.Hooks.BeforePublishCommit != nil {
		if err := s.Hooks.BeforePublishCommit(); err != nil {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("before-publish hook: %v", err)}
		}
	}
	if fence := in.Fence; fence != nil {
		if err := checkFence(ctx, tx, *fence, s.Clock.Now().UTC()); err != nil {
			return err
		}
	}
	if in.Finalize != nil {
		if err := in.Finalize(ctx, tx); err != nil {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("publish finalize: %v", err)}
		}
	}
	return nil
}

// recordObservation appends an observations row linking one stream to one
// snapshot at one moment.
func recordObservation(ctx context.Context, tx *sql.Tx, streamID, snapID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO observations (stream_id, snapshot_id, observed_at) VALUES (?, ?, ?)`,
		streamID, snapID, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("record observation: %v", err)}
	}
	return nil
}

// bumpObject advances an object's counter, records the revision occurrence,
// and updates its presence state and author.
func bumpObject(ctx context.Context, tx *sql.Tx, streamID int64, kind, id, fp, author string, present bool, snapID int64) (int64, error) {
	var counter int64
	err := tx.QueryRowContext(ctx,
		`SELECT counter FROM objects WHERE stream_id = ? AND kind = ? AND provider_id = ?`,
		streamID, kind, id).Scan(&counter)
	newCounter := counter + 1
	if err == sql.ErrNoRows {
		newCounter = 1
	} else if err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("read object: %v", err)}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO objects (stream_id, kind, provider_id, counter, present, last_fingerprint, author)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(stream_id, kind, provider_id) DO UPDATE SET
		   counter = excluded.counter, present = excluded.present, last_fingerprint = excluded.last_fingerprint, author = excluded.author`,
		streamID, kind, id, newCounter, boolToInt(present), fp, author); err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("update object: %v", err)}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO revisions (stream_id, kind, provider_id, counter, snapshot_id) VALUES (?, ?, ?, ?, ?)`,
		streamID, kind, id, newCounter, snapID); err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("record revision: %v", err)}
	}
	return newCounter, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// emitEvents writes the object and target events for a changed inventory and
// updates object state. currentExists is false on a stream's initial
// publication; currentHead is then ignored.
func (s *Store) emitEvents(ctx context.Context, tx *sql.Tx, streamID int64, in PublishInput, snap forge.Snapshot, currentExists bool, currentHead string, snapID int64, now time.Time) error {
	// Target head event. The synthetic target object has no author.
	if currentExists && currentHead != snap.Head {
		rev, err := bumpObject(ctx, tx, streamID, objectKindTarget, in.Target.ID, sha256hex(snap.Head), "", true, snapID)
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, streamID, eventInsert{
			kind: KindHeadChanged, objectKind: objectKindTarget, objectID: in.Target.ID,
			revision: rev, url: in.Target.URL, snapshotID: snapID, observedAt: now,
			headBefore: currentHead, headAfter: snap.Head, author: "",
		}); err != nil {
			return err
		}
	} else if !currentExists {
		rev, err := bumpObject(ctx, tx, streamID, objectKindTarget, in.Target.ID, sha256hex(snap.Head), "", true, snapID)
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, streamID, eventInsert{
			kind: KindInitialObservation, objectKind: objectKindTarget, objectID: in.Target.ID,
			revision: rev, url: in.Target.URL, snapshotID: snapID, observedAt: now,
			headAfter: snap.Head, author: "",
		}); err != nil {
			return err
		}
	}

	present := make(map[string]map[string]bool, 3)
	for _, k := range []string{string(forge.KindThread), string(forge.KindReview), string(forge.KindComment)} {
		present[k] = map[string]bool{}
	}
	threadByID := make(map[string]forge.Thread, len(snap.Threads))
	for _, t := range snap.Threads {
		threadByID[t.ID] = t
	}

	// Feedback objects in the new inventory, in deterministic order.
	for _, o := range collectObjects(snap) {
		var prev objectRow
		err := tx.QueryRowContext(ctx,
			`SELECT counter, present, last_fingerprint FROM objects WHERE stream_id = ? AND kind = ? AND provider_id = ?`,
			streamID, string(o.kind), o.id).Scan(&prev.counter, &prev.present, &prev.lastFp)
		switch {
		case err == sql.ErrNoRows:
			// First observation of this object in the stream.
			rev, err := bumpObject(ctx, tx, streamID, string(o.kind), o.id, o.fingerprint, o.author, true, snapID)
			if err != nil {
				return err
			}
			if err := insertEvent(ctx, tx, streamID, eventInsert{
				kind: KindInitialObservation, objectKind: string(o.kind), objectID: o.id,
				revision: rev, url: eventURL(o, in.Target.URL), snapshotID: snapID, observedAt: now,
				author: o.author,
			}); err != nil {
				return err
			}
		case err != nil:
			return &Error{Code: CodeStore, Message: fmt.Sprintf("read object: %v", err)}
		case !prev.present:
			// Reappearance after a not_observed gets a new revision.
			rev, err := bumpObject(ctx, tx, streamID, string(o.kind), o.id, o.fingerprint, o.author, true, snapID)
			if err != nil {
				return err
			}
			if err := insertEvent(ctx, tx, streamID, eventInsert{
				kind: KindRevision, objectKind: string(o.kind), objectID: o.id,
				revision: rev, url: eventURL(o, in.Target.URL), snapshotID: snapID, observedAt: now,
				author: revisionAuthor(ctx, tx, streamID, snapID, o, threadByID[o.id], forge.CanonicalAccount(in.Account)),
			}); err != nil {
				return err
			}
		case prev.lastFp != o.fingerprint:
			// Content changed; the revision counter advances.
			rev, err := bumpObject(ctx, tx, streamID, string(o.kind), o.id, o.fingerprint, o.author, true, snapID)
			if err != nil {
				return err
			}
			if err := insertEvent(ctx, tx, streamID, eventInsert{
				kind: KindRevision, objectKind: string(o.kind), objectID: o.id,
				revision: rev, url: eventURL(o, in.Target.URL), snapshotID: snapID, observedAt: now,
				author: revisionAuthor(ctx, tx, streamID, snapID, o, threadByID[o.id], forge.CanonicalAccount(in.Account)),
			}); err != nil {
				return err
			}
		}
		present[string(o.kind)][o.id] = true
	}

	// Objects absent from this complete inventory: not_observed, not a
	// resolution. Their identity and counter history is retained.
	var absent []objectState
	for _, k := range []string{string(forge.KindThread), string(forge.KindReview), string(forge.KindComment)} {
		rows, err := tx.QueryContext(ctx,
			`SELECT provider_id, last_fingerprint, author FROM objects WHERE stream_id = ? AND kind = ? AND present = 1`,
			streamID, k)
		if err != nil {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("read objects: %v", err)}
		}
		for rows.Next() {
			var id, fp, author string
			if err := rows.Scan(&id, &fp, &author); err != nil {
				rows.Close()
				return &Error{Code: CodeStore, Message: fmt.Sprintf("read objects: %v", err)}
			}
			if !present[k][id] {
				absent = append(absent, objectState{kind: forge.Kind(k), id: id, fingerprint: fp, author: author})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return &Error{Code: CodeStore, Message: fmt.Sprintf("read objects: %v", err)}
		}
		rows.Close()
	}
	sort.Slice(absent, func(i, j int) bool {
		if absent[i].kind != absent[j].kind {
			return absent[i].kind < absent[j].kind
		}
		return absent[i].id < absent[j].id
	})
	for _, o := range absent {
		// The not_observed event carries the last known author (read from the
		// objects table above); the object row keeps that author.
		rev, err := bumpObject(ctx, tx, streamID, string(o.kind), o.id, o.fingerprint, o.author, false, snapID)
		if err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, streamID, eventInsert{
			kind: KindNotObserved, objectKind: string(o.kind), objectID: o.id,
			revision: rev, url: eventURL(o, in.Target.URL), snapshotID: snapID, observedAt: now,
			author: o.author,
		}); err != nil {
			return err
		}
	}
	return nil
}

// eventURL returns the object's own URL for event delivery, falling back to
// the target URL when the object carries no URL (the REST payload lacked
// html_url, or the object is absent from the inventory).
func eventURL(o objectState, fallback string) string {
	if o.url != "" {
		return o.url
	}
	return fallback
}

// revisionAuthor attributes a revision event to the authors of what changed.
// For threads, an all-self changed-comment set — every added or changed
// comment authored by the stream's own account, no comment removed, and the
// thread root (author, body, path) unchanged, so a resolution-state-only
// change also qualifies — is attributed to the account, letting the delivery
// filter apply to self-authored replies. Everything else keeps the root
// author: other kinds, mixed or reviewer-authored changes, removals, and any
// uncertainty (missing prior snapshot, thread absent from every retained
// snapshot, corrupt body, read error). Delivering extra noise is acceptable;
// hiding reviewer activity is not.
func revisionAuthor(ctx context.Context, tx *sql.Tx, streamID, beforeSeq int64, o objectState, cur forge.Thread, account string) string {
	if o.kind != forge.KindThread {
		return o.author
	}
	prior, ok, err := findPriorThread(ctx, tx, streamID, beforeSeq, o.id)
	if err != nil || !ok {
		return o.author
	}
	if prior.Author != cur.Author || prior.Body != cur.Body || prior.Path != cur.Path {
		return o.author
	}
	authors, removed := threadCommentChanges(prior.Comments, cur.Comments)
	if removed || len(authors) == 0 {
		return o.author
	}
	for _, a := range authors {
		if forge.CanonicalAccount(a) != account {
			return o.author
		}
	}
	return account
}

// findPriorThread returns the thread with the given ID as it appeared in the
// most recent retained snapshot before beforeSeq, walking back past any
// snapshots that lack it (a reappearance after not_observed). A corrupt body
// stops the walk as an uncertainty, reported as not found.
func findPriorThread(ctx context.Context, tx *sql.Tx, streamID, beforeSeq int64, threadID string) (forge.Thread, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT body FROM snapshots WHERE stream_id = ? AND id < ? ORDER BY id DESC`,
		streamID, beforeSeq)
	if err != nil {
		return forge.Thread{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return forge.Thread{}, false, err
		}
		var snap forge.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			return forge.Thread{}, false, nil
		}
		for _, t := range snap.Threads {
			if t.ID == threadID {
				return t, true, rows.Err()
			}
		}
	}
	return forge.Thread{}, false, rows.Err()
}

// threadCommentChanges diffs a thread's comments against their prior state
// by ID, comparing the fingerprint-relevant fields (author and body). It
// returns the authors of added and changed comments and whether any prior
// comment was removed.
func threadCommentChanges(prior, cur []forge.ThreadComment) (authors []string, removed bool) {
	prev := make(map[string]forge.ThreadComment, len(prior))
	for _, c := range prior {
		prev[c.ID] = c
	}
	for _, c := range cur {
		if p, ok := prev[c.ID]; ok {
			delete(prev, c.ID)
			if p.Author == c.Author && p.Body == c.Body {
				continue
			}
		}
		authors = append(authors, c.Author)
	}
	return authors, len(prev) > 0
}

// SnapshotSummary describes a stream's current snapshot for result
// envelopes: identity, collection window, feedback object counts (the
// synthetic target object is excluded), and the last successful complete
// observation time for the snapshot.
type SnapshotSummary struct {
	ID             string
	Head           string
	CollectedStart time.Time
	CollectedEnd   time.Time
	Threads        int
	Reviews        int
	Comments       int
	ObservedAt     time.Time
}

// CurrentSnapshot returns the current snapshot summary for one stream, or
// false when the stream has no snapshot (or does not exist).
func (s *Store) CurrentSnapshot(ctx context.Context, targetID, account string) (SnapshotSummary, bool, error) {
	var sum SnapshotSummary
	var streamID, snapID int64
	var start, end, observedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT t.stream_id, s.id, s.head, s.collected_start, s.collected_end,
		       (SELECT MAX(o.observed_at) FROM observations o
		         WHERE o.stream_id = t.stream_id AND o.snapshot_id = s.id)
		FROM targets t JOIN snapshots s ON s.id = t.current_snapshot_id
		WHERE t.target_id = ? AND t.account = ?`,
		targetID, forge.CanonicalAccount(account)).Scan(
		&streamID, &snapID, &sum.Head, &start, &end, &observedAt)
	if err == sql.ErrNoRows {
		return SnapshotSummary{}, false, nil
	}
	if err != nil {
		return SnapshotSummary{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("read current snapshot: %v", err)}
	}
	stamps := []struct {
		col string
		dst *time.Time
	}{{start.String, &sum.CollectedStart}, {end.String, &sum.CollectedEnd}, {observedAt.String, &sum.ObservedAt}}
	for _, st := range stamps {
		if st.col == "" {
			continue
		}
		if *st.dst, err = time.Parse(time.RFC3339Nano, st.col); err != nil {
			return SnapshotSummary{}, false, &Error{Code: CodeStoreCorrupt, Message: "unparseable snapshot timestamps"}
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, count(*) FROM objects
		 WHERE stream_id = ? AND present = 1 AND kind IN ('thread', 'review', 'comment')
		 GROUP BY kind`, streamID)
	if err != nil {
		return SnapshotSummary{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("count objects: %v", err)}
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return SnapshotSummary{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("count objects: %v", err)}
		}
		switch forge.Kind(kind) {
		case forge.KindThread:
			sum.Threads = n
		case forge.KindReview:
			sum.Reviews = n
		case forge.KindComment:
			sum.Comments = n
		}
	}
	if err := rows.Err(); err != nil {
		return SnapshotSummary{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("count objects: %v", err)}
	}
	sum.ID = formatSnapshotID(snapID)
	return sum, true, nil
}
