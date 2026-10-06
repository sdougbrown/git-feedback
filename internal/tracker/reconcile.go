// Package tracker implements the shared one-shot reconciliation engine used
// by reconcile and wait: bootstrap admission, gated authentication, atomic
// scheduling and lease acquisition, collection outside transactions, and
// fenced finalization. It never polls and never holds a write transaction
// during HTTP I/O.
package tracker

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// Default engine timing, as pinned by the plan.
const (
	DefaultLeaseTTL     = 60 * time.Second
	DefaultLeaseRefresh = 15 * time.Second
	DefaultMinInterval  = 60 * time.Second
)

// Result status values the CLI maps onto the envelope.
const (
	StatusUpdated    = "updated"
	StatusUnchanged  = "unchanged"
	StatusDeferred   = "deferred"
	StatusBusy       = "busy"
	StatusHeadChange = "head_changed"
	StatusError      = "error"
)

// Engine is the shared one-shot tracking engine. It is safe for concurrent
// use as long as the store and clock are.
type Engine struct {
	Store   *store.Store
	Clock   clock.Clock
	APIBase string
	Runner  github.CommandRunner // nil → the real gh CLI
	// LeaseTTL, LeaseRefresh, and MinInterval default to the pinned 60s,
	// 15s, and 60s when zero.
	LeaseTTL     time.Duration
	LeaseRefresh time.Duration
	MinInterval  time.Duration

	// NewServices builds the scope-bound collection services (gate, pacer,
	// cache) for one (host, account) scope. Nil binds the store-backed
	// implementations; exposed for tests that substitute in-memory ones.
	NewServices func(host, account string) Services
}

// Services bundles the scope-bound collection services.
type Services struct {
	Gate  github.RateGate // may also implement github.BackoffGate
	Pacer github.RequestPacer
	Cache github.HTTPCache
}

// ReconcileInput describes one one-shot reconciliation.
type ReconcileInput struct {
	URL string
	// Head pins the collection: a mismatch returns head_changed before any
	// feedback is collected.
	Head string
	// Account is the explicit account; "" verifies the active gh account.
	Account string
	// Session, when supplied (the wait path), skips admission and
	// verification and reuses the verified session. Cadence and lease rules
	// still apply.
	Session forge.Session
}

// Freshness carries the freshness inputs for the envelope.
type Freshness struct {
	SnapshotObservedAt time.Time
	Stale              bool
}

// Result carries everything the CLI envelope needs for one reconciliation.
// Nullability (account, heads, snapshot, attempt) is expressed with the
// Valid/Has booleans so the CLI can emit JSON nulls faithfully.
type Result struct {
	Status string
	Target forge.Target

	Account    string // empty unless a session was verified or supplied
	HasAccount bool

	ObservedHead    string
	HasObservedHead bool
	ExpectedHead    string
	HasExpectedHead bool

	Snapshot *store.SnapshotSummary
	Attempt  *store.Attempt

	Freshness    Freshness
	BlockedUntil time.Time // cadence or gate until, when deferred
}

// durations resolves the engine timing with pinned defaults.
func (e *Engine) durations() (ttl, refresh, min time.Duration) {
	ttl, refresh, min = e.LeaseTTL, e.LeaseRefresh, e.MinInterval
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	if refresh <= 0 {
		refresh = DefaultLeaseRefresh
	}
	if min <= 0 {
		min = DefaultMinInterval
	}
	return ttl, refresh, min
}

// services returns the scope-bound collection services.
func (e *Engine) services(host, account string) Services {
	if e.NewServices != nil {
		return e.NewServices(host, account)
	}
	return Services{
		Gate:  e.Store.NewGate(host, account),
		Pacer: e.Store.NewPacer(host, account),
		Cache: e.Store.NewHTTPCache(host, account),
	}
}

// Reconcile runs one one-shot reconciliation cycle for the URL.
func (e *Engine) Reconcile(ctx context.Context, in ReconcileInput) (Result, error) {
	// Unsupported hosts are rejected before credential lookup and before
	// any admission.
	target, err := github.ParseTarget(in.URL)
	if err != nil {
		return Result{}, err
	}
	host := forge.CanonicalHost(target.Host)
	ttl, refresh, min := e.durations()
	token := newLeaseToken()

	var sess forge.Session
	account := forge.CanonicalAccount(in.Account)
	if in.Session != nil {
		sess = in.Session
		account = forge.CanonicalAccount(sess.Login())
	} else {
		s, verified, res, aerr := e.admit(ctx, target, account, token, ttl)
		if res != nil {
			return *res, nil
		}
		if aerr != nil {
			return Result{}, aerr
		}
		sess, account = s, verified
	}

	// Collection services bound to the verified account's scope.
	gate := e.Store.NewGate(host, account)
	svcs := e.services(host, account)
	adapter := github.NewAdapter(e.APIBase, e.Runner, nil, svcs.Gate, svcs.Pacer, svcs.Cache, e.Clock)

	// One write transaction: recheck next_due, consult the gates, and take
	// the account lease (excluding live bootstrap admission).
	streamID, _, err := e.Store.StreamID(ctx, target.ID, account)
	if err != nil {
		return Result{}, err
	}
	acq, err := e.Store.AcquireCollectionLease(ctx, host, account, token, ttl, streamID, gate)
	if err != nil {
		return Result{}, err
	}
	switch acq.Outcome {
	case store.AcquireBusy:
		return Result{Status: StatusBusy, Target: target}, nil
	case store.AcquireDeferredDue, store.AcquireDeferredGated:
		return e.deferredResult(ctx, target, account, acq.BlockedUntil), nil
	}

	// Collection runs outside any write transaction while a refresh loop
	// extends our lease.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-e.Clock.After(refresh):
				_ = e.Store.RefreshLease(ctx, host, account, token, ttl, e.Clock.Now())
			}
		}
	}()
	coll, collErr := adapter.Collect(ctx, sess, target, forge.CollectOptions{ExpectedHead: in.Head})
	close(stop)
	<-done

	if collErr == nil && coll.Complete {
		return e.publish(ctx, target, host, account, token, min, coll)
	}
	if collErr != nil {
		var rl *forge.ErrRateLimited
		if errors.As(collErr, &rl) {
			// The gate already persisted the backoff; release and defer.
			_ = e.Store.ReleaseLease(ctx, host, account, token)
			return e.deferredResult(ctx, target, account, rl.Until), nil
		}
		if errors.Is(collErr, forge.ErrHeadChanged) {
			observed, expected := coll.HeadAfter, coll.HeadBefore
			if in.Head != "" && coll.HeadBefore != in.Head {
				observed, expected = coll.HeadBefore, in.Head
			}
			att := e.finalizeFenced(ctx, host, account, token, streamID, store.AttemptInput{
				Outcome:      store.OutcomeHeadChange,
				Complete:     false,
				ObservedHead: observed,
				ExpectedHead: expected,
				NextDue:      e.Clock.Now().Add(min),
			})
			res := Result{
				Status:          StatusHeadChange,
				Target:          target,
				Account:         account,
				HasAccount:      true,
				ObservedHead:    observed,
				HasObservedHead: observed != "",
				ExpectedHead:    expected,
				HasExpectedHead: expected != "",
				Snapshot:        e.currentSnapshot(ctx, target.ID, account),
				Freshness:       e.freshness(ctx, target.ID, account),
				Attempt:         att,
				BlockedUntil:    e.nextDueOr(),
			}
			if att != nil {
				res.BlockedUntil = att.NextDue
			}
			return res, nil
		}
		// Unexpected operational error (including context cancellation):
		// fenced failure finalization, then surface the error.
		att := e.finalizeFenced(ctx, host, account, token, streamID, store.AttemptInput{
			Outcome:   store.OutcomeFailed,
			Complete:  false,
			ErrorCode: errorCode(collErr),
			NextDue:   e.Clock.Now().Add(min),
		})
		if ctx.Err() != nil {
			return Result{Attempt: att}, ctx.Err()
		}
		return Result{Status: StatusError, Target: target, Account: account, HasAccount: true, Attempt: att}, collErr
	}
	// A nil error with an incomplete result is an adapter contract bug.
	return Result{Status: StatusError, Target: target, Account: account, HasAccount: true}, forge.ErrIncomplete
}

// publish publishes the complete inventory with the fence and finalizes
// cadence and lease ownership inside the publish transaction.
func (e *Engine) publish(ctx context.Context, target forge.Target, host, account, token string, min time.Duration, coll forge.CollectResult) (Result, error) {
	now := e.Clock.Now()
	nextDue := now.Add(min)
	streamID, _, err := e.Store.StreamID(ctx, target.ID, account)
	if err != nil {
		return Result{}, err
	}
	res, err := e.Store.Publish(ctx, store.PublishInput{
		Target:   target,
		Account:  account,
		Snapshot: coll.Snapshot,
		Fence:    &store.Fence{Host: host, Account: account, Token: token},
		Finalize: func(ctx context.Context, tx *sql.Tx) error {
			var streamID int64
			if err := tx.QueryRowContext(ctx,
				`SELECT stream_id FROM targets WHERE target_id = ? AND account = ?`,
				target.ID, forge.CanonicalAccount(account)).Scan(&streamID); err != nil {
				return err
			}
			if err := e.Store.SetNextDueTx(ctx, tx, streamID, nextDue); err != nil {
				return err
			}
			if err := e.Store.RecordAttemptTx(ctx, tx, store.AttemptInput{
				StreamID:     streamID,
				Outcome:      store.OutcomeOK,
				Complete:     true,
				ObservedHead: coll.Snapshot.Head,
				NextDue:      nextDue,
				At:           now,
			}, now); err != nil {
				return err
			}
			// Release lease ownership in the same transaction.
			_, err := tx.ExecContext(ctx,
				`UPDATE leases SET token = NULL, expiry_ms = NULL WHERE host = ? AND account = ? AND token = ?`,
				host, forge.CanonicalAccount(account), token)
			return err
		},
	})
	if err != nil {
		// The publication (and with it the finalization) rolled back. The
		// fence may still hold; finalize as a failure so cadence advances.
		att := e.finalizeFenced(ctx, host, account, token, streamID, store.AttemptInput{
			Outcome:   store.OutcomeFailed,
			ErrorCode: errorCode(err),
			NextDue:   nextDue,
		})
		_ = att
		return Result{}, err
	}
	status := StatusUpdated
	if !res.Changed {
		status = StatusUnchanged
	}
	return Result{
		Status:          status,
		Target:          target,
		Account:         account,
		HasAccount:      true,
		ObservedHead:    coll.Snapshot.Head,
		HasObservedHead: true,
		Snapshot:        e.currentSnapshot(ctx, target.ID, account),
		Freshness:       e.freshness(ctx, target.ID, account),
	}, nil
}

// currentSnapshot returns the stream's current snapshot summary, or nil.
func (e *Engine) currentSnapshot(ctx context.Context, targetID, account string) *store.SnapshotSummary {
	sum, ok, err := e.Store.CurrentSnapshot(ctx, targetID, account)
	if err != nil || !ok {
		return nil
	}
	return &sum
}

// freshness computes the freshness inputs for the stream's current snapshot.
func (e *Engine) freshness(ctx context.Context, targetID, account string) Freshness {
	sum, ok, err := e.Store.CurrentSnapshot(ctx, targetID, account)
	if err != nil || !ok {
		return Freshness{}
	}
	_, min, _ := e.durations()
	return Freshness{
		SnapshotObservedAt: sum.ObservedAt,
		Stale:              !sum.ObservedAt.IsZero() && e.Clock.Now().Sub(sum.ObservedAt) > min,
	}
}

// deferredResult builds a deferred result with the stream's current
// snapshot. Without a verified account, account and snapshot stay null.
func (e *Engine) deferredResult(ctx context.Context, target forge.Target, account string, blockedUntil time.Time) Result {
	res := Result{
		Status:       StatusDeferred,
		Target:       target,
		BlockedUntil: blockedUntil,
	}
	if account != "" {
		res.Account, res.HasAccount = account, true
		res.Snapshot = e.currentSnapshot(ctx, target.ID, account)
		res.Freshness = e.freshness(ctx, target.ID, account)
	}
	return res
}
