package tracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// newLeaseToken returns a fresh 128-bit random hex lease token.
func newLeaseToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The token only needs to be unique among this store's callers.
		return hex.EncodeToString([]byte(time.Now().String()))[:32]
	}
	return hex.EncodeToString(b[:])
}

// isBusy reports whether err is the store's lease-busy error.
func isBusy(err error) bool {
	var se *store.Error
	return errors.As(err, &se) && se.Code == store.CodeLeaseBusy
}

// leaseAdmission adapts the store's bootstrap lease to the github
// BootstrapAdmission seam. Acquire re-takes (and thereby refreshes) the
// lease the engine already holds; Release is a no-op because the engine
// itself releases after transferring verification state to the verified
// account.
type leaseAdmission struct {
	engine *Engine
	token  string
	ttl    time.Duration
}

// Acquire implements github.BootstrapAdmission.
func (a *leaseAdmission) Acquire(ctx context.Context, host string) error {
	return a.engine.acquireBootstrap(ctx, host, a.token, a.ttl)
}

// Release implements github.BootstrapAdmission. Intentionally a no-op.
func (a *leaseAdmission) Release(host string) {}

// admit takes bootstrap admission, runs gated authentication, and transfers
// the verification state (pacing and gate quota) to the verified account
// before releasing the bootstrap lease. A non-nil Result short-circuits the
// reconciliation with busy/deferred; an error is fatal for this cycle.
func (e *Engine) admit(ctx context.Context, target forge.Target, account, token string, ttl time.Duration) (forge.Session, string, *Result, error) {
	host := forge.CanonicalHost(target.Host)
	if err := e.acquireBootstrap(ctx, host, token, ttl); err != nil {
		if isBusy(err) {
			return nil, "", &Result{Status: StatusBusy, Target: target}, nil
		}
		return nil, "", nil, err
	}
	adm := &leaseAdmission{engine: e, token: token, ttl: ttl}
	svcs := e.services(host, "")
	authAdapter := newGithubAdapter(e, adm, svcs)
	sess, err := authAdapter.Authenticate(ctx, account)
	if err != nil {
		e.releaseBootstrap(ctx, host, token)
		var rl *forge.ErrRateLimited
		if errors.As(err, &rl) {
			return nil, "", ptr(e.deferredResult(ctx, target, "", rl.Until)), nil
		}
		return nil, "", nil, err
	}
	verified := forge.CanonicalAccount(sess.Login())
	// The verification request's pacing timestamp belongs to the verified
	// account's schedule now; adopt the recorded gate quota with it.
	now := e.Clock.Now()
	if err := e.Store.TransferPace(ctx, host, "", verified, now); err != nil {
		return nil, "", nil, err
	}
	if err := e.Store.AdoptGateQuota(ctx, host, verified); err != nil {
		return nil, "", nil, err
	}
	if err := e.Store.ReleaseLease(ctx, host, "", token); err != nil {
		return nil, "", nil, err
	}
	return sess, verified, nil, nil
}

// acquireBootstrap acquires the empty-account lease.
func (e *Engine) acquireBootstrap(ctx context.Context, host, token string, ttl time.Duration) error {
	return e.Store.AcquireBootstrapLease(ctx, host, token, ttl, e.Clock.Now())
}

// releaseBootstrap releases the caller's bootstrap admission token.
func (e *Engine) releaseBootstrap(ctx context.Context, host, token string) {
	_ = e.Store.ReleaseLease(ctx, host, "", token)
}

// newGithubAdapter builds the GitHub adapter over the engine's transport
// inputs and the given scope-bound services.
func newGithubAdapter(e *Engine, admission github.BootstrapAdmission, svcs Services) *github.Adapter {
	clk := e.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return github.NewAdapter(e.APIBase, e.Runner, admission, svcs.Gate, svcs.Pacer, svcs.Cache, clk)
}

// errorCode maps an operational failure to a stable attempt error code.
func errorCode(err error) string {
	var rl *forge.ErrRateLimited
	switch {
	case errors.Is(err, forge.ErrIncomplete):
		return "incomplete"
	case errors.Is(err, forge.ErrAuth):
		return "auth"
	case errors.Is(err, forge.ErrNotFound):
		return "not_found"
	case errors.Is(err, forge.ErrHeadChanged):
		return "head_changed"
	case errors.As(err, &rl):
		return "rate_limited"
	default:
		var se *store.Error
		if errors.As(err, &se) {
			return se.Code
		}
		return "collection_error"
	}
}

func ptr[T any](v T) *T { return &v }
