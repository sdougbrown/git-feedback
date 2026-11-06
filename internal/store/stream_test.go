package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

func TestStreamCursorRejectsFullReset(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	baseline, err := st.StreamBaseline(ctx, testTarget().ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadStream(ctx, StreamInput{TargetID: testTarget().ID, Account: "bob", Cursor: baseline.Cursor}); err == nil {
		t.Fatal("cursor accepted for a different account")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Reset(dir); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	if _, err := st.ReadStream(ctx, StreamInput{TargetID: testTarget().ID, Account: "alice", Cursor: baseline.Cursor}); err == nil {
		t.Fatal("cursor from reset store accepted")
	}
}

func TestStreamPositionReplayAndReset(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "first")}, nil, nil))
	baseline, err := st.StreamBaseline(ctx, testTarget().ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "second")}, nil, nil))
	input := StreamInput{TargetID: testTarget().ID, Account: "alice", Cursor: baseline.Cursor}
	first, err := st.ReadStream(ctx, input)
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("read = %+v, %v; want one revision", first, err)
	}
	replay, err := st.ReadStream(ctx, input)
	if err != nil || len(replay.Events) != 1 || replay.Events[0].ID != first.Events[0].ID {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	input.Cursor = first.Cursor
	next, err := st.ReadStream(ctx, input)
	if err != nil || len(next.Events) != 0 {
		t.Fatalf("resume = %+v, %v; want no events", next, err)
	}
	if _, err := st.ResetTarget(ctx, testTarget().ID); err != nil {
		t.Fatal(err)
	}
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "second")}, nil, nil))
	if _, err := st.ReadStream(ctx, input); err == nil {
		t.Fatal("cursor from reset stream accepted")
	}
}

func TestStreamPaginationIgnoresAcknowledgements(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	baseline, err := st.StreamBaseline(ctx, testTarget().ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "first"), thread("t2", "second")}, nil, nil))
	for _, ev := range events(t, st, "alice", "consumer") {
		mustAck(t, st, "alice", "consumer", ev.ID)
	}
	input := StreamInput{TargetID: testTarget().ID, Account: "alice", Cursor: baseline.Cursor, Limit: 1}
	first, err := st.ReadStream(ctx, input)
	if err != nil || len(first.Events) != 1 || !first.HasMore {
		t.Fatalf("first = %+v, %v", first, err)
	}
	input.Cursor = first.Cursor
	second, err := st.ReadStream(ctx, input)
	if err != nil || len(second.Events) != 1 || second.Events[0].ID == first.Events[0].ID || second.HasMore {
		t.Fatalf("second = %+v, %v", second, err)
	}
}

func TestStreamFiltersDoNotChangePosition(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	baseline, err := st.StreamBaseline(ctx, testTarget().ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	publish(t, st, "alice", mkSnapshot("headB", []forge.Thread{thread("t1", "review")}, nil, nil))
	input := StreamInput{TargetID: testTarget().ID, Account: "alice", Cursor: baseline.Cursor, SkipHeadChanged: true}
	filtered, err := st.ReadStream(ctx, input)
	if err != nil || len(filtered.Events) != 1 {
		t.Fatalf("filtered = %+v, %v", filtered, err)
	}
	input.Cursor = filtered.Cursor
	input.SkipHeadChanged = false
	resumed, err := st.ReadStream(ctx, input)
	if err != nil || len(resumed.Events) != 0 {
		t.Fatalf("resumed = %+v, %v", resumed, err)
	}
	input.Cursor = baseline.Cursor
	replay, err := st.ReadStream(ctx, input)
	if err != nil || len(replay.Events) != 2 {
		t.Fatalf("unfiltered replay = %+v, %v", replay, err)
	}
}
