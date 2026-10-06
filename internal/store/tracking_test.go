package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
)

// trackingBase is the fake clock base shared with the other store tests.
var trackingBase = baseTime

func trackingStore(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(trackingBase)
	st, err := Open(filepath.Join(t.TempDir(), "state"), Options{Clock: clk})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, clk
}

// TestTrackingMigrationPreservesInbox builds a populated Stage 3 database by
// applying 0001 directly, then opens it with the store so 0002 applies, and
// asserts outstanding events/acks survive alongside the new tables.
func TestTrackingMigrationPreservesInbox(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, dbFilename)
	db, err := sql.Open("sqlite", dsn(path, 5000))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = 1"); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	ms, _, err := discoverMigrations()
	if err != nil {
		t.Fatalf("discover migrations: %v", err)
	}
	for _, m := range ms {
		if m.version != 1 {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply 0001: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (1)`); err != nil {
		t.Fatalf("record version: %v", err)
	}
	seed := []string{
		`INSERT INTO targets (stream_id, target_id, account, host, target_json)
		 VALUES (1, 'github:github.com:owner/name:7', 'alice', 'github.com', '{}')`,
		`INSERT INTO snapshots (id, stream_id, head, fingerprint, body, collected_start, collected_end)
		 VALUES (1, 1, 'h1', 'fp', '{}', '2026-07-01T11:59:00Z', '2026-07-01T12:00:00Z')`,
		`INSERT INTO events (stream_id, kind, object_kind, object_id, revision, url, snapshot_id, observed_at)
		 VALUES (1, 'initial_observation', 'thread', 't1', 1, 'u', 1, '2026-07-01T12:00:00Z')`,
		`INSERT INTO acks (consumer, stream_id, event_id) VALUES ('c1', 1, 1)`,
		`INSERT INTO leases (host, account, token, expiry_ms) VALUES ('github.com', 'alice', 'tok', 9999999999999)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	st, err := Open(dir, Options{Clock: clock.NewFake(trackingBase)})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer st.Close()

	// The outstanding event survives and stays pending for a new consumer.
	pending, err := st.Inbox(ctx, InboxInput{TargetID: "github:github.com:owner/name:7", Account: "alice", Consumer: "c2"})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(pending.Events) != 1 || pending.Events[0].ID != "e1" {
		t.Fatalf("pending events = %+v, want e1", pending.Events)
	}
	// The existing acknowledgement survives: re-acking is a no-op.
	ack, err := st.Ack(ctx, AckInput{TargetID: "github:github.com:owner/name:7", Account: "alice", Consumer: "c1", EventIDs: []string{"e1"}})
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if ack.Acknowledged != 0 {
		t.Fatalf("ack acknowledged %d, want 0 (ack row must have survived)", ack.Acknowledged)
	}
	// The Stage 4 tables exist and the leases table gained pace_ms.
	for _, tbl := range []string{"attempts", "schedule", "rate_gates", "http_cache"} {
		var n int
		if err := st.db.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatalf("table %s missing after migration: %v", tbl, err)
		}
	}
	if _, err := st.db.Exec(`UPDATE leases SET pace_ms = 123 WHERE host = 'github.com' AND account = 'alice'`); err != nil {
		t.Fatalf("pace_ms column missing: %v", err)
	}
}

// TestAccountIsolation asserts that gates, the HTTP cache, and leases for
// two accounts on one host do not collide.
func TestAccountIsolation(t *testing.T) {
	ctx := context.Background()
	st, clk := trackingStore(t)
	host := "github.com"

	// Gates: a low remaining budget for alice does not block bob.
	gA, gB := st.NewGate(host, "alice"), st.NewGate(host, "bob")
	gA.Record(forge.RateInfo{Resource: github.ResourceREST, Remaining: 5, Limit: 5000, Reset: clk.Now().Add(time.Hour)})
	if err := gA.Check(github.ResourceREST, clk.Now()); err == nil {
		t.Fatal("alice gate should block on remaining below reserve")
	}
	if err := gB.Check(github.ResourceREST, clk.Now()); err != nil {
		t.Fatalf("bob gate should be open: %v", err)
	}

	// Cache: entries are per-account.
	cA, cB := st.NewHTTPCache(host, "alice"), st.NewHTTPCache(host, "bob")
	cA.Put("https://api.github.com/repos/o/n/pulls/7/reviews", github.CacheEntry{ETag: `"e1"`, Body: []byte("[]"), HasLink: true})
	if got, ok := cA.Get("https://api.github.com/repos/o/n/pulls/7/reviews"); !ok || got.ETag != `"e1"` {
		t.Fatalf("alice cache miss: %+v %v", got, ok)
	}
	if _, ok := cB.Get("https://api.github.com/repos/o/n/pulls/7/reviews"); ok {
		t.Fatal("bob must not see alice's cache entry")
	}

	// Leases: bob can collect while alice holds her lease; bootstrap is
	// excluded by any live account lease.
	if err := st.AcquireAccountLease(ctx, host, "alice", "tokA", time.Minute, clk.Now()); err != nil {
		t.Fatalf("alice lease: %v", err)
	}
	if err := st.AcquireBootstrapLease(ctx, host, "tokB", time.Minute, clk.Now()); !isLeaseBusy(err) {
		t.Fatalf("bootstrap must be blocked by live account leases, got %v", err)
	}
	if err := st.AcquireAccountLease(ctx, host, "bob", "tokB", time.Minute, clk.Now()); err != nil {
		t.Fatalf("bob lease must not collide with alice's: %v", err)
	}
	// Secondary backoff is account-wide: alice's secondary row does not block
	// bob's secondary check (host-wide secondary checking is bootstrap-only).
	gA.Backoff(github.ResourceREST, clk.Now().Add(time.Hour))
	if err := gB.Check(github.ResourceSecondary, clk.Now()); err != nil {
		t.Fatalf("bob secondary check must not be blocked by alice's backoff: %v", err)
	}
	if err := st.ReleaseLease(ctx, host, "alice", "tokA"); err != nil {
		t.Fatalf("release alice: %v", err)
	}
	if err := st.ReleaseLease(ctx, host, "bob", "tokB"); err != nil {
		t.Fatalf("release bob: %v", err)
	}
	if err := st.AcquireBootstrapLease(ctx, host, "tokB", time.Minute, clk.Now()); err != nil {
		t.Fatalf("bootstrap lease after releases: %v", err)
	}
}

func isLeaseBusy(err error) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == CodeLeaseBusy
}

// TestBootstrapExcludesAccountCollector asserts the conservative barrier in
// both directions.
func TestBootstrapExcludesAccountCollector(t *testing.T) {
	ctx := context.Background()
	st, clk := trackingStore(t)
	host := "github.com"

	if err := st.AcquireBootstrapLease(ctx, host, "boot", time.Minute, clk.Now()); err != nil {
		t.Fatalf("bootstrap lease: %v", err)
	}
	if err := st.AcquireAccountLease(ctx, host, "alice", "tok", time.Minute, clk.Now()); !isLeaseBusy(err) {
		t.Fatalf("account acquisition must be blocked by live bootstrap, got %v", err)
	}
	if err := st.ReleaseLease(ctx, host, "", "boot"); err != nil {
		t.Fatalf("release bootstrap: %v", err)
	}
	if err := st.AcquireAccountLease(ctx, host, "alice", "tok", time.Minute, clk.Now()); err != nil {
		t.Fatalf("account lease: %v", err)
	}
	if err := st.AcquireBootstrapLease(ctx, host, "boot2", time.Minute, clk.Now()); !isLeaseBusy(err) {
		t.Fatalf("bootstrap acquisition must be blocked by live account lease, got %v", err)
	}
}

// TestExpiredOwnerCannotPublish asserts that a stale fence cannot publish
// once a replacement owner holds the lease.
func TestExpiredOwnerCannotPublish(t *testing.T) {
	ctx := context.Background()
	st, clk := trackingStore(t)
	host := "github.com"
	if err := st.AcquireAccountLease(ctx, host, "alice", "winner", time.Minute, clk.Now()); err != nil {
		t.Fatalf("winner lease: %v", err)
	}
	clk.Advance(2 * time.Minute)
	if err := st.AcquireAccountLease(ctx, host, "alice", "loser", time.Minute, clk.Now()); err != nil {
		t.Fatalf("loser lease: %v", err)
	}
	_, err := st.Publish(ctx, PublishInput{
		Target: testTarget(), Account: "alice", Snapshot: mkSnapshot("h1", nil, nil, nil),
		Fence: &Fence{Host: host, Account: "alice", Token: "winner"},
	})
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeFenceLost {
		t.Fatalf("publish with stale fence = %v, want fence_lost", err)
	}
	if n := countRows(t, st, "snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want 0", n)
	}
}

// TestExpiryAfterFenceCreation asserts that expiry after fence creation
// prevents publication without needing a replacement owner.
func TestExpiryAfterFenceCreation(t *testing.T) {
	ctx := context.Background()
	st, clk := trackingStore(t)
	host := "github.com"
	if err := st.AcquireAccountLease(ctx, host, "alice", "tok", time.Minute, clk.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}
	clk.Advance(2 * time.Minute)
	_, err := st.Publish(ctx, PublishInput{
		Target: testTarget(), Account: "alice", Snapshot: mkSnapshot("h1", nil, nil, nil),
		Fence: &Fence{Host: host, Account: "alice", Token: "tok"},
	})
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeFenceLost {
		t.Fatalf("publish after expiry = %v, want fence_lost", err)
	}
	if n := countRows(t, st, "snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want 0", n)
	}
}

// TestExpiryWhileAwaitingWriteLock holds the publish inside its transaction
// (via the BeforePublishCommit hook) while the fence expires, and asserts
// the post-hook fence recheck rolls the publication back.
func TestExpiryWhileAwaitingWriteLock(t *testing.T) {
	ctx := context.Background()
	st, clk := trackingStore(t)
	host := "github.com"
	if err := st.AcquireAccountLease(ctx, host, "alice", "tok", time.Minute, clk.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}
	st.Hooks.BeforePublishCommit = func() error {
		// Simulate the write lock being held while the lease expires.
		clk.Advance(2 * time.Minute)
		return nil
	}
	_, err := st.Publish(ctx, PublishInput{
		Target: testTarget(), Account: "alice", Snapshot: mkSnapshot("h1", nil, nil, nil),
		Fence: &Fence{Host: host, Account: "alice", Token: "tok"},
	})
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeFenceLost {
		t.Fatalf("publish = %v, want fence_lost after post-hook recheck", err)
	}
	if n := countRows(t, st, "snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want 0 (publication must roll back)", n)
	}
	if n := countRows(t, st, "events"); n != 0 {
		t.Fatalf("events = %d, want 0 (publication must roll back)", n)
	}
}

// TestBackoffSurvivesRestart records a secondary backoff, closes and
// reopens the store, and asserts the gate still blocks.
func TestBackoffSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	clk := clock.NewFake(trackingBase)
	st, err := Open(dir, Options{Clock: clk})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.NewGate("github.com", "alice").Backoff(github.ResourceSecondary, clk.Now().Add(time.Hour))
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(dir, Options{Clock: clk})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	err = st2.NewGate("github.com", "alice").Check(github.ResourceREST, clk.Now())
	var rl *forge.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("rest after restart = %v, want rate limited by persisted secondary backoff", err)
	}
}

// TestRequestSpacingSurvivesLeaseHandoff asserts that the durable pacing
// timestamp survives lease release and reacquisition, so requests stay at
// least one second apart.
func TestRequestSpacingSurvivesLeaseHandoff(t *testing.T) {
	ctx := context.Background()
	st, clk := trackingStore(t)
	host := "github.com"

	p1 := st.NewPacer(host, "alice")
	if err := p1.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if !clk.Now().Equal(trackingBase) {
		t.Fatalf("first wait advanced the clock to %v", clk.Now())
	}

	// Lease handoff: release and reacquire; pacing metadata must persist.
	if err := st.AcquireAccountLease(ctx, host, "alice", "tok", time.Minute, clk.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := st.ReleaseLease(ctx, host, "alice", "tok"); err != nil {
		t.Fatalf("release: %v", err)
	}

	p2 := st.NewPacer(host, "alice")
	if err := p2.Wait(ctx); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if got, want := clk.Now(), trackingBase.Add(time.Second); !got.Equal(want) {
		t.Fatalf("after handoff wait clock = %v, want %v (spacing must persist)", got, want)
	}
}
