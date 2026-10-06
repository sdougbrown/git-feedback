package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
)

// AcquireOutcome classifies the result of the atomic due/gate/lease check.
type AcquireOutcome int

const (
	// AcquireDone means the lease is ours and collection may proceed.
	AcquireDone AcquireOutcome = iota
	// AcquireDeferredDue means the stream's next_due is in the future.
	AcquireDeferredDue
	// AcquireDeferredGated means a rate gate blocks collection.
	AcquireDeferredGated
	// AcquireBusy means another caller holds the required lease.
	AcquireBusy
)

// AcquireResult reports the outcome of the atomic acquisition check.
type AcquireResult struct {
	Outcome      AcquireOutcome
	BlockedUntil time.Time // next_due or gate until, when deferred
}

// NextDue returns the stream's persisted next collection time.
func (s *Store) NextDue(ctx context.Context, streamID int64) (time.Time, bool, error) {
	var due sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT next_due FROM schedule WHERE stream_id = ?`, streamID).Scan(&due)
	if err == sql.ErrNoRows || (err == nil && !due.Valid) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("read next_due: %v", err)}
	}
	t, err := time.Parse(time.RFC3339Nano, due.String)
	if err != nil {
		return time.Time{}, false, &Error{Code: CodeStoreCorrupt, Message: "unparseable next_due in schedule"}
	}
	return t, true, nil
}

// SetNextDue persists the stream's next collection time.
func (s *Store) SetNextDue(ctx context.Context, streamID int64, due time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("begin set next_due: %v", err)}
	}
	defer tx.Rollback()
	if err := setNextDueTx(ctx, tx, streamID, due); err != nil {
		return err
	}
	return tx.Commit()
}

// SetNextDueTx upserts the schedule row inside the caller's transaction.
func (s *Store) SetNextDueTx(ctx context.Context, tx *sql.Tx, streamID int64, due time.Time) error {
	return setNextDueTx(ctx, tx, streamID, due)
}

func setNextDueTx(ctx context.Context, tx *sql.Tx, streamID int64, due time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO schedule (stream_id, next_due) VALUES (?, ?)
		 ON CONFLICT(stream_id) DO UPDATE SET next_due = excluded.next_due`,
		streamID, due.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("set next_due: %v", err)}
	}
	return nil
}

// AcquireCollectionLease runs the pinned atomic acquisition check in one
// write transaction: recheck the stream's next_due, consult the account's
// resource gates, then acquire the account lease (which excludes live
// bootstrap admission). Collection happens outside transactions, after this
// returns AcquireDone.
func (s *Store) AcquireCollectionLease(ctx context.Context, host, account, token string, ttl time.Duration, streamID int64, g *Gate) (AcquireResult, error) {
	account = forge.CanonicalAccount(account)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AcquireResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("begin acquire: %v", err)}
	}
	defer tx.Rollback()
	now := s.Clock.Now().UTC()

	if streamID > 0 {
		var due sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT next_due FROM schedule WHERE stream_id = ?`, streamID).Scan(&due)
		switch {
		case err == nil && due.Valid:
			t, perr := time.Parse(time.RFC3339Nano, due.String)
			if perr != nil {
				return AcquireResult{}, &Error{Code: CodeStoreCorrupt, Message: "unparseable next_due in schedule"}
			}
			if t.After(now) {
				return AcquireResult{Outcome: AcquireDeferredDue, BlockedUntil: t}, nil
			}
		case err == sql.ErrNoRows:
			// Never collected; due immediately.
		case err != nil:
			return AcquireResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read next_due: %v", err)}
		}
	}

	if g != nil {
		for _, resource := range []string{github.ResourceREST, github.ResourceGraphQL, github.ResourceSecondary} {
			if err := g.check(ctx, tx, resource, now); err != nil {
				if rl, ok := err.(*forge.ErrRateLimited); ok {
					return AcquireResult{Outcome: AcquireDeferredGated, BlockedUntil: rl.Until}, nil
				}
				return AcquireResult{}, err
			}
		}
	}

	if err := acquireLeaseTx(ctx, tx, host, account, token, ttl, now); err != nil {
		return AcquireResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AcquireResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit acquire: %v", err)}
	}
	return AcquireResult{Outcome: AcquireDone}, nil
}
