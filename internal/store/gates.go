package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
)

// queryer is the read/write surface shared by *sql.DB and *sql.Tx, so gate
// checks can run either standalone or inside the acquisition transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Gate is the store-backed github.RateGate for one (host, account) scope.
// An empty account is the bootstrap/host-wide scope: it conservatively
// honors every known account's gate rows for the resource. The reserved
// `secondary` resource records account-wide secondary backoff and is
// checked for both transports (and, host-wide, for bootstrap scopes).
type Gate struct {
	db      *sql.DB
	host    string
	account string // canonical; "" is bootstrap scope
}

// NewGate returns the rate-gate service bound to one (host, account) scope.
func (s *Store) NewGate(host, account string) *Gate {
	return &Gate{db: s.db, host: host, account: forge.CanonicalAccount(account)}
}

// Check implements github.RateGate. It mirrors github.MemoryGate semantics:
// an until-window blocks until it passes, and a budget below the reserve
// blocks until its reset.
func (g *Gate) Check(resource string, now time.Time) error {
	return g.check(context.Background(), g.db, resource, now)
}

// check evaluates the gate for one resource against q, which is either the
// pool or the acquisition transaction.
func (g *Gate) check(ctx context.Context, q queryer, resource string, now time.Time) error {
	type candidate struct {
		account string
		scope   string // "" = the scope's own row for resource
	}
	cands := []candidate{{account: g.account, scope: resource}}
	if resource != github.ResourceSecondary {
		// Account-wide secondary backoff blocks both transports.
		cands = append(cands, candidate{account: g.account, scope: github.ResourceSecondary})
	}
	if g.account == "" {
		// Bootstrap honors known account gates for the resource.
		cands = append(cands, candidate{account: "*", scope: resource})
	}
	if g.account == "" {
		// Bootstrap checks host-wide secondary backoff.
		cands = append(cands, candidate{account: "*", scope: github.ResourceSecondary})
	}

	var until time.Time
	for _, c := range cands {
		var rows *sql.Rows
		var err error
		if c.account == "*" {
			rows, err = q.QueryContext(ctx,
				`SELECT remaining, reset_ms, until_ms FROM rate_gates
				 WHERE host = ? AND resource = ? AND account != ''`, g.host, c.scope)
		} else {
			rows, err = q.QueryContext(ctx,
				`SELECT remaining, reset_ms, until_ms FROM rate_gates
				 WHERE host = ? AND account = ? AND resource = ?`, g.host, c.account, c.scope)
		}
		if err != nil {
			return &Error{Code: CodeStore, Message: "read rate gate: " + err.Error()}
		}
		for rows.Next() {
			var remaining, reset, rowUntil sql.NullInt64
			if err := rows.Scan(&remaining, &reset, &rowUntil); err != nil {
				rows.Close()
				return &Error{Code: CodeStore, Message: "read rate gate: " + err.Error()}
			}
			if t, blocked := gateRowBlocks(remaining, reset, rowUntil, now); blocked && t.After(until) {
				until = t
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return &Error{Code: CodeStore, Message: "read rate gate: " + err.Error()}
		}
		rows.Close()
	}
	if !until.IsZero() {
		return &forge.ErrRateLimited{Resource: resource, Until: until}
	}
	return nil
}

// gateRowBlocks applies the MemoryGate semantics to one persisted row.
func gateRowBlocks(remaining, reset, until sql.NullInt64, now time.Time) (time.Time, bool) {
	if until.Valid && until.Int64 > now.UnixMilli() {
		return time.UnixMilli(until.Int64).UTC(), true
	}
	if remaining.Valid && remaining.Int64 > 0 && remaining.Int64 < github.ReserveRemaining &&
		reset.Valid && time.UnixMilli(reset.Int64).UTC().After(now) {
		return time.UnixMilli(reset.Int64).UTC(), true
	}
	return time.Time{}, false
}

// Record implements github.RateGate: it upserts the quota observation and,
// mirroring MemoryGate, blocks through the reset when the budget is in the
// reserve.
func (g *Gate) Record(info forge.RateInfo) {
	reset := int64(0)
	if !info.Reset.IsZero() {
		reset = info.Reset.UTC().UnixMilli()
	}
	until := sql.NullInt64{}
	if info.Remaining >= 0 && info.Remaining < github.ReserveRemaining {
		until = sql.NullInt64{Int64: reset, Valid: true}
	}
	if _, err := g.db.Exec(
		`INSERT INTO rate_gates (host, account, resource, remaining, rate_limit, reset_ms, until_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(host, account, resource) DO UPDATE SET
		   remaining = excluded.remaining,
		   rate_limit = excluded.rate_limit,
		   reset_ms = excluded.reset_ms,
		   until_ms = CASE WHEN excluded.remaining >= 0 AND excluded.remaining < ?
		                  THEN excluded.reset_ms ELSE rate_gates.until_ms END`,
		g.host, g.account, info.Resource, info.Remaining, info.Limit, reset, until, github.ReserveRemaining); err != nil {
		// Recording is advisory; the next Check re-reads persisted state.
		return
	}
}

// Backoff implements github.BackoffGate: it extends the resource's
// until-window and records the same window as account-wide secondary
// backoff, which blocks both transports.
func (g *Gate) Backoff(resource string, until time.Time) {
	if until.IsZero() {
		return
	}
	ms := until.UTC().UnixMilli()
	for _, res := range []string{resource, github.ResourceSecondary} {
		if _, err := g.db.Exec(
			`INSERT INTO rate_gates (host, account, resource, until_ms) VALUES (?, ?, ?, ?)
			 ON CONFLICT(host, account, resource) DO UPDATE SET
			   until_ms = MAX(COALESCE(rate_gates.until_ms, 0), excluded.until_ms)`,
			g.host, g.account, res, ms); err != nil {
			return
		}
	}
}

// AdoptGateQuota copies the bootstrap scope's persisted quota for all
// resources to the verified account, adopting the quota recorded during
// identity verification.
func (s *Store) AdoptGateQuota(ctx context.Context, host, account string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rate_gates (host, account, resource, remaining, rate_limit, reset_ms, until_ms)
		 SELECT host, ?, resource, remaining, rate_limit, reset_ms, until_ms
		   FROM rate_gates WHERE host = ? AND account = ''
		 ON CONFLICT(host, account, resource) DO UPDATE SET
		   remaining = excluded.remaining, rate_limit = excluded.rate_limit,
		   reset_ms = excluded.reset_ms, until_ms = excluded.until_ms`,
		forge.CanonicalAccount(account), host)
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("adopt gate quota: %v", err)}
	}
	return nil
}
