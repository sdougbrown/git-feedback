package cli

import (
	"context"
	"time"

	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/sdougbrown/git-feedback/internal/tracker"
)

// waitDefaultTimeout is the pinned default wait deadline.
const waitDefaultTimeout = 30 * time.Minute

// handleWait delivers pending events, running the shared reconciliation
// engine on its persisted schedule until events appear, a fatal error
// occurs, or the deadline expires. Stored backlog is delivered without
// remote authentication; delivery acknowledges nothing.
func handleWait(ctx context.Context, inv Invocation) (Result, error) {
	// Host validation happens before any credential lookup or store work.
	if _, err := parseTarget(inv); err != nil {
		return Result{}, err
	}
	consumer := inv.Flags["consumer"]
	if consumer == "" {
		return Result{}, &usageError{"wait requires --consumer <NAME>"}
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
	eng.OpenStore = func(busy time.Duration) (*store.Store, error) {
		return openHookedStore(inv, clk, busy)
	}

	res, err := eng.Wait(ctx, tracker.WaitInput{
		URL:      inv.URL,
		Head:     inv.Flags["head"],
		Account:  inv.Flags["account"],
		Consumer: consumer,
		Limit:    limit,
	})
	if err != nil {
		return Result{}, mapReconcileError(err)
	}
	return waitResult(inv.Command, res), nil
}

// waitResult maps the tracker wait outcome onto the envelope.
func waitResult(command string, res tracker.WaitResult) Result {
	r := Result{
		Command: command,
		Status:  res.Status,
		Target:  envelopeTarget(res.Target),
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
	events := []any{}
	for _, ev := range res.Events {
		events = append(events, eventMap(ev))
	}
	r.Events = events
	r.HasMore = res.HasMore
	if res.NextCursor != "" {
		r.NextCursor = &res.NextCursor
	}
	return r
}
