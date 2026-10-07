package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// countRows counts rows in one table for assertions on durable state.
func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// currentSnapshotID reads targets.current_snapshot_id (0 when null).
func currentSnapshotID(t *testing.T, st *Store) int64 {
	t.Helper()
	var id sql.NullInt64
	if err := st.db.QueryRow(`SELECT current_snapshot_id FROM targets`).Scan(&id); err != nil {
		t.Fatalf("read current snapshot: %v", err)
	}
	return id.Int64
}

// eventKinds maps the event list to "kind:object_kind:object_id:rev" strings.
func eventKinds(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind+":"+e.ObjectKind+":"+e.ObjectID+":"+itoa(e.Revision))
	}
	return out
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}

// TestInitialFeedback verifies that the first publication emits
// initial_observation events for every feedback object and the target head,
// all referencing the first snapshot.
func TestInitialFeedback(t *testing.T) {
	st := openTestStore(t)
	res := publish(t, st, "alice", mkSnapshot("headA",
		[]forge.Thread{thread("t1", "fix this")},
		[]forge.Review{{ID: "r1", Author: "rev", Body: "lgtm-ish", State: "CHANGES_REQUESTED"}},
		[]forge.Comment{{ID: "c1", Author: "rev", Body: "note"}}))
	if res.SnapshotID != "s1" || !res.Changed {
		t.Fatalf("result = %+v, want s1 changed", res)
	}
	evs := events(t, st, "alice", "c1")
	want := []string{
		"initial_observation:target:github:github.com:owner/name:7:1",
		"initial_observation:thread:t1:1",
		"initial_observation:review:r1:1",
		"initial_observation:comment:c1:1",
	}
	if got := eventKinds(evs); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for _, e := range evs {
		if e.SnapshotID != "s1" {
			t.Errorf("event %s references snapshot %s, want s1", e.ID, e.SnapshotID)
		}
		if e.URL != testTarget().URL {
			t.Errorf("event %s url = %q", e.ID, e.URL)
		}
		if e.ObservedAt.IsZero() {
			t.Errorf("event %s has zero observed_at", e.ID)
		}
	}
	if n := countRows(t, st, "snapshots"); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	if n := countRows(t, st, "observations"); n != 1 {
		t.Errorf("observations = %d, want 1", n)
	}
	if id := currentSnapshotID(t, st); id != 1 {
		t.Errorf("current_snapshot_id = %d, want 1", id)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestReassertionAfterReply verifies that a reply to a thread is an ordinary
// revision: new snapshot, counter 2, revision event.
func TestReassertionAfterReply(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA",
		[]forge.Thread{thread("t1", "fix this")}, nil, nil))
	reply := thread("t1", "fix this")
	reply.Comments = []forge.ThreadComment{{ID: "tc1", Author: "author", Body: "re-asserting", CreatedAt: baseTime}}
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{reply}, nil, nil))
	evs := events(t, st, "alice", "c1")
	want := []string{
		"initial_observation:target:github:github.com:owner/name:7:1",
		"initial_observation:thread:t1:1",
		"revision:thread:t1:2",
	}
	if got := eventKinds(evs); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if n := countRows(t, st, "snapshots"); n != 2 {
		t.Errorf("snapshots = %d, want 2", n)
	}
}

// TestChangedResolvedThread verifies that a resolution-state change is a
// revision even though only the state changed.
func TestChangedResolvedThread(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA",
		[]forge.Thread{thread("t1", "fix this")}, nil, nil))
	resolved := thread("t1", "fix this")
	resolved.ResolutionState = "RESOLVED"
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{resolved}, nil, nil))
	evs := events(t, st, "alice", "c1")
	want := []string{
		"initial_observation:target:github:github.com:owner/name:7:1",
		"initial_observation:thread:t1:1",
		"revision:thread:t1:2",
	}
	if got := eventKinds(evs); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestUnchangedIdempotence verifies that an unchanged complete collection
// appends only an observations row referencing the existing snapshot.
func TestUnchangedIdempotence(t *testing.T) {
	st := openTestStore(t)
	snap := mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil)
	first := publish(t, st, "alice", snap)
	second := publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	if second.Changed || second.SnapshotID != first.SnapshotID {
		t.Fatalf("second publish = %+v, want unchanged %s", second, first.SnapshotID)
	}
	if got, want := eventKinds(events(t, st, "alice", "c1")), 2; len(got) != want {
		t.Fatalf("events = %v, want %d", got, want)
	}
	if n := countRows(t, st, "snapshots"); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	if n := countRows(t, st, "observations"); n != 2 {
		t.Errorf("observations = %d, want 2", n)
	}
}

// TestContentReversion verifies A→B→A produces three distinct snapshots and
// three revision occurrences for the thread.
func TestContentReversion(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v2")}, nil, nil))
	third := publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil))
	if !third.Changed || third.SnapshotID != "s3" {
		t.Fatalf("third publish = %+v, want new s3 (content hashes cannot dedupe)", third)
	}
	evs := events(t, st, "alice", "c1")
	want := []string{
		"initial_observation:target:github:github.com:owner/name:7:1",
		"initial_observation:thread:t1:1",
		"revision:thread:t1:2",
		"revision:thread:t1:3",
	}
	if got := eventKinds(evs); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if n := countRows(t, st, "snapshots"); n != 3 {
		t.Errorf("snapshots = %d, want 3", n)
	}
}

// TestHeadOnlyChange verifies that a head change with identical feedback
// writes a new snapshot and a target head_changed event, and no object events.
func TestHeadOnlyChange(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headB", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	evs := events(t, st, "alice", "c1")
	want := []string{
		"initial_observation:target:github:github.com:owner/name:7:1",
		"initial_observation:thread:t1:1",
		"head_changed:target:github:github.com:owner/name:7:2",
	}
	if got := eventKinds(evs); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	head := evs[len(evs)-1]
	if head.HeadBefore != "headA" || head.HeadAfter != "headB" {
		t.Fatalf("head event before/after = %q/%q, want headA/headB", head.HeadBefore, head.HeadAfter)
	}
	if n := countRows(t, st, "snapshots"); n != 2 {
		t.Errorf("snapshots = %d, want 2", n)
	}
}

// TestHeadOnlyReversion verifies that a head-only reversion produces three
// snapshots and two head_changed events.
func TestHeadOnlyReversion(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headB", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	evs := events(t, st, "alice", "c1")
	headEvents := 0
	for _, e := range evs {
		if e.Kind == KindHeadChanged {
			headEvents++
		}
	}
	if headEvents != 2 {
		t.Fatalf("head_changed events = %d, want 2 (events: %v)", headEvents, eventKinds(evs))
	}
	if n := countRows(t, st, "snapshots"); n != 3 {
		t.Errorf("snapshots = %d, want 3", n)
	}
}

// TestAbsentIsNotResolved verifies that an object absent from a later complete
// inventory yields not_observed, and its reappearance a new revision.
func TestAbsentIsNotResolved(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	evs := events(t, st, "alice", "c1")
	want := []string{
		"initial_observation:target:github:github.com:owner/name:7:1",
		"initial_observation:thread:t1:1",
		"not_observed:thread:t1:2",
		"revision:thread:t1:3",
	}
	if got := eventKinds(evs); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestFenceValidation verifies publish against a valid, stale, and wrong
// token lease.
func TestFenceValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	fc := clock.NewFake(baseTime)
	st, err := Open(dir, Options{Clock: fc})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.PutLease(context.Background(), "github.com", "alice", "tok", baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("put lease: %v", err)
	}
	fence := Fence{Host: "github.com", Account: "alice", Token: "tok"}
	if _, err := st.Publish(context.Background(), PublishInput{
		Target: testTarget(), Account: "alice", Snapshot: mkSnapshot("headA", nil, nil, nil), Fence: &fence,
	}); err != nil {
		t.Fatalf("publish with valid fence: %v", err)
	}
	fc.SetTo(baseTime.Add(2 * time.Minute))
	if _, err := st.Publish(context.Background(), PublishInput{
		Target: testTarget(), Account: "alice", Snapshot: mkSnapshot("headB", nil, nil, nil), Fence: &fence,
	}); err == nil {
		t.Fatal("publish with expired fence succeeded, want refusal")
	}
	wrong := Fence{Host: "github.com", Account: "alice", Token: "other"}
	if _, err := st.Publish(context.Background(), PublishInput{
		Target: testTarget(), Account: "alice", Snapshot: mkSnapshot("headC", nil, nil, nil), Fence: &wrong,
	}); err == nil {
		t.Fatal("publish with wrong fence token succeeded, want refusal")
	}
	if n := countRows(t, st, "snapshots"); n != 1 {
		t.Errorf("snapshots = %d, want 1 (only the fenced publication)", n)
	}
}

// TestPublishRollback verifies that a BeforePublishCommit error rolls back
// the entire publication: the previous snapshot survives with no new events
// or observations.
func TestPublishRollback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil))
	st.Hooks.BeforePublishCommit = func() error { return errors.New("boom") }
	_, err = st.Publish(context.Background(), PublishInput{
		Target: testTarget(), Account: "alice",
		Snapshot: mkSnapshot("headA", []forge.Thread{thread("t1", "v2")}, nil, nil),
	})
	if err == nil {
		t.Fatal("publish with failing hook succeeded, want rollback")
	}
	st.Close()

	reopened, err := Open(dir, Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if n := countRows(t, reopened, "snapshots"); n != 1 {
		t.Errorf("snapshots = %d, want 1 (previous snapshot survives)", n)
	}
	if n := countRows(t, reopened, "events"); n != 2 {
		t.Errorf("events = %d, want 2", n)
	}
	if n := countRows(t, reopened, "observations"); n != 1 {
		t.Errorf("observations = %d, want 1", n)
	}
	if id := currentSnapshotID(t, reopened); id != 1 {
		t.Errorf("current_snapshot_id = %d, want 1", id)
	}
}

// TestCrashAfterPublish injects an after-commit panic, recovers from it, and
// confirms the committed events are present in a reopened store.
func TestCrashAfterPublish(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Hooks.AfterPublishCommit = func() { panic("after_commit") }
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_, err = st.Publish(context.Background(), PublishInput{
			Target: testTarget(), Account: "alice",
			Snapshot: mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil),
		})
	}()
	if !panicked {
		t.Fatal("after-commit hook did not panic")
	}
	if err != nil {
		t.Fatalf("publish returned error despite after-commit panic: %v", err)
	}
	st.Close()

	reopened, err := Open(dir, Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	evs := events(t, reopened, "alice", "c1")
	if len(evs) != 2 {
		t.Fatalf("events after crash = %d (%v), want 2", len(evs), eventKinds(evs))
	}
	if id := currentSnapshotID(t, reopened); id != 1 {
		t.Errorf("current_snapshot_id = %d, want 1", id)
	}
}
