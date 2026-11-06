package cli

import (
	"context"
	"io"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/sdougbrown/git-feedback/internal/tracker"
)

func handleBaseline(ctx context.Context, inv Invocation) (Result, error) {
	clk := newClock()
	st, err := openStore(inv, clk)
	if err != nil {
		return Result{}, err
	}
	defer st.Close()
	t, account, err := resolveLocal(ctx, st, inv)
	if err != nil {
		return Result{}, err
	}
	r, err := localEnvelope(ctx, inv.Command, st, clk, t, account)
	if err != nil {
		return Result{}, err
	}
	baseline, err := st.StreamBaseline(ctx, t.ID, account)
	if err != nil {
		return Result{}, mapStoreError(err)
	}
	if r.Snapshot == nil || r.Snapshot.ID != baseline.SnapshotID {
		return Result{}, &statusError{code: "busy", message: "snapshot changed while reading baseline; retry", retryable: true}
	}
	r.NextCursor = &baseline.Cursor
	return r, nil
}

func handleWatch(ctx context.Context, inv Invocation, stdout io.Writer) (Result, error) {
	target, err := parseTarget(inv)
	if err != nil {
		return Result{}, err
	}
	cursor := inv.Flags["after"]
	if cursor == "" {
		return Result{}, &usageError{Message: "watch requires --after <CURSOR>; run baseline first"}
	}
	account, err := store.StreamCursorAccount(cursor, target.ID)
	if err != nil {
		return Result{}, mapStoreError(err)
	}
	if selected := inv.Flags["account"]; selected != "" && forge.CanonicalAccount(selected) != account {
		return Result{}, &usageError{Message: "watch cursor belongs to a different account"}
	}
	limit, err := parseLimit(inv)
	if err != nil {
		return Result{}, err
	}
	clk := newClock()
	eng, err := newEngineBase(clk)
	if err != nil {
		return Result{}, err
	}
	eng.OpenStore = func(busy time.Duration) (*store.Store, error) { return openHookedStore(inv, clk, busy) }
	for {
		res, err := eng.Wait(ctx, tracker.WaitInput{
			URL: inv.Args[0], Account: account, Cursor: cursor, Limit: limit,
			IncludeSelf: inv.Bools["include-self"], SkipEmptyReviews: inv.Bools["skip-empty-reviews"], SkipHeadChanged: inv.Bools["skip-head-changed"],
		})
		if err != nil {
			return Result{}, mapReconcileError(err)
		}
		r := waitResult(inv.Command, res)
		if res.Status != tracker.StatusEvents {
			r.NextCursor = &cursor
			return r, nil
		}
		if inv.Bools["stop-on-events"] {
			return r, nil
		}
		if err := Write(stdout, r); err != nil {
			return Result{}, &statusError{code: "output_error", message: err.Error()}
		}
		cursor = res.NextCursor
	}
}
