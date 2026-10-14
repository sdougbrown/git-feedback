package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// ShowInput selects one event's object content from one stream.
type ShowInput struct {
	TargetID string
	Account  string
	EventID  string
}

// ShowResult reports one event and the object content as recorded in the
// event's snapshot.
type ShowResult struct {
	Event Event
	// Object is the stored object content — *forge.Thread (with its
	// comments), *forge.Review, or *forge.Comment. It is nil for synthetic
	// target events and when the object is absent from the event's snapshot
	// body (always so for not_observed); a reappearance revision event's
	// snapshot contains the object, so it shows the reapplied content.
	Object any
}

// Show returns one event of one stream and the object content recorded in
// the event's snapshot. It is a read-only local lookup: no authentication,
// no network access, and no acknowledgement. Events stay retrievable after
// acknowledgement because the store retains its full history.
func (s *Store) Show(ctx context.Context, in ShowInput) (ShowResult, error) {
	seq, ok := parseEventID(in.EventID)
	if !ok {
		return ShowResult{}, &Error{Code: CodeUnknownEvent, Message: fmt.Sprintf("unknown event %q for this stream", in.EventID)}
	}
	streamID, err := s.resolveStream(ctx, in.TargetID, forge.CanonicalAccount(in.Account))
	if err != nil {
		return ShowResult{}, err
	}
	var ev Event
	var observedAt string
	var snapSeq int64
	var headBefore, headAfter sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT kind, object_kind, object_id, revision, url, snapshot_id, observed_at, head_before, head_after, author
		 FROM events WHERE id = ? AND stream_id = ?`, seq, streamID).
		Scan(&ev.Kind, &ev.ObjectKind, &ev.ObjectID, &ev.Revision, &ev.URL, &snapSeq,
			&observedAt, &headBefore, &headAfter, &ev.Author)
	if err == sql.ErrNoRows {
		return ShowResult{}, &Error{Code: CodeUnknownEvent, Message: fmt.Sprintf("unknown event %q for this stream", in.EventID)}
	}
	if err != nil {
		return ShowResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read event: %v", err)}
	}
	ev.Seq, ev.ID = seq, formatEventID(seq)
	ev.SnapshotID = formatSnapshotID(snapSeq)
	if ev.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt); err != nil {
		return ShowResult{}, &Error{Code: CodeStoreCorrupt, Message: "unparseable observed_at in events"}
	}
	ev.HeadBefore, ev.HeadAfter = headBefore.String, headAfter.String

	obj, err := s.snapshotObject(ctx, snapSeq, forge.Kind(ev.ObjectKind), ev.ObjectID)
	if err != nil {
		return ShowResult{}, err
	}
	return ShowResult{Event: ev, Object: obj}, nil
}

// snapshotObject decodes one snapshot body and returns the object of the
// given kind and ID, or nil when it is absent (or the kind is the synthetic
// target, which has no feedback content).
func (s *Store) snapshotObject(ctx context.Context, snapSeq int64, kind forge.Kind, objectID string) (any, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM snapshots WHERE id = ?`, snapSeq).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("event snapshot s%d is missing", snapSeq)}
	}
	if err != nil {
		return nil, &Error{Code: CodeStore, Message: fmt.Sprintf("read snapshot body: %v", err)}
	}
	var snap forge.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		return nil, &Error{Code: CodeStoreCorrupt, Message: "unparseable snapshot body"}
	}
	switch kind {
	case forge.KindThread:
		for _, t := range snap.Threads {
			if t.ID == objectID {
				return &t, nil
			}
		}
	case forge.KindReview:
		for _, r := range snap.Reviews {
			if r.ID == objectID {
				return &r, nil
			}
		}
	case forge.KindComment:
		for _, c := range snap.Comments {
			if c.ID == objectID {
				return &c, nil
			}
		}
	}
	return nil, nil
}
