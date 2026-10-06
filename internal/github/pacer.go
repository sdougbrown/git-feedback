package github

import (
	"context"
	"sync"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
)

// RequestPacer spaces out requests. Wait blocks until the next request may
// be issued or ctx is done.
type RequestPacer interface {
	Wait(ctx context.Context) error
}

// FixedPacer enforces a minimum interval between consecutive requests using
// the injected Clock, so tests advance time instead of sleeping.
type FixedPacer struct {
	Clock    clock.Clock
	Interval time.Duration

	mu   sync.Mutex
	last time.Time
}

// NewFixedPacer returns a FixedPacer with the given minimum interval.
func NewFixedPacer(clk clock.Clock, interval time.Duration) *FixedPacer {
	return &FixedPacer{Clock: clk, Interval: interval}
}

// Wait implements RequestPacer.
func (p *FixedPacer) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	var wait time.Duration
	if now := p.Clock.Now(); !p.last.IsZero() {
		if next := p.last.Add(p.Interval); next.After(now) {
			wait = next.Sub(now)
		}
	}
	p.mu.Unlock()
	if wait > 0 {
		p.Clock.Sleep(wait)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.last = p.Clock.Now()
	p.mu.Unlock()
	return nil
}
