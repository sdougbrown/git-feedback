package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// TestFreshStoreGetsDataVersion verifies that a store created by this binary
// is stamped with the supported data version and opens cleanly.
func TestFreshStoreGetsDataVersion(t *testing.T) {
	st := openTestStore(t)
	var version int
	if err := st.db.QueryRow(`SELECT version FROM data_version`).Scan(&version); err != nil {
		t.Fatalf("read data_version: %v", err)
	}
	if version != SupportedDataVersion {
		t.Fatalf("data version = %d, want %d", version, SupportedDataVersion)
	}
}

// TestRefuseLegacyStoreWithoutDataVersion verifies that a store created
// before data versioning (no data_version row) is refused as data version 1,
// with a message that names the store path and the remedy, and that no
// re-baseline happens: the store stays refused on every open.
func TestRefuseLegacyStoreWithoutDataVersion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM data_version`); err != nil {
		t.Fatalf("clear data_version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(dir, Options{})
	if err == nil {
		t.Fatal("open succeeded on legacy store, want refusal")
	}
	se, ok := err.(*Error)
	if !ok || se.Code != CodeStoreDataOlder {
		t.Fatalf("error = %v, want code %s", err, CodeStoreDataOlder)
	}
	wantPath := filepath.Join(dir, DBFilename)
	if !strings.Contains(se.Message, wantPath) {
		t.Errorf("message %q does not name the store path %q", se.Message, wantPath)
	}
	if !strings.Contains(se.Message, "store reset") {
		t.Errorf("message %q does not name the remedy", se.Message)
	}
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("second open succeeded on legacy store, want refusal (no re-baseline)")
	}
}

// TestRefuseDataVersionMismatches verifies that an explicit older data
// version is refused as store_data_older and a newer one as store_newer,
// both naming the store path.
func TestRefuseDataVersionMismatches(t *testing.T) {
	t.Run("older explicit version", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		st, err := Open(dir, Options{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := st.db.Exec(`UPDATE data_version SET version = ?`, SupportedDataVersion-1); err != nil {
			t.Fatalf("set data version: %v", err)
		}
		st.Close()

		_, err = Open(dir, Options{})
		if err == nil {
			t.Fatal("open succeeded on older data version, want refusal")
		}
		se, ok := err.(*Error)
		if !ok || se.Code != CodeStoreDataOlder {
			t.Fatalf("error = %v, want code %s", err, CodeStoreDataOlder)
		}
		if !strings.Contains(se.Message, filepath.Join(dir, DBFilename)) {
			t.Errorf("message %q does not name the store path", se.Message)
		}
	})
	t.Run("newer data version", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		st, err := Open(dir, Options{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := st.db.Exec(`UPDATE data_version SET version = ?`, 9999); err != nil {
			t.Fatalf("set data version: %v", err)
		}
		st.Close()

		_, err = Open(dir, Options{})
		if err == nil {
			t.Fatal("open succeeded on newer data version, want refusal")
		}
		se, ok := err.(*Error)
		if !ok || se.Code != CodeStoreNewer {
			t.Fatalf("error = %v, want code %s", err, CodeStoreNewer)
		}
		if !strings.Contains(se.Message, filepath.Join(dir, DBFilename)) {
			t.Errorf("message %q does not name the store path", se.Message)
		}
	})
}

// TestResetRemovesDatabaseFiles verifies that Reset deletes the database
// files and reports whether anything existed, and that the store reopens
// fresh with the supported data version afterwards.
func TestResetRemovesDatabaseFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A process killed mid-transaction can leave non-empty WAL sidecars
	// behind; Reset must remove those too.
	for _, name := range []string{DBFilename + "-wal", DBFilename + "-shm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	removed, err := Reset(dir)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !removed {
		t.Error("reset reported nothing removed for an existing store")
	}
	for _, name := range []string{DBFilename, DBFilename + "-wal", DBFilename + "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still present after reset", name)
		}
	}

	// A second reset has nothing to remove.
	removed, err = Reset(dir)
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if removed {
		t.Error("second reset reported removal with no store present")
	}

	// The store reopens fresh with the supported data version.
	st2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after reset: %v", err)
	}
	defer st2.Close()
	var version int
	if err := st2.db.QueryRow(`SELECT version FROM data_version`).Scan(&version); err != nil {
		t.Fatalf("read data_version after reset: %v", err)
	}
	if version != SupportedDataVersion {
		t.Fatalf("data version after reset = %d, want %d", version, SupportedDataVersion)
	}
}

// TestResetTargetScopesToTarget publishes streams for two targets, marks the
// first target's events acknowledged, then resets the first target: its
// rows vanish, the other target's stream survives with its events pending
// for a new consumer, and host-scoped state (leases) survives.
func TestResetTargetScopesToTarget(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	other := forge.NewTarget("github", "github.com", "owner/other", 8, "https://github.com/owner/other/pull/8")
	for _, tgt := range []forge.Target{testTarget(), other} {
		if _, err := st.Publish(ctx, PublishInput{
			Target:  tgt,
			Account: "alice",
			Snapshot: mkSnapshot("head-"+tgt.ID,
				[]forge.Thread{{ID: "t1", Author: "rev", Body: "fix this", Path: "main.go"}},
				nil, nil),
		}); err != nil {
			t.Fatalf("publish %s: %v", tgt.ID, err)
		}
	}
	evs := events(t, st, "alice", "c2")
	if len(evs) == 0 {
		t.Fatal("no events to acknowledge")
	}
	mustAck(t, st, "alice", "c1", evs[0].ID)
	if err := st.PutLease(ctx, "github.com", "alice", "tok", baseTime.Add(time.Hour)); err != nil {
		t.Fatalf("put lease: %v", err)
	}

	otherBefore, err := st.Inbox(ctx, InboxInput{TargetID: other.ID, Account: "alice", Consumer: "c3", Limit: MaxInboxLimit})
	if err != nil {
		t.Fatalf("inbox other before reset: %v", err)
	}
	removed, err := st.ResetTarget(ctx, testTarget().ID)
	if err != nil {
		t.Fatalf("reset target: %v", err)
	}
	if !removed {
		t.Error("reset reported no stream removed for the published target")
	}

	// The reset target's stream is gone; the other target's stream survives.
	if _, ok, err := st.StreamID(ctx, testTarget().ID, "alice"); err != nil || ok {
		t.Fatalf("stream for reset target still present (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := st.StreamID(ctx, other.ID, "alice"); err != nil || !ok {
		t.Fatalf("surviving stream missing (ok=%v, err=%v)", ok, err)
	}
	// The surviving stream's events stay deliverable to a new consumer: the
	// reset removed nothing from it.
	in, err := st.Inbox(ctx, InboxInput{TargetID: other.ID, Account: "alice", Consumer: "c2", Limit: MaxInboxLimit})
	if err != nil {
		t.Fatalf("inbox other: %v", err)
	}
	if len(in.Events) != len(otherBefore.Events) || len(in.Events) == 0 {
		t.Fatalf("pending events on surviving stream = %d, want %d", len(in.Events), len(otherBefore.Events))
	}
	// Host-scoped state survives.
	var token string
	if err := st.db.QueryRow(`SELECT token FROM leases WHERE host = 'github.com' AND account = 'alice'`).Scan(&token); err != nil {
		t.Fatalf("read lease after reset: %v", err)
	}
	if token != "tok" {
		t.Fatalf("lease token = %q, want tok", token)
	}
}
