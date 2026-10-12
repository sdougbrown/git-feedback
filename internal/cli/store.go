package cli

import (
	"context"
	"os"
	"path/filepath"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// handleStore dispatches the store subcommand. The store is a disposable
// cache: `path` prints where it lives, and `reset` deletes it — wholly or
// scoped to one target — so the next collection starts from a new baseline.
// parseInvocation guarantees Sub is "path" or "reset" before dispatch.
func handleStore(ctx context.Context, inv Invocation) (Result, error) {
	if inv.Sub == "path" {
		return handleStorePath(inv)
	}
	return handleStoreReset(ctx, inv)
}

// handleStorePath resolves and reports the effective store directory. It
// never opens or creates the store.
func handleStorePath(inv Invocation) (Result, error) {
	dir, err := stateDir(inv)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Command: inv.Command,
		Status:  StatusOK,
		Store:   &StoreInfo{Path: dir},
	}, nil
}

// handleStoreReset deletes the store. Without --target the database files
// are removed directly — the store is never opened, so reset is also the
// remedy for a store that Open refuses. With --target only that target's
// streams, snapshots, events, attempts, and acknowledgements are deleted;
// other targets and host-scoped state survive.
func handleStoreReset(ctx context.Context, inv Invocation) (Result, error) {
	dir, err := stateDir(inv)
	if err != nil {
		return Result{}, err
	}
	r := Result{Command: inv.Command, Store: &StoreInfo{Path: dir}}
	if raw := inv.Flags["target"]; raw == "" {
		removed, err := store.Reset(dir)
		if err != nil {
			return r, mapStoreError(err)
		}
		r.Store.Removed = removed
		r.Status = StatusUnchanged
		if removed {
			r.Status = StatusUpdated
		}
		return r, nil
	}
	t, err := parseTargetURL(inv.Flags["target"])
	if err != nil {
		return r, err
	}
	r.Store.Target = &Target{
		ID:     t.ID,
		URL:    t.URL,
		Forge:  t.Forge,
		Host:   t.Host,
		Repo:   t.Repo,
		Number: t.Number,
	}
	// A target-scoped reset needs the store open; with no store file
	// there is nothing to reset — report unchanged without creating one.
	// A stat failure (e.g. EACCES) counts as existing and surfaces
	// through the open path's own error mapping.
	if !store.Exists(dir) {
		r.Status = StatusUnchanged
		return r, nil
	}
	// Only sidecar files remain: a partial reset left orphans. The main
	// database is gone, so no target scope can be resolved — remove the
	// orphaned files whole, as a full reset would.
	if _, err := os.Stat(filepath.Join(dir, store.DBFilename)); err != nil && os.IsNotExist(err) {
		removed, err := store.Reset(dir)
		if err != nil {
			return r, mapStoreError(err)
		}
		r.Store.Removed = removed
		r.Status = StatusUnchanged
		if removed {
			r.Status = StatusUpdated
		}
		return r, nil
	}
	clk := newClock()
	st, err := openStore(inv, clk)
	if err != nil {
		return r, err
	}
	defer st.Close()
	removed, err := st.ResetTarget(ctx, t.ID)
	if err != nil {
		return r, mapStoreError(err)
	}
	r.Store.Removed = removed
	r.Status = StatusUnchanged
	if removed {
		r.Status = StatusUpdated
	}
	return r, nil
}
