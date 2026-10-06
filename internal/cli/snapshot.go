package cli

import (
	"context"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// handleSnapshot exports one stored immutable snapshot through the store's
// no-replace export. The requested snapshot ID is never substituted: an
// unknown or cross-stream ID is an error. No authentication or network
// access occurs.
func handleSnapshot(ctx context.Context, inv Invocation) (Result, error) {
	snapshotID := inv.Flags["snapshot"]
	if snapshotID == "" {
		return Result{}, &usageError{"snapshot requires --snapshot <ID>"}
	}
	output := inv.Flags["output"]
	if output == "" {
		return Result{}, &usageError{"snapshot requires --output <FILE>"}
	}
	stateDir, err := stateDir(inv)
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
	exp, err := st.Export(ctx, store.ExportInput{
		TargetID:    t.ID,
		Account:     account,
		SnapshotID:  snapshotID,
		Destination: output,
		StateDir:    stateDir,
	})
	if err != nil {
		return Result{}, mapStoreError(err)
	}
	r.Export = &Export{Path: exp.Path, Digest: exp.Digest, Counts: exp.Counts}
	return r, nil
}
