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

// TestInboxExcludeAuthor filters out events whose author equals the excluded
// canonical login, while author-less (legacy) and other authors' events are
// always delivered.
func TestInboxExcludeAuthor(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// alice authors one thread, one review, one comment; rev authors one of
	// each. The target object is synthetic (author '').
	snap := &forge.Snapshot{
		Head:           "headA",
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime,
		Threads: []forge.Thread{
			{ID: "t1", Author: "alice", Body: "alice thread", Path: "a.go"},
			{ID: "t2", Author: "rev", Body: "rev thread", Path: "b.go"},
		},
		Reviews: []forge.Review{
			{ID: "r1", Author: "alice", Body: "alice review", State: "APPROVED"},
			{ID: "r2", Author: "rev", Body: "rev review", State: "APPROVED"},
		},
		Comments: []forge.Comment{
			{ID: "c1", Author: "alice", Body: "alice comment", CreatedAt: baseTime},
			{ID: "c2", Author: "rev", Body: "rev comment", CreatedAt: baseTime},
		},
	}
	publish(t, st, "alice", snap)

	// 1. Empty ExcludeAuthor returns everything (compatibility).
	all := events(t, st, "alice", "c1")
	// 1 target + 2 threads + 2 reviews + 2 comments = 7
	if len(all) != 7 {
		t.Fatalf("all events = %d (%v), want 7", len(all), eventKinds(all))
	}

	// 2. ExcludeAuthor "alice" filters out alice-authored events.
	res, err := st.Inbox(ctx, InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "c1",
		ExcludeAuthor: "alice", Limit: MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("inbox exclude: %v", err)
	}
	// 1 target + 3 rev-authored (thread, review, comment) = 4
	if len(res.Events) != 4 {
		t.Fatalf("filtered events = %d (%v), want 4", len(res.Events), eventKinds(res.Events))
	}
	for _, ev := range res.Events {
		if ev.Author == "alice" {
			t.Errorf("event %s has author alice, want filtered", ev.ID)
		}
		if ev.ObjectKind != "target" && ev.Author != "rev" {
			t.Errorf("event %s has author %q, want rev or target", ev.ID, ev.Author)
		}
	}
	var hasTarget bool
	for _, ev := range res.Events {
		if ev.ObjectKind == "target" {
			hasTarget = true
		}
	}
	if !hasTarget {
		t.Error("target event missing from filtered result")
	}

	// 3. Pagination with ExcludeAuthor terminates correctly: no duplicates,
	// no misses beyond the filtered ones.
	seen := map[string]bool{}
	var total int
	cursor := ""
	for i := 0; i < 10; i++ {
		page, err := st.Inbox(ctx, InboxInput{
			TargetID: testTarget().ID, Account: "alice", Consumer: "c1",
			ExcludeAuthor: "alice", Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("inbox page %d: %v", i, err)
		}
		for _, ev := range page.Events {
			if seen[ev.ID] {
				t.Errorf("event %s appears on multiple pages", ev.ID)
			}
			seen[ev.ID] = true
			total++
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if total != 4 {
		t.Fatalf("paged events = %d, want 4 (no duplicates, no misses beyond filtered)", total)
	}

	// 4. A legacy row (author '') is delivered under ExcludeAuthor. Simulate
	// a pre-0003 event row by inserting one with an empty author.
	var streamID int64
	if err := st.db.QueryRow(`SELECT stream_id FROM targets WHERE target_id = ?`, testTarget().ID).Scan(&streamID); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	var snapID int64
	if err := st.db.QueryRow(`SELECT id FROM snapshots WHERE stream_id = ?`, streamID).Scan(&snapID); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if _, err := st.db.Exec(
		`INSERT INTO events (stream_id, kind, object_kind, object_id, revision, url, snapshot_id, observed_at, author)
		 VALUES (?, 'revision', 'thread', 'legacy', 1, 'u', ?, '2026-07-01T12:00:00Z', '')`,
		streamID, snapID); err != nil {
		t.Fatalf("insert legacy event: %v", err)
	}
	res, err = st.Inbox(ctx, InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "c1",
		ExcludeAuthor: "alice", Limit: MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("inbox legacy: %v", err)
	}
	var hasLegacy bool
	for _, ev := range res.Events {
		if ev.ObjectID == "legacy" && ev.Author == "" {
			hasLegacy = true
		}
	}
	if !hasLegacy {
		t.Error("legacy event (author '') not delivered under ExcludeAuthor")
	}

	// 5. Author canonicalization: a login stored with different case is
	// still the same account and must filter.
	snap2 := &forge.Snapshot{
		Head:           "headB",
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime.Add(time.Minute),
		Threads:        []forge.Thread{{ID: "t3", Author: "Alice", Body: "alice again", Path: "c.go"}},
	}
	publish(t, st, "alice", snap2)
	var stored string
	if err := st.db.QueryRow(`SELECT author FROM events WHERE object_id = 't3' LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("read t3 author: %v", err)
	}
	if stored != "alice" {
		t.Errorf("stored t3 author = %q, want canonical alice", stored)
	}
	res, err = st.Inbox(ctx, InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "c1",
		ExcludeAuthor: "alice", Limit: MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("inbox t3: %v", err)
	}
	for _, ev := range res.Events {
		if ev.ObjectID == "t3" {
			t.Errorf("event %s for author 'Alice' not filtered under ExcludeAuthor alice", ev.ID)
		}
	}
}
