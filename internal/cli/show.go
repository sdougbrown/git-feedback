package cli

import (
	"context"
	"fmt"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// handleShow returns one event's record and the object content as recorded
// in the event's snapshot. It is a read-only local lookup: no authentication
// or network access occurs, and it acknowledges nothing. The single event
// record renders in events, so a show envelope pipes into ack --events-from -.
func handleShow(ctx context.Context, inv Invocation) (Result, error) {
	eventID := inv.Flags["event"]
	if eventID == "" {
		return Result{}, &usageError{"show requires --event <ID>"}
	}
	if !store.ValidEventID(eventID) {
		return Result{}, &usageError{fmt.Sprintf("invalid event ID %q: expected e<digits>", eventID)}
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
	res, err := st.Show(ctx, store.ShowInput{TargetID: t.ID, Account: account, EventID: eventID})
	if err != nil {
		return Result{}, mapStoreError(err)
	}
	r.Events = []any{eventMap(res.Event)}
	r.Object = res.Object
	return r, nil
}
