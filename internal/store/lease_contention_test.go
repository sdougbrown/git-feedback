package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
)

// A deferred lease-acquire transaction that upgrades read→write fails
// immediately with SQLITE_BUSY when another writer commits after this
// connection's read snapshot began: the busy handler does not apply to
// snapshot invalidation. The lease transaction must therefore take the
// write lock up front (BEGIN IMMEDIATE), so cross-process contention is
// handled by busy_timeout and resolves into the busy classification.
func TestAcquireAccountLeaseSurvivesConcurrentCommit(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Options{Clock: clock.Real{}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// A second connection acts as the competing process.
	other, err := sql.Open("sqlite", dsn(filepath.Join(dir, DBFilename), 0))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	// Hold the write lock with an uncommitted foreign lease row.
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO leases (host, account, token, expiry_ms)
		VALUES ('github.com', 'me', 'foreign', ?)`,
		time.Now().Add(time.Minute).UTC().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	launch := time.Now()
	go func() {
		done <- st.AcquireAccountLease(context.Background(), "github.com", "me", "mine", time.Minute, time.Now())
	}()
	// Give the acquire time to reach BEGIN IMMEDIATE before releasing the
	// lock, so the contention window is actually exercised. If the acquire
	// returned before the commit, it raced past the window and the test
	// proved nothing.
	time.Sleep(100 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if elapsed := time.Since(launch); elapsed < 90*time.Millisecond {
			t.Fatalf("acquire returned after %v without contending on the lock; the test did not exercise the contention window", elapsed)
		}
		var se *Error
		if !errors.As(err, &se) {
			t.Fatalf("want a store error, got %v", err)
		}
		if se.Code != CodeLeaseBusy {
			t.Fatalf("code = %q (%s), want %q: the lease upgrade must survive the concurrent commit",
				se.Code, se.Message, CodeLeaseBusy)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lease acquisition blocked past the busy timeout")
	}
}
