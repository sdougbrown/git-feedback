package cli

import (
	"context"
	"errors"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/sdougbrown/git-feedback/internal/tracker"
)

// handleReconcile runs one one-shot reconciliation through the tracker
// engine: admission, gated authentication, lease, a single collection, and
// fenced publication. It never polls. Rate deferrals surface as the
// deferred status; unexpected operational errors as the error envelope.
func handleReconcile(ctx context.Context, inv Invocation) (Result, error) {
	// Host validation happens before any credential lookup.
	if _, err := parseTarget(inv); err != nil {
		return Result{}, err
	}
	clk := newClock()
	st, err := openStore(inv, clk)
	if err != nil {
		return Result{}, err
	}
	defer st.Close()

	eng, err := newEngine(st, clk)
	if err != nil {
		return Result{}, err
	}
	res, err := eng.Reconcile(ctx, tracker.ReconcileInput{
		URL:     inv.URL,
		Head:    inv.Flags["head"],
		Account: inv.Flags["account"],
	})
	if err != nil {
		return Result{}, mapReconcileError(err)
	}

	r := Result{
		Command: inv.Command,
		Status:  res.Status,
	}
	if res.Target.ID != "" {
		r.Target = envelopeTarget(res.Target)
	}
	if res.HasAccount {
		r.Account = &res.Account
	}
	if res.HasObservedHead {
		r.ObservedHead = &res.ObservedHead
	}
	if res.HasExpectedHead {
		r.ExpectedHead = &res.ExpectedHead
	}
	if res.Snapshot != nil {
		r.Snapshot = snapshotEnvelope(*res.Snapshot)
	}
	r.Freshness = fresh(res.Freshness)
	if res.Attempt != nil {
		r.Attempt = attemptEnvelope(*res.Attempt)
	}
	return r, nil
}

// fresh maps the tracker freshness onto the envelope.
func fresh(f tracker.Freshness) Freshness {
	return Freshness{
		SnapshotObservedAt: stamp(f.SnapshotObservedAt),
		Stale:              f.Stale,
	}
}

// mapReconcileError maps reconciliation failures onto envelope errors.
func mapReconcileError(err error) error {
	var rl *forge.ErrRateLimited
	switch {
	case errors.Is(err, forge.ErrUnsupportedHost):
		return &statusError{code: "unsupported_host", message: err.Error()}
	case errors.Is(err, forge.ErrAuth):
		return &statusError{code: "auth", message: err.Error()}
	case errors.Is(err, forge.ErrNotFound):
		return &statusError{code: "not_found", message: err.Error()}
	case errors.As(err, &rl):
		return &statusError{code: "rate_limited", message: err.Error(), retryable: true}
	}
	var se *store.Error
	if errors.As(err, &se) {
		return &statusError{code: se.Code, message: se.Message}
	}
	return &statusError{code: "collection_error", message: err.Error()}
}
