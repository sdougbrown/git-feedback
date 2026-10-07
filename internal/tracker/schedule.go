package tracker

import (
	"context"
	"time"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// finalizeFenced records one fenced attempt finalization: validate the
// fence, record the attempt with its cadence, and release lease ownership.
// When the fence no longer holds (expired or taken over), nothing is
// written; the caller's token release is attempted separately and is a
// no-op unless we still own the lease. It returns the recorded attempt, or
// nil when the fence was lost.
func (e *Engine) finalizeFenced(ctx context.Context, host, account, token string, streamID int64, in store.AttemptInput) *store.Attempt {
	in.StreamID = streamID
	att, err := e.Store.FinalizeAttempt(ctx, store.FinalizeInput{
		Fence:   &store.Fence{Host: host, Account: account, Token: token},
		Attempt: in,
	})
	if err != nil {
		// Fence lost or store failure: never record cadence we do not own.
		_ = e.Store.ReleaseLease(ctx, host, account, token)
		return nil
	}
	return &att
}

// nextDueOr returns the cadence a failed cycle would have scheduled: now
// plus the minimum interval.
func (e *Engine) nextDueOr() time.Time {
	_, _, min := e.durations()
	return e.Clock.Now().Add(min)
}
