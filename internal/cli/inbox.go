package cli

import (
	"context"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// handleInbox returns one bounded page of pending events for one stream and
// consumer. Reading acknowledges nothing.
func handleInbox(ctx context.Context, inv Invocation) (Result, error) {
	consumer := inv.Flags["consumer"]
	if consumer == "" {
		return Result{}, &usageError{"inbox requires --consumer <NAME>"}
	}
	limit, err := parseLimit(inv)
	if err != nil {
		return Result{}, err
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
	page, err := st.Inbox(ctx, store.InboxInput{
		TargetID: t.ID,
		Account:  account,
		Consumer: consumer,
		Cursor:   inv.Flags["after"],
		Limit:    limit,
	})
	if err != nil {
		return Result{}, mapStoreError(err)
	}
	if inv.Bools["ids-only"] {
		// Machine consumers frequently need only the IDs to build --event args.
		ids := make([]any, 0, len(page.Events))
		for _, ev := range page.Events {
			ids = append(ids, ev.ID)
		}
		r.Events = ids
	} else {
		events := make([]any, 0, len(page.Events))
		for _, ev := range page.Events {
			events = append(events, eventMap(ev))
		}
		r.Events = events
	}
	r.HasMore = page.HasMore
	if page.NextCursor != "" {
		r.NextCursor = &page.NextCursor
	}
	return r, nil
}
