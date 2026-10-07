package github

import (
	"context"
	"sync"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// ReserveRemaining is the minimum number of requests or points that must
// remain in a resource budget before the transport will spend another one.
const ReserveRemaining = 10

// RateGate admits requests per resource and records quota observations.
type RateGate interface {
	// Check returns nil when a request on resource may proceed at now.
	Check(ctx context.Context, resource string, now time.Time) error
	// Record stores one quota observation for a resource.
	Record(info forge.RateInfo)
}

// BackoffGate is an optional RateGate extension that records out-of-band
// backoff windows such as Retry-After and X-Poll-Interval.
type BackoffGate interface {
	Backoff(resource string, until time.Time)
}

type gateState struct {
	remaining int
	limit     int
	reset     time.Time
	until     time.Time
}

// MemoryGate is the in-memory RateGate used before Stage 4 supplies
// persistent, SQLite-backed gates.
type MemoryGate struct {
	mu    sync.Mutex
	state map[string]*gateState
}

// NewMemoryGate returns an empty MemoryGate.
func NewMemoryGate() *MemoryGate {
	return &MemoryGate{state: map[string]*gateState{}}
}

// Check implements RateGate.
func (g *MemoryGate) Check(_ context.Context, resource string, now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.state[resource]
	if !ok {
		return nil
	}
	if now.Before(st.until) {
		return &forge.ErrRateLimited{Resource: resource, Until: st.until}
	}
	if st.remaining > 0 && st.remaining < ReserveRemaining && now.Before(st.reset) {
		return &forge.ErrRateLimited{Resource: resource, Until: st.reset}
	}
	return nil
}

// Record implements RateGate.
func (g *MemoryGate) Record(info forge.RateInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.state[info.Resource]
	if !ok {
		st = &gateState{}
		g.state[info.Resource] = st
	}
	st.remaining = info.Remaining
	st.limit = info.Limit
	st.reset = info.Reset
	if info.Remaining >= 0 && info.Remaining < ReserveRemaining {
		st.until = info.Reset
	}
}

// Backoff implements BackoffGate.
func (g *MemoryGate) Backoff(resource string, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.state[resource]
	if !ok {
		st = &gateState{}
		g.state[resource] = st
	}
	if until.After(st.until) {
		st.until = until
	}
}
