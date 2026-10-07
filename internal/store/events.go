package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// Event kinds as pinned by the plan.
const (
	KindInitialObservation = "initial_observation"
	KindRevision           = "revision"
	KindNotObserved        = "not_observed"
	KindHeadChanged        = "head_changed"
)

// objectKindTarget is the synthetic target object kind that tracks the
// collected head. It is metadata, not a finding.
const objectKindTarget = string(forge.KindTarget)

// Event is one durable occurrence in a stream's history.
type Event struct {
	ID         string // "e<seq>"
	Seq        int64
	Kind       string
	ObjectKind string
	ObjectID   string
	Revision   int64
	URL        string
	SnapshotID string // "s<seq>"
	ObservedAt time.Time
	// HeadBefore and HeadAfter are set only on head events.
	HeadBefore string
	HeadAfter  string
	// Author is the object's authoring login (the stream's own account is
	// filtered from delivery by default). Empty for synthetic target events
	// and legacy rows, which are always deliverable.
	Author string
}

// eventInsert carries the fields of one event row to insert.
type eventInsert struct {
	kind       string
	objectKind string
	objectID   string
	revision   int64
	url        string
	snapshotID int64
	observedAt time.Time
	headBefore string // empty means NULL
	headAfter  string // empty means NULL
	author     string
}

func insertEvent(ctx context.Context, tx *sql.Tx, streamID int64, e eventInsert) error {
	var before, after any
	if e.headBefore != "" {
		before = e.headBefore
	}
	if e.headAfter != "" {
		after = e.headAfter
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (stream_id, kind, object_kind, object_id, revision, url, snapshot_id, observed_at, head_before, head_after, author)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		streamID, e.kind, e.objectKind, e.objectID, e.revision, e.url, e.snapshotID,
		e.observedAt.UTC().Format(time.RFC3339Nano), before, after, e.author)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("insert event: %v", err)}
	}
	return nil
}

func formatSnapshotID(seq int64) string { return fmt.Sprintf("s%d", seq) }

func formatEventID(seq int64) string { return fmt.Sprintf("e%d", seq) }

// parseEventID parses an "e<seq>" event ID.
func parseEventID(id string) (int64, bool) {
	if len(id) < 2 || id[0] != 'e' {
		return 0, false
	}
	var seq int64
	if _, err := fmt.Sscanf(id[1:], "%d", &seq); err != nil {
		return 0, false
	}
	// Reject trailing garbage such as "e1x".
	return seq, fmt.Sprintf("e%d", seq) == id
}

// parseSnapshotID parses an "s<seq>" snapshot ID.
func parseSnapshotID(id string) (int64, bool) {
	if len(id) < 2 || id[0] != 's' {
		return 0, false
	}
	var seq int64
	if _, err := fmt.Sscanf(id[1:], "%d", &seq); err != nil {
		return 0, false
	}
	return seq, fmt.Sprintf("s%d", seq) == id
}
