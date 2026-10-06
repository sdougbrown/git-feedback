package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// PacingSpacing is the minimum spacing between consecutive requests within
// one (host, account) scope, as pinned by the plan.
const PacingSpacing = time.Second

// acquireLeaseTx grants (host, account) to token unless a live foreign lease
// or a live bootstrap lease (for account scopes) blocks it. It runs inside
// the caller's write transaction.
func acquireLeaseTx(ctx context.Context, tx *sql.Tx, host, account, token string, ttl time.Duration, now time.Time) error {
	if account != "" {
		var token sql.NullString
		var expiry sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT token, expiry_ms FROM leases WHERE host = ? AND account = ''`, host).Scan(&token, &expiry)
		if err != nil && err != sql.ErrNoRows {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("read lease: %v", err)}
		}
		if token.Valid && token.String != "" && expiry.Valid && expiry.Int64 > now.UnixMilli() {
			return &Error{Code: CodeLeaseBusy, Message: fmt.Sprintf("bootstrap admission for %s is live", host)}
		}
	}
	row := tx.QueryRowContext(ctx,
		`SELECT token, expiry_ms FROM leases WHERE host = ? AND account = ?`, host, account)
	var held sql.NullString
	var expiry sql.NullInt64
	if err := row.Scan(&held, &expiry); err != nil && err != sql.ErrNoRows {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("read lease: %v", err)}
	}
	live := held.Valid && held.String != "" && expiry.Valid && expiry.Int64 > now.UnixMilli()
	switch {
	case live && held.String == token:
		// Re-acquisition by the current holder refreshes the expiry.
	case live:
		return &Error{Code: CodeLeaseBusy, Message: fmt.Sprintf("lease for %s/%s is live", host, account)}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO leases (host, account, token, expiry_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT(host, account) DO UPDATE SET token = excluded.token, expiry_ms = excluded.expiry_ms`,
		host, account, token, now.Add(ttl).UTC().UnixMilli()); err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("acquire lease: %v", err)}
	}
	return nil
}

// holder is one lease row's token and expiry.
type holder struct {
	token  sql.NullString
	expiry sql.NullInt64
}

// live reports whether the holder's token is non-empty and unexpired.
func (h holder) live(now time.Time) bool {
	return h.token.Valid && h.token.String != "" && h.expiry.Valid && h.expiry.Int64 > now.UnixMilli()
}

// AcquireAccountLease takes the collection lease for one account scope. It
// fails busy when a live foreign lease for the scope, or a live bootstrap
// lease on the host, exists.
func (s *Store) AcquireAccountLease(ctx context.Context, host, account, token string, ttl time.Duration, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("begin acquire lease: %v", err)}
	}
	defer tx.Rollback()
	if err := acquireLeaseTx(ctx, tx, host, forge.CanonicalAccount(account), token, ttl, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("commit acquire lease: %v", err)}
	}
	return nil
}

// AcquireBootstrapLease takes the host-wide empty-account admission lease.
// Its acquisition atomically excludes all live account leases on the host:
// identity verification never overlaps collection under the same unknown
// account.
func (s *Store) AcquireBootstrapLease(ctx context.Context, host, token string, ttl time.Duration, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("begin acquire lease: %v", err)}
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT token, expiry_ms FROM leases WHERE host = ? AND account != ''`, host)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("read leases: %v", err)}
	}
	for rows.Next() {
		var h holder
		if err := rows.Scan(&h.token, &h.expiry); err != nil {
			rows.Close()
			return &Error{Code: CodeStore, Message: fmt.Sprintf("read leases: %v", err)}
		}
		if h.live(now) {
			rows.Close()
			return &Error{Code: CodeLeaseBusy, Message: fmt.Sprintf("an account collection lease on %s is live", host)}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return &Error{Code: CodeStore, Message: fmt.Sprintf("read leases: %v", err)}
	}
	rows.Close()
	if err := acquireLeaseTx(ctx, tx, host, "", token, ttl, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("commit acquire lease: %v", err)}
	}
	return nil
}

// ReleaseLease clears the scope's token and expiry only when the token
// matches, so context cancellation releases only the caller's ownership.
// Pacing metadata (pace_ms) is preserved.
func (s *Store) ReleaseLease(ctx context.Context, host, account, token string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE leases SET token = NULL, expiry_ms = NULL WHERE host = ? AND account = ? AND token = ?`,
		host, forge.CanonicalAccount(account), token)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("release lease: %v", err)}
	}
	return nil
}

// RefreshLease extends the scope's lease expiry only when the token matches.
func (s *Store) RefreshLease(ctx context.Context, host, account, token string, ttl time.Duration, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE leases SET expiry_ms = ? WHERE host = ? AND account = ? AND token = ?`,
		now.Add(ttl).UTC().UnixMilli(), host, forge.CanonicalAccount(account), token)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("refresh lease: %v", err)}
	}
	return nil
}

// RecordPace records the scope's last-request time.
func (s *Store) RecordPace(ctx context.Context, host, account string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO leases (host, account, pace_ms) VALUES (?, ?, ?)
		 ON CONFLICT(host, account) DO UPDATE SET pace_ms = excluded.pace_ms`,
		host, forge.CanonicalAccount(account), at.UTC().UnixMilli())
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("record pace: %v", err)}
	}
	return nil
}

// TransferPace writes the verification timestamp to the verified account's
// pacing row before the bootstrap lease is released, so the account's next
// request still honors the spacing the verification request spent.
func (s *Store) TransferPace(ctx context.Context, host, fromAccount, toAccount string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO leases (host, account, pace_ms) VALUES (?, ?, ?)
		 ON CONFLICT(host, account) DO UPDATE SET
		   pace_ms = MAX(COALESCE(pace_ms, 0), excluded.pace_ms)`,
		host, forge.CanonicalAccount(toAccount), at.UTC().UnixMilli())
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("transfer pace: %v", err)}
	}
	return nil
}

// Pacer is the store-backed RequestPacer: durable per-scope pacing with a
// one-second minimum spacing that survives lease release and restart.
type Pacer struct {
	db      *sql.DB
	host    string
	account string
	Clock   clock.Clock
}

// NewPacer returns the pacing service bound to one (host, account) scope.
func (s *Store) NewPacer(host, account string) *Pacer {
	return &Pacer{db: s.db, host: host, account: forge.CanonicalAccount(account), Clock: s.Clock}
}

// claim reserves the next request slot: it returns the time the request may
// proceed and durably records it as the scope's last-request time.
func (p *Pacer) claim(ctx context.Context) (time.Time, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, &Error{Code: CodeStore, Message: fmt.Sprintf("begin pace: %v", err)}
	}
	defer tx.Rollback()
	var pace sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT pace_ms FROM leases WHERE host = ? AND account = ?`, p.host, p.account).Scan(&pace)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read pace: %v", err)}
	}
	now := p.Clock.Now().UTC()
	next := now
	if pace.Valid {
		if cand := time.UnixMilli(pace.Int64).UTC().Add(PacingSpacing); cand.After(next) {
			next = cand
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO leases (host, account, pace_ms) VALUES (?, ?, ?)
		 ON CONFLICT(host, account) DO UPDATE SET pace_ms = excluded.pace_ms`,
		p.host, p.account, next.UnixMilli()); err != nil {
		return time.Time{}, &Error{Code: CodeStore, Message: fmt.Sprintf("record pace: %v", err)}
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, &Error{Code: CodeStore, Message: fmt.Sprintf("commit pace: %v", err)}
	}
	return next, nil
}

// Wait implements github.RequestPacer. It blocks until the scope's next
// allowed request time using the injected clock.
func (p *Pacer) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	next, err := p.claim(ctx)
	if err != nil {
		return err
	}
	if d := next.Sub(p.Clock.Now()); d > 0 {
		p.Clock.Sleep(d)
	}
	return ctx.Err()
}
