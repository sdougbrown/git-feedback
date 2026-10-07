package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

var baseTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// openTestStore opens a store in a fresh temporary state directory with a
// fake clock.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state"), Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testTarget is one canonical target for tests.
func testTarget() forge.Target {
	return forge.NewTarget("github", "github.com", "owner/name", 7, "https://github.com/owner/name/pull/7")
}

// mkSnapshot builds a snapshot with the given head and feedback objects.
func mkSnapshot(head string, threads []forge.Thread, reviews []forge.Review, comments []forge.Comment) *forge.Snapshot {
	return &forge.Snapshot{
		Head:           head,
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime,
		Threads:        threads,
		Reviews:        reviews,
		Comments:       comments,
	}
}

// thread is a shorthand thread builder.
func thread(id, body string, comments ...forge.ThreadComment) forge.Thread {
	return forge.Thread{ID: id, Author: "reviewer", Body: body, Path: "main.go", Comments: comments}
}

// publish publishes snap to st for account and fails the test on error.
func publish(t *testing.T, st *Store, account string, snap *forge.Snapshot) PublishResult {
	t.Helper()
	res, err := st.Publish(context.Background(), PublishInput{Target: testTarget(), Account: account, Snapshot: snap})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return res
}

// events lists the events of a stream through an inbox read, which is the
// only public event view.
func events(t *testing.T, st *Store, account, consumer string) []Event {
	t.Helper()
	res, err := st.Inbox(context.Background(), InboxInput{
		TargetID: testTarget().ID, Account: account, Consumer: consumer, Limit: MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	return res.Events
}

// mustAck acknowledges ids or fails the test.
func mustAck(t *testing.T, st *Store, account, consumer string, ids ...string) AckResult {
	t.Helper()
	res, err := st.Ack(context.Background(), AckInput{TargetID: testTarget().ID, Account: account, Consumer: consumer, EventIDs: ids})
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	return res
}

// TestDSNPathEscaping verifies that a state directory whose name contains the
// characters that corrupt the SQLite DSN ('?', '#', and a space) opens and
// round-trips a publication, and that the DSN keeps its three pinned pragmas.
func TestDSNPathEscaping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st?te #dir")
	full := filepath.Join(dir, dbFilename)

	// The DSN must escape the path so the driver's first-'?' split and the
	// SQLite URI decode target the real file, and must keep all three pragmas.
	d := dsn(full, 0)
	if got := strings.Count(d, "_pragma="); got != 3 {
		t.Fatalf("dsn %q has %d pragmas, want 3", d, got)
	}
	// The path portion (before the query separator) must not contain a raw
	// '?' or '#', which would be misread as the query/fragment start.
	pathPart := d
	if i := strings.Index(d, "?"); i >= 0 {
		pathPart = d[:i]
	}
	if strings.ContainsAny(pathPart, "?#") {
		t.Fatalf("dsn path %q contains an unescaped '?' or '#'", pathPart)
	}

	st, err := Open(dir, Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("open store in %q: %v", dir, err)
	}
	defer st.Close()

	// The database must land at the exact expected path. An unescaped '?' or
	// '#' in the path makes the driver/SQLite truncate the path at the first
	// '?' and open a different file, so this is the assertion that catches it.
	if _, statErr := os.Stat(full); statErr != nil {
		t.Fatalf("db file not at expected path %q: %v", full, statErr)
	}

	publish(t, st, "alice", mkSnapshot("headA",
		[]forge.Thread{thread("t1", "fix this")}, nil, nil))
	evs := events(t, st, "alice", "c1")
	if len(evs) != 2 {
		t.Fatalf("events = %d (%v), want 2", len(evs), eventKinds(evs))
	}
}
