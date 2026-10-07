package cli

import (
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/sdougbrown/git-feedback/internal/tracker"
)

// newEngineBase builds the tracker engine with the shared wiring: clock,
// test-only environment overrides (inert in the default build), and the
// gh runner. Callers set the store-specific field (Store or OpenStore)
// and any store-level hooks.
func newEngineBase(clk clock.Clock) (*tracker.Engine, error) {
	hooks := ReadTestHooks()
	apiBase, err := hooks.APIOverride()
	if err != nil {
		return nil, &usageError{err.Error()}
	}
	minInterval, err := hooks.Interval()
	if err != nil {
		return nil, &usageError{err.Error()}
	}
	return &tracker.Engine{
		Clock:       clk,
		APIBase:     apiBase,
		Runner:      hooks.GHRunner(),
		MinInterval: minInterval,
	}, nil
}

// newEngine builds the tracker engine for one invocation, applying the
// test-only environment overrides (inert in the default build) and the
// store-level crash hook.
func newEngine(st *store.Store, clk clock.Clock) (*tracker.Engine, error) {
	eng, err := newEngineBase(clk)
	if err != nil {
		return nil, err
	}
	eng.Store = st
	ReadTestHooks().ApplyStoreCrash(st)
	return eng, nil
}

// openHookedStore opens a state-dir store with the given busy timeout,
// applying the test-only crash hook.
func openHookedStore(inv Invocation, clk clock.Clock, busyTimeout time.Duration) (*store.Store, error) {
	dir, err := stateDir(inv)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dir, store.Options{Clock: clk, BusyTimeout: busyTimeout})
	if err != nil {
		return nil, &statusError{code: "store_error", message: err.Error(), err: err}
	}
	ReadTestHooks().ApplyStoreCrash(st)
	return st, nil
}
