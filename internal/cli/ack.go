package cli

import (
	"context"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// handleAck acknowledges exactly the supplied event IDs for one consumer and
// stream, transactionally and idempotently. Unknown or out-of-scope IDs
// reject the whole request. No authentication or network access occurs.
func handleAck(ctx context.Context, inv Invocation) (Result, error) {
	consumer := inv.Flags["consumer"]
	if consumer == "" {
		return Result{}, &usageError{"ack requires --consumer <NAME>"}
	}
	if len(inv.Events) == 0 {
		return Result{}, &usageError{"ack requires at least one --event <ID>"}
	}
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
	if _, err := st.Ack(ctx, store.AckInput{
		TargetID: t.ID,
		Account:  account,
		Consumer: consumer,
		EventIDs: inv.Events,
	}); err != nil {
		return Result{}, mapStoreError(err)
	}
	return r, nil
}
