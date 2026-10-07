// Package store makes observation durable: an SQLite-backed store of
// immutable per-stream snapshots, append-only revision-aware events, and
// per-consumer inboxes and acknowledgements, plus atomic no-replace snapshot
// export.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// Error codes reported by the store. The CLI maps them to envelope codes and
// exit codes; they are stable contract.
const (
	CodeStore              = "store_error"
	CodeStoreCorrupt       = "store_corrupt"
	CodeStoreNewer         = "store_newer"
	CodeUnknownStream      = "unknown_stream"
	CodeUnknownEvent       = "unknown_event"
	CodeUnknownSnapshot    = "unknown_snapshot"
	CodeInvalidCursor      = "invalid_cursor"
	CodeInvalidConsumer    = "invalid_consumer"
	CodeInvalidLimit       = "invalid_limit"
	CodeInvalidInput       = "invalid_input"
	CodeFenceLost          = "fence_lost"
	CodeLeaseBusy          = "lease_busy"
	CodeDestinationExists  = "destination_exists"
	CodeExportStateDir     = "export_state_dir"
	CodeExportUnsupported  = "export_unsupported"
	CodeInvalidDestination = "invalid_destination"
)

// Error is a typed store error carrying a stable code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Fence identifies the admission lease a caller must still own at commit
// time. Stage 4's tracker always supplies one; unit tests may omit it.
type Fence struct {
	Host    string
	Account string
	Token   string
}

// Options configures Open.
type Options struct {
	// Clock is consulted for fence expiry and observed_at stamps. Nil uses
	// clock.Real.
	Clock clock.Clock
	// BusyTimeout bounds SQLite lock waits (zero uses the pinned 5000ms).
	// Callers with a command deadline clamp it to the remaining time.
	BusyTimeout time.Duration
}

// Store is the durable observation store rooted at one state directory.
type Store struct {
	db *sql.DB

	// Clock is consulted by Publish for fence expiry checks; tests inject a
	// fake. Replaceable before first use, read-only afterwards.
	Clock clock.Clock

	// Hooks are nil-safe publication hooks. BeforePublishCommit runs after
	// all inserts and before commit; its error rolls back the publication.
	// AfterPublishCommit runs after a successful commit.
	Hooks Hooks
}

// Hooks carries the publication seams.
type Hooks struct {
	BeforePublishCommit func() error
	AfterPublishCommit  func()
}

// Open opens (creating if necessary) the private store inside stateDir and
// applies pending schema migrations. A corrupt or newer-than-supported store
// is refused without replacement.
func Open(stateDir string, opts Options) (*Store, error) {
	db, _, err := openDatabase(stateDir, int(opts.BusyTimeout/time.Millisecond))
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return &Store{db: db, Clock: clk}, nil
}

// StreamID returns the internal stream id for (target, account), or false
// when no stream exists.
func (s *Store) StreamID(ctx context.Context, targetID, account string) (int64, bool, error) {
	var streamID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT stream_id FROM targets WHERE target_id = ? AND account = ?`,
		targetID, forge.CanonicalAccount(account)).Scan(&streamID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, &Error{Code: CodeStore, Message: fmt.Sprintf("resolve stream: %v", err)}
	}
	return streamID, true, nil
}

// TargetInfo returns the stored target record for a public target ID.
func (s *Store) TargetInfo(ctx context.Context, targetID string) (forge.Target, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT target_json FROM targets WHERE target_id = ? LIMIT 1`, targetID).Scan(&raw)
	if err == sql.ErrNoRows {
		return forge.Target{}, false, nil
	}
	if err != nil {
		return forge.Target{}, false, &Error{Code: CodeStore, Message: fmt.Sprintf("read target: %v", err)}
	}
	var t forge.Target
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return forge.Target{}, false, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("decode target: %v", err)}
	}
	return t, true, nil
}

// AccountsForTarget returns the canonical accounts that have a stream for
// the target, ordered ascending.
func (s *Store) AccountsForTarget(ctx context.Context, targetID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT account FROM targets WHERE target_id = ? ORDER BY account`, targetID)
	if err != nil {
		return nil, &Error{Code: CodeStore, Message: fmt.Sprintf("read accounts: %v", err)}
	}
	defer rows.Close()
	var accounts []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, &Error{Code: CodeStore, Message: fmt.Sprintf("read accounts: %v", err)}
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// PutLease seeds or replaces an admission lease. It exists for tests and for
// Stage 4's tracker wiring; Publish validates fences against this table.
func (s *Store) PutLease(ctx context.Context, host, account, token string, expiry time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO leases (host, account, token, expiry_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT(host, account) DO UPDATE SET token = excluded.token, expiry_ms = excluded.expiry_ms`,
		host, forge.CanonicalAccount(account), token, expiry.UTC().UnixMilli())
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("put lease: %v", err)}
	}
	return nil
}

// checkFence validates that the fence's lease still exists, matches the
// token, and has not expired according to the store's clock. q is either the
// *sql.DB or a *sql.Tx.
func checkFence(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, f Fence, now time.Time) error {
	var token sql.NullString
	var expiry sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT token, expiry_ms FROM leases WHERE host = ? AND account = ?`,
		f.Host, forge.CanonicalAccount(f.Account)).Scan(&token, &expiry)
	if err == sql.ErrNoRows {
		return &Error{Code: CodeFenceLost, Message: fmt.Sprintf("no admission lease for %s/%s", f.Host, f.Account)}
	}
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("read lease: %v", err)}
	}
	if !token.Valid || token.String == "" || token.String != f.Token {
		return &Error{Code: CodeFenceLost, Message: fmt.Sprintf("admission lease for %s/%s is held by another token", f.Host, f.Account)}
	}
	if !expiry.Valid || expiry.Int64 <= now.UnixMilli() {
		return &Error{Code: CodeFenceLost, Message: fmt.Sprintf("admission lease for %s/%s has expired", f.Host, f.Account)}
	}
	return nil
}
