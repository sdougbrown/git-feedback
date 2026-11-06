package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

type streamCursor struct {
	Version    int    `json:"v"`
	Target     string `json:"t"`
	Account    string `json:"a"`
	Generation string `json:"g"`
	After      int64  `json:"after"`
}

type StreamInput struct {
	TargetID         string
	Account          string
	Cursor           string
	Limit            int
	ExcludeAuthor    string
	SkipEmptyReviews bool
	SkipHeadChanged  bool
}

type StreamResult struct {
	SnapshotID string
	Cursor     string
	Events     []Event
	HasMore    bool
}

func encodeStreamCursor(c streamCursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Store) StreamBaseline(ctx context.Context, targetID, account string) (StreamResult, error) {
	account = forge.CanonicalAccount(account)
	var generation string
	var snapshot, high int64
	err := s.db.QueryRowContext(ctx, `SELECT t.generation, t.current_snapshot_id,
		COALESCE((SELECT MAX(e.id) FROM events e WHERE e.stream_id = t.stream_id), 0)
		FROM targets t WHERE t.target_id = ? AND t.account = ?`, targetID, account).Scan(&generation, &snapshot, &high)
	if err == sql.ErrNoRows {
		return StreamResult{}, &Error{Code: CodeUnknownStream, Message: "no observation stream; reconcile first"}
	}
	if err != nil {
		return StreamResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read stream baseline: %v", err)}
	}
	cursor, err := encodeStreamCursor(streamCursor{1, targetID, account, generation, high})
	return StreamResult{SnapshotID: formatSnapshotID(snapshot), Cursor: cursor}, err
}

func parseStreamCursor(raw, targetID string) (streamCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return streamCursor{}, &Error{Code: CodeInvalidCursor, Message: "malformed watch cursor"}
	}
	var c streamCursor
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || c.After < 0 || c.Generation == "" || c.Target != targetID || c.Account == "" || c.Account != forge.CanonicalAccount(c.Account) {
		return streamCursor{}, &Error{Code: CodeInvalidCursor, Message: "invalid watch cursor for target"}
	}
	return c, nil
}

func StreamCursorAccount(raw, targetID string) (string, error) {
	c, err := parseStreamCursor(raw, targetID)
	return c.Account, err
}

func (s *Store) verifyStreamGeneration(ctx context.Context, c streamCursor) (int64, error) {
	var streamID int64
	var generation string
	err := s.db.QueryRowContext(ctx, `SELECT stream_id, generation FROM targets WHERE target_id = ? AND account = ?`, c.Target, c.Account).Scan(&streamID, &generation)
	if err == sql.ErrNoRows || (err == nil && generation != c.Generation) {
		return 0, &Error{Code: CodeInvalidCursor, Message: "observation stream changed; establish a new baseline"}
	}
	if err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("read stream identity: %v", err)}
	}
	return streamID, nil
}

func (s *Store) ReadStream(ctx context.Context, in StreamInput) (StreamResult, error) {
	c, err := parseStreamCursor(in.Cursor, in.TargetID)
	if err != nil {
		return StreamResult{}, err
	}
	if c.Account != forge.CanonicalAccount(in.Account) {
		return StreamResult{}, &Error{Code: CodeInvalidCursor, Message: "invalid watch cursor for account"}
	}
	limit, err := deliveryLimit(in.Limit)
	if err != nil {
		return StreamResult{}, err
	}
	streamID, err := s.verifyStreamGeneration(ctx, c)
	if err != nil {
		return StreamResult{}, err
	}
	var highWater int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events WHERE stream_id = ?`, streamID).Scan(&highWater); err != nil {
		return StreamResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read high-water mark: %v", err)}
	}
	page, err := s.scanDelivery(ctx, deliveryScan{
		mode: deliveryStream, streamID: streamID, after: c.After, highWater: highWater, limit: limit,
		generation: c.Generation, excludeAuthor: in.ExcludeAuthor,
		skipEmptyReviews: in.SkipEmptyReviews, skipHeadChanged: in.SkipHeadChanged,
	})
	if err != nil {
		return StreamResult{}, err
	}
	if _, err := s.verifyStreamGeneration(ctx, c); err != nil {
		return StreamResult{}, err
	}
	if c.After > page.HighWater {
		return StreamResult{}, &Error{Code: CodeInvalidCursor, Message: "cursor is ahead of stream; establish a new baseline"}
	}
	if page.HasMore {
		c.After = page.Events[len(page.Events)-1].Seq
	} else {
		c.After = page.HighWater
	}
	next, err := encodeStreamCursor(c)
	return StreamResult{Cursor: next, Events: page.Events, HasMore: page.HasMore}, err
}
