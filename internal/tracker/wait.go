package tracker

import (
	"context"
	"errors"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// Wait statuses: the inbox shape on delivery, or the deadline expiry.
const (
	StatusEvents  = "events"
	StatusTimeout = "timeout"
)

// MaxCycleBound is the pinned ceiling on one shared-engine cycle inside a
// wait, so a hanging remote can never outlive the deadline machinery.
const MaxCycleBound = 90 * time.Second

// busyRetryBackoff is how long a busy observation sleeps before retrying.
// Lease re-acquisition issues no GitHub requests.
const busyRetryBackoff = time.Second

// clock resolves the engine's clock with the real default.
func (e *Engine) clock() clock.Clock {
	if e.Clock == nil {
		return clock.Real{}
	}
	return e.Clock
}

// WaitInput selects what wait waits for.
type WaitInput struct {
	URL string
	// Head pins the delivery: a cycle observing a different head is fatal.
	Head string
	// Account is the explicit account; "" lets admitted authentication
	// select the active account.
	Account string
	// Consumer receives the events; delivery acknowledges nothing.
	Consumer string
	// Limit bounds the delivered page (0 uses the store default).
	Limit int
}

// WaitResult is the terminal wait outcome: delivered events or the
// deadline expiry with the last recorded attempt.
type WaitResult struct {
	Status string
	Target forge.Target

	Account    string
	HasAccount bool

	ObservedHead    string
	HasObservedHead bool
	ExpectedHead    string
	HasExpectedHead bool

	Snapshot *store.SnapshotSummary
	Attempt  *store.Attempt

	Freshness Freshness

	Events     []store.Event
	HasMore    bool
	NextCursor string
}

// OpenStore, when set, opens a store whose SQLite lock waits are bounded by
// the given duration. Wait opens one store for the backlog pre-check and
// reopens it per cycle so lock waits never exceed the remaining deadline.
// Nil reuses the engine's store for every cycle.
type OpenStore func(busy time.Duration) (*store.Store, error)

// Wait delivers pending events for the consumer, running the shared
// reconciliation engine on its persisted schedule until events appear, a
// fatal error occurs, or the context deadline expires. Stored backlog is
// delivered without remote authentication; ambiguous stored accounts are
// resolved only by admitted authentication, never by guessing.
func (e *Engine) Wait(ctx context.Context, in WaitInput) (WaitResult, error) {
	target, err := github.ParseTarget(in.URL)
	if err != nil {
		return WaitResult{}, err
	}
	clk := e.clock()

	eng, closeStore, err := e.storeFor(clk, remainingOf(ctx))
	if err != nil {
		return WaitResult{}, err
	}
	defer closeStore()

	// Resolve the backlog pre-check account only from explicit input or an
	// unambiguous stored stream; ambiguity defers to authentication.
	account := forge.CanonicalAccount(in.Account)
	if account == "" {
		accounts, aerr := eng.Store.AccountsForTarget(ctx, target.ID)
		if aerr != nil {
			return WaitResult{}, err
		}
		if len(accounts) == 1 {
			account = accounts[0]
		}
	}

	var (
		session     forge.Session
		lastAttempt *store.Attempt
	)
	for {
		// Inbox check: no authentication, no GitHub requests.
		if account != "" {
			res, done, err := e.backlog(ctx, eng, target, account, in)
			if err != nil {
				return WaitResult{}, err
			}
			if done {
				return res, nil
			}
		}

		remaining := remainingOf(ctx)
		if remaining <= 0 {
			return e.timeoutResult(eng, target, account, in, lastAttempt), nil
		}
		// One shared-engine cycle, bounded by min(90s, remaining).
		cycle, cancel := context.WithTimeout(ctx, min(MaxCycleBound, remaining))
		cycleEng, closeCycle, err := e.storeFor(clk, remainingOf(cycle))
		if err != nil {
			cancel()
			return WaitResult{}, err
		}
		res, rerr := cycleEng.Reconcile(cycle, ReconcileInput{
			URL:     in.URL,
			Head:    in.Head,
			Account: in.Account,
			Session: session,
		})
		cancel()
		lastAttempt = attemptOf(res, cycleEng, target, account, lastAttempt)
		closeCycle()

		if rerr != nil {
			if fatalWaitError(rerr) || ctx.Err() != nil {
				if !fatalWaitError(rerr) {
					return e.timeoutResult(eng, target, account, in, lastAttempt), nil
				}
				return WaitResult{}, rerr
			}
			// Transient failure: the engine recorded the next attempt on
			// the shared cadence; sleep until then, bounded by deadline.
		} else if res.Status == StatusHeadChange && in.Head != "" {
			// A pinned-head mismatch is fatal: surface it immediately.
			return waitHeadChanged(res), nil
		}

		if res.HasAccount && res.Account != "" {
			account = res.Account
		}
		if res.Session != nil {
			session = res.Session
		}

		// Events may exist now even if the cycle changed nothing.
		if account != "" {
			res, done, err := e.backlog(ctx, eng, target, account, in)
			if err != nil {
				return WaitResult{}, err
			}
			if done {
				return res, nil
			}
		}

		if err := e.sleepUntil(ctx, clk, wakeTime(res, rerr, eng)); err != nil {
			return e.timeoutResult(eng, target, account, in, lastAttempt), nil
		}
	}
}

// attemptOf captures the latest attempt for the timeout result: the cycle's
// own recorded attempt, or the store's latest attempt for the stream.
func attemptOf(res Result, cycleEng *Engine, target forge.Target, account string, prev *store.Attempt) *store.Attempt {
	if res.Attempt != nil {
		return res.Attempt
	}
	if !res.HasAccount || res.Account == "" {
		if account == "" {
			return prev
		}
		res.Account = account
	}
	if att, ok, err := cycleEng.Store.LatestAttempt(context.Background(), target.ID, res.Account); err == nil && ok {
		return &att
	}
	return prev
}

// wakeTime computes the next wake instant from one cycle's outcome. Sleeps
// honor the shared cadence: published, failed, or head-changed cycles sleep
// until the persisted next_due, deferrals until the gate lifts, and busy
// until the short retry backoff.
func wakeTime(res Result, rerr error, eng *Engine) time.Time {
	now := eng.clock().Now()
	_, _, min := eng.durations()
	if rerr != nil || res.Status == StatusHeadChange {
		if res.Attempt != nil && res.Attempt.HasNextDue {
			return res.Attempt.NextDue
		}
		return now.Add(min)
	}
	switch res.Status {
	case StatusDeferred, StatusBusy:
		if res.Status == StatusDeferred && !res.BlockedUntil.IsZero() && res.BlockedUntil.After(now) {
			return res.BlockedUntil
		}
		return now.Add(busyRetryBackoff)
	default: // updated / unchanged: cadence was persisted at publication.
		if res.Attempt != nil && res.Attempt.HasNextDue {
			return res.Attempt.NextDue
		}
		return now.Add(min)
	}
}

// backlog returns the pending page for (target, account, consumer) without
// any remote access. done is false when the stream has no pending events or
// does not exist yet.
func (e *Engine) backlog(ctx context.Context, eng *Engine, target forge.Target, account string, in WaitInput) (WaitResult, bool, error) {
	page, err := eng.Store.Inbox(ctx, store.InboxInput{
		TargetID: target.ID,
		Account:  account,
		Consumer: in.Consumer,
		Limit:    in.Limit,
	})
	if err != nil {
		var se *store.Error
		if errors.As(err, &se) && se.Code == store.CodeUnknownStream {
			return WaitResult{}, false, nil
		}
		return WaitResult{}, false, err
	}
	if len(page.Events) == 0 {
		return WaitResult{}, false, nil
	}
	sum, ok, err := eng.Store.CurrentSnapshot(ctx, target.ID, account)
	if err != nil {
		return WaitResult{}, false, err
	}
	res := WaitResult{
		Status:     StatusEvents,
		Target:     target,
		Account:    account,
		HasAccount: true,
		Events:     page.Events,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}
	if ok {
		res.Snapshot = &sum
		res.ObservedHead, res.HasObservedHead = sum.Head, true
	}
	if in.Head != "" {
		res.ExpectedHead, res.HasExpectedHead = in.Head, true
		// Delivered backlog never asserts a fresh read of the pinned tip:
		// stale records that the stored head differs from the request.
		res.Freshness.Stale = res.ObservedHead != in.Head
	} else {
		_, _, min := e.durations()
		res.Freshness.Stale = ok && !sum.ObservedAt.IsZero() && e.clock().Now().Sub(sum.ObservedAt) > min
	}
	return res, true, nil
}

// waitHeadChanged maps a fatal pinned-head mismatch onto the wait result.
func waitHeadChanged(res Result) WaitResult {
	return WaitResult{
		Status:          res.Status,
		Target:          res.Target,
		Account:         res.Account,
		HasAccount:      res.HasAccount,
		ObservedHead:    res.ObservedHead,
		HasObservedHead: res.HasObservedHead,
		ExpectedHead:    res.ExpectedHead,
		HasExpectedHead: res.HasExpectedHead,
		Snapshot:        res.Snapshot,
		Attempt:         res.Attempt,
		Freshness:       res.Freshness,
	}
}

// timeoutResult builds the deadline expiry result with the last attempt.
func (e *Engine) timeoutResult(eng *Engine, target forge.Target, account string, in WaitInput, lastAttempt *store.Attempt) WaitResult {
	res := WaitResult{Status: StatusTimeout, Target: target}
	if account != "" {
		res.Account, res.HasAccount = account, true
		res.Snapshot = eng.currentSnapshot(context.Background(), target.ID, account)
		res.Freshness = eng.freshness(context.Background(), target.ID, account)
	}
	if in.Head != "" {
		res.ExpectedHead, res.HasExpectedHead = in.Head, true
	}
	res.Attempt = lastAttempt
	return res
}

// sleepUntil sleeps until wake, bounded by the context deadline, without
// any GitHub request. A context cancellation reports the deadline expiry.
func (e *Engine) sleepUntil(ctx context.Context, clk clock.Clock, wake time.Time) error {
	if deadline, ok := ctx.Deadline(); ok && wake.After(deadline) {
		wake = deadline
	}
	d := wake.Sub(clk.Now())
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clk.After(d):
		return nil
	}
}

// storeFor binds an engine copy to a store whose SQLite lock waits are
// bounded by the remaining deadline (clamped to the pinned 5s ceiling).
// A nil OpenStore reuses the engine's store for every cycle.
func (e *Engine) storeFor(clk clock.Clock, remaining time.Duration) (*Engine, func(), error) {
	if e.OpenStore == nil {
		if e.Store == nil {
			return nil, nil, errors.New("tracker: engine has no store")
		}
		return e, func() {}, nil
	}
	st, err := e.OpenStore(clampBusy(remaining))
	if err != nil {
		return nil, nil, err
	}
	eng := *e
	eng.Store = st
	return &eng, func() { st.Close() }, nil
}

// clampBusy bounds the SQLite busy timeout by the remaining deadline.
func clampBusy(remaining time.Duration) time.Duration {
	const ceiling = 5 * time.Second
	const floor = 10 * time.Millisecond
	if remaining <= 0 || remaining > ceiling {
		return ceiling
	}
	if remaining < floor {
		return floor
	}
	return remaining
}

// remainingOf returns the context's remaining deadline; a context without a
// deadline reports the unbounded cycle ceiling, and expiry reports zero.
func remainingOf(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return MaxCycleBound
	}
	d := time.Until(deadline)
	if d < 0 {
		return 0
	}
	return d
}

// fatalWaitError reports whether one reconciliation failure must end the
// wait immediately instead of being retried until the deadline.
func fatalWaitError(err error) bool {
	return errors.Is(err, forge.ErrAuth) ||
		errors.Is(err, forge.ErrNotFound) ||
		errors.Is(err, forge.ErrAccountMismatch) ||
		errors.Is(err, forge.ErrUnsupportedHost)
}
