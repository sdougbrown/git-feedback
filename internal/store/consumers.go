package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// consumerNameRe is the pinned consumer-name pattern.
var consumerNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Inbox limits.
const (
	DefaultInboxLimit = 50
	MinInboxLimit     = 1
	MaxInboxLimit     = 200
)

// InboxInput selects one pending-event page for one stream and consumer.
type InboxInput struct {
	TargetID string
	Account  string
	Consumer string
	// Cursor continues a previous page sequence; empty starts a new sequence
	// whose high-water mark is the stream's current maximum event seq.
	Cursor string
	Limit  int
	// ExcludeAuthor, when non-empty, filters out events whose author equals
	// this canonical login. Author-less (legacy) events are always delivered.
	// Empty means no filtering.
	ExcludeAuthor string
}

// InboxResult is one bounded page of pending events.
type InboxResult struct {
	Events     []Event
	HighWater  int64
	HasMore    bool
	NextCursor string // empty when no further page exists
}

// cursor is the JSON payload of a page-sequence cursor.
type cursor struct {
	TargetID  string `json:"t"`
	Account   string `json:"a"`
	Consumer  string `json:"c"`
	HighWater int64  `json:"hw"`
	After     int64  `json:"after"`
}

func encodeCursor(c cursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", &Error{Code: CodeStore, Message: fmt.Sprintf("encode cursor: %v", err)}
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(s string) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, &Error{Code: CodeInvalidCursor, Message: "malformed cursor"}
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, &Error{Code: CodeInvalidCursor, Message: "malformed cursor"}
	}
	return c, nil
}

// validateConsumer rejects names outside the pinned pattern.
func validateConsumer(consumer string) error {
	if !consumerNameRe.MatchString(consumer) {
		return &Error{Code: CodeInvalidConsumer, Message: fmt.Sprintf("invalid consumer name %q", consumer)}
	}
	return nil
}

// resolveStream returns the stream_id for (target, account), or unknown_stream.
func (s *Store) resolveStream(ctx context.Context, targetID, account string) (int64, error) {
	var streamID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT stream_id FROM targets WHERE target_id = ? AND account = ?`,
		targetID, forge.CanonicalAccount(account)).Scan(&streamID)
	if err == sql.ErrNoRows {
		return 0, &Error{Code: CodeUnknownStream, Message: fmt.Sprintf("no stream for %s under account %q", targetID, account)}
	}
	if err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("resolve stream: %v", err)}
	}
	return streamID, nil
}

// Inbox returns pending events for (stream, consumer): the stream's events
// minus that consumer's acknowledgements, ordered ascending, bounded by the
// page sequence's fixed high-water mark. Reading acknowledges nothing.
func (s *Store) Inbox(ctx context.Context, in InboxInput) (InboxResult, error) {
	if err := validateConsumer(in.Consumer); err != nil {
		return InboxResult{}, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultInboxLimit
	}
	if limit < MinInboxLimit || limit > MaxInboxLimit {
		return InboxResult{}, &Error{Code: CodeInvalidLimit, Message: fmt.Sprintf("limit %d outside %d–%d", limit, MinInboxLimit, MaxInboxLimit)}
	}
	account := forge.CanonicalAccount(in.Account)
	streamID, err := s.resolveStream(ctx, in.TargetID, account)
	if err != nil {
		return InboxResult{}, err
	}

	var highWater, after int64
	if in.Cursor == "" {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) FROM events WHERE stream_id = ?`, streamID).Scan(&highWater); err != nil {
			return InboxResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read high-water mark: %v", err)}
		}
	} else {
		c, err := decodeCursor(in.Cursor)
		if err != nil {
			return InboxResult{}, err
		}
		if c.TargetID != in.TargetID || c.Account != account || c.Consumer != in.Consumer {
			return InboxResult{}, &Error{Code: CodeInvalidCursor, Message: "cursor belongs to a different stream or consumer"}
		}
		var maxSeq int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) FROM events WHERE stream_id = ?`, streamID).Scan(&maxSeq); err != nil {
			return InboxResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read high-water mark: %v", err)}
		}
		if c.After < 0 || c.After > c.HighWater || c.HighWater > maxSeq {
			return InboxResult{}, &Error{Code: CodeInvalidCursor, Message: "cursor sequence is outside the stream's history"}
		}
		highWater, after = c.HighWater, c.After
	}

	where := `WHERE e.stream_id = ? AND e.id > ? AND e.id <= ? AND a.event_id IS NULL`
	args := []any{in.Consumer, streamID, after, highWater}
	excludeAuthor := forge.CanonicalAccount(in.ExcludeAuthor)
	if excludeAuthor != "" {
		// Author-less (legacy) events are always delivered; only events whose
		// author matches the excluded canonical login are filtered out.
		where += ` AND (e.author = '' OR e.author != ?)`
		args = append(args, excludeAuthor)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.kind, e.object_kind, e.object_id, e.revision, e.url, e.snapshot_id, e.observed_at, e.head_before, e.head_after, e.author
		FROM events e
		LEFT JOIN acks a ON a.event_id = e.id AND a.consumer = ? AND a.stream_id = e.stream_id
		`+where+`
		ORDER BY e.id ASC
		LIMIT ?`, args...)
	if err != nil {
		return InboxResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read inbox: %v", err)}
	}
	defer rows.Close()

	events := make([]Event, 0, limit)
	for rows.Next() {
		var ev Event
		var observedAt string
		var snapSeq int64
		var headBefore, headAfter sql.NullString
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.ObjectKind, &ev.ObjectID, &ev.Revision,
			&ev.URL, &snapSeq, &observedAt, &headBefore, &headAfter, &ev.Author); err != nil {
			return InboxResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read inbox: %v", err)}
		}
		ev.ID = formatEventID(ev.Seq)
		ev.SnapshotID = formatSnapshotID(snapSeq)
		if ev.ObservedAt, err = time.Parse(time.RFC3339Nano, observedAt); err != nil {
			return InboxResult{}, &Error{Code: CodeStoreCorrupt, Message: "unparseable observed_at in events"}
		}
		ev.HeadBefore, ev.HeadAfter = headBefore.String, headAfter.String
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return InboxResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read inbox: %v", err)}
	}

	result := InboxResult{Events: events, HighWater: highWater}
	if len(events) > limit {
		result.Events = events[:limit]
		result.HasMore = true
	}
	if result.HasMore {
		result.NextCursor, err = encodeCursor(cursor{
			TargetID: in.TargetID, Account: account, Consumer: in.Consumer,
			HighWater: highWater, After: result.Events[len(result.Events)-1].Seq,
		})
		if err != nil {
			return InboxResult{}, err
		}
	}
	return result, nil
}

// AckInput acknowledges exactly the supplied event IDs for one consumer and
// stream.
type AckInput struct {
	TargetID string
	Account  string
	Consumer string
	EventIDs []string
}

// AckResult reports how many acknowledgements were newly recorded.
type AckResult struct {
	Acknowledged int
}

// Ack acknowledges the supplied event IDs transactionally and idempotently.
// Unknown or out-of-scope IDs reject the whole request.
func (s *Store) Ack(ctx context.Context, in AckInput) (AckResult, error) {
	if err := validateConsumer(in.Consumer); err != nil {
		return AckResult{}, err
	}
	account := forge.CanonicalAccount(in.Account)
	streamID, err := s.resolveStream(ctx, in.TargetID, account)
	if err != nil {
		return AckResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AckResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("begin ack: %v", err)}
	}
	defer tx.Rollback()
	acknowledged := 0
	for _, id := range in.EventIDs {
		seq, ok := parseEventID(id)
		if !ok {
			return AckResult{}, &Error{Code: CodeUnknownEvent, Message: fmt.Sprintf("unknown event %q for this stream", id)}
		}
		var eventStream int64
		err := tx.QueryRowContext(ctx,
			`SELECT stream_id FROM events WHERE id = ?`, seq).Scan(&eventStream)
		if err == sql.ErrNoRows || (err == nil && eventStream != streamID) {
			return AckResult{}, &Error{Code: CodeUnknownEvent, Message: fmt.Sprintf("unknown event %q for this stream", id)}
		}
		if err != nil {
			return AckResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read event: %v", err)}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO acks (consumer, stream_id, event_id) VALUES (?, ?, ?)`,
			in.Consumer, streamID, seq)
		if err != nil {
			return AckResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("record ack: %v", err)}
		}
		n, _ := res.RowsAffected()
		acknowledged += int(n)
	}
	if err := tx.Commit(); err != nil {
		return AckResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit ack: %v", err)}
	}
	return AckResult{Acknowledged: acknowledged}, nil
}
