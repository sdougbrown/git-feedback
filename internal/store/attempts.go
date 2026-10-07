package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// Attempt outcomes.
const (
	OutcomeOK         = "ok"
	OutcomeFailed     = "failed"
	OutcomeHeadChange = "head_changed"
)

// Attempt is one recorded collection attempt.
type Attempt struct {
	Seq          int64
	StreamID     int64 // 0 when the stream was never created
	HasStream    bool
	Outcome      string
	ErrorCode    string
	HasErrorCode bool
	Complete     bool
	ObservedHead string
	HasObserved  bool
	ExpectedHead string
	HasExpected  bool
	At           time.Time
	NextDue      time.Time
	HasNextDue   bool
}

// AttemptInput describes one attempt to record.
type AttemptInput struct {
	StreamID     int64 // 0 → NULL (stream never created)
	Outcome      string
	ErrorCode    string
	Complete     bool
	ObservedHead string
	ExpectedHead string
	NextDue      time.Time // zero → NULL
	At           time.Time // zero → the store's clock
}

// RecordAttempt appends one attempt row.
func (s *Store) RecordAttempt(ctx context.Context, in AttemptInput) (Attempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, &Error{Code: CodeStore, Message: fmt.Sprintf("begin attempt: %v", err)}
	}
	defer tx.Rollback()
	seq, err := recordAttemptTx(ctx, tx, in, s.Clock.Now().UTC())
	if err != nil {
		return Attempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Attempt{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit attempt: %v", err)}
	}
	return attemptFromSeq(s.db, seq)
}

// RecordAttemptTx appends one attempt row inside the caller's transaction.
func (s *Store) RecordAttemptTx(ctx context.Context, tx *sql.Tx, in AttemptInput, now time.Time) error {
	_, err := recordAttemptTx(ctx, tx, in, now)
	return err
}

func recordAttemptTx(ctx context.Context, q queryer, in AttemptInput, now time.Time) (int64, error) {
	if in.At.IsZero() {
		in.At = now
	}
	at := in.At.UTC().Format(time.RFC3339Nano)
	var nextDue any
	if !in.NextDue.IsZero() {
		nextDue = in.NextDue.UTC().Format(time.RFC3339Nano)
	}
	res, err := q.ExecContext(ctx,
		`INSERT INTO attempts (stream_id, outcome, error_code, complete, observed_head, expected_head, at, next_due)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullInt64(in.StreamID, in.StreamID > 0), in.Outcome,
		nullString(in.ErrorCode, in.ErrorCode != ""), boolToInt(in.Complete),
		nullString(in.ObservedHead, in.ObservedHead != ""),
		nullString(in.ExpectedHead, in.ExpectedHead != ""),
		at, nextDue)
	if err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("record attempt: %v", err)}
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, &Error{Code: CodeStore, Message: fmt.Sprintf("record attempt: %v", err)}
	}
	return seq, nil
}

// FinalizeInput describes one fenced attempt finalization: it validates the
// fence, records the attempt with its cadence, and releases lease ownership
// in one transaction.
type FinalizeInput struct {
	Fence   *Fence
	Attempt AttemptInput
}

// FinalizeAttempt runs the fenced finalization. When the fence no longer
// holds, nothing is written and a fence_lost error is returned; the caller
// remains responsible for releasing its token.
func (s *Store) FinalizeAttempt(ctx context.Context, in FinalizeInput) (Attempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, &Error{Code: CodeStore, Message: fmt.Sprintf("begin finalize: %v", err)}
	}
	defer tx.Rollback()
	now := s.Clock.Now().UTC()
	if in.Fence != nil {
		if err := checkFence(ctx, tx, *in.Fence, now); err != nil {
			return Attempt{}, err
		}
	}
	seq, err := recordAttemptTx(ctx, tx, in.Attempt, now)
	if err != nil {
		return Attempt{}, err
	}
	if !in.Attempt.NextDue.IsZero() && in.Attempt.StreamID > 0 {
		if err := setNextDueTx(ctx, tx, in.Attempt.StreamID, in.Attempt.NextDue); err != nil {
			return Attempt{}, err
		}
	}
	if in.Fence != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET token = NULL, expiry_ms = NULL WHERE host = ? AND account = ? AND token = ?`,
			in.Fence.Host, forge.CanonicalAccount(in.Fence.Account), in.Fence.Token); err != nil {
			return Attempt{}, &Error{Code: CodeStore, Message: fmt.Sprintf("release lease: %v", err)}
		}
	}
	if err := tx.Commit(); err != nil {
		return Attempt{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit finalize: %v", err)}
	}
	return attemptFromSeq(s.db, seq)
}

// LatestAttempt returns the stream's most recent attempt row.
func (s *Store) LatestAttempt(ctx context.Context, targetID, account string) (Attempt, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.stream_id, a.outcome, a.error_code, a.complete,
		        a.observed_head, a.expected_head, a.at, a.next_due
		 FROM attempts a JOIN targets t ON t.stream_id = a.stream_id
		 WHERE t.target_id = ? AND t.account = ?
		 ORDER BY a.id DESC LIMIT 1`,
		targetID, forge.CanonicalAccount(account))
	if err != nil {
		return Attempt{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("read attempt: %v", err)}
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Attempt{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("read attempt: %v", err)}
		}
		return Attempt{}, false, nil
	}
	a, err := scanAttempt(rows)
	if err != nil {
		return Attempt{}, false, err
	}
	return a, true, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanAttempt(r rowScanner) (Attempt, error) {
	var a Attempt
	var streamID sql.NullInt64
	var errorCode, observed, expected, nextDue sql.NullString
	var at string
	var complete int
	if err := r.Scan(&a.Seq, &streamID, &a.Outcome, &errorCode, &complete,
		&observed, &expected, &at, &nextDue); err != nil {
		return a, &Error{Code: CodeStore, Message: fmt.Sprintf("read attempt: %v", err)}
	}
	a.StreamID, a.HasStream = streamID.Int64, streamID.Valid
	a.ErrorCode, a.HasErrorCode = errorCode.String, errorCode.Valid
	a.Complete = complete != 0
	a.ObservedHead, a.HasObserved = observed.String, observed.Valid
	a.ExpectedHead, a.HasExpected = expected.String, expected.Valid
	var perr error
	if a.At, perr = time.Parse(time.RFC3339Nano, at); perr != nil {
		return a, &Error{Code: CodeStoreCorrupt, Message: "unparseable at in attempts"}
	}
	if nextDue.Valid {
		if a.NextDue, perr = time.Parse(time.RFC3339Nano, nextDue.String); perr != nil {
			return a, &Error{Code: CodeStoreCorrupt, Message: "unparseable next_due in attempts"}
		}
		a.HasNextDue = true
	}
	return a, nil
}

// attemptFromSeq reads back one attempt row by sequence.
func attemptFromSeq(q queryer, seq int64) (Attempt, error) {
	row := q.QueryRowContext(context.Background(),
		`SELECT id, stream_id, outcome, error_code, complete, observed_head, expected_head, at, next_due
		 FROM attempts WHERE id = ?`, seq)
	a, err := scanAttempt(row)
	if err != nil {
		return Attempt{}, err
	}
	return a, nil
}

// nullInt64 returns a nullable value for stream_id-style columns.
func nullInt64(zero int64, use bool) any {
	if !use {
		return nil
	}
	return zero
}

// nullString returns a nullable string value.
func nullString(s string, use bool) any {
	if !use {
		return nil
	}
	return s
}
