package store

import (
	"context"
	"testing"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// TestIndependentConsumers verifies per-consumer inboxes and acknowledgements.
func TestIndependentConsumers(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))

	c1, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1"})
	if err != nil {
		t.Fatalf("inbox c1: %v", err)
	}
	c2, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c2"})
	if err != nil {
		t.Fatalf("inbox c2: %v", err)
	}
	if len(c1.Events) != 2 || len(c2.Events) != 2 {
		t.Fatalf("inbox sizes = %d/%d, want 2/2", len(c1.Events), len(c2.Events))
	}
	mustAck(t, st, "alice", "c1", c1.Events[0].ID)
	c1Again, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1"})
	if err != nil {
		t.Fatalf("inbox c1: %v", err)
	}
	c2Again, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c2"})
	if err != nil {
		t.Fatalf("inbox c2: %v", err)
	}
	if len(c1Again.Events) != 1 || len(c2Again.Events) != 2 {
		t.Fatalf("after ack: c1 = %d, c2 = %d, want 1/2", len(c1Again.Events), len(c2Again.Events))
	}
}

// TestInboxDoesNotAck verifies that reading acknowledges nothing.
func TestInboxDoesNotAck(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	for i := 0; i < 2; i++ {
		res, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1"})
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		if len(res.Events) != 2 {
			t.Fatalf("read %d: events = %d, want 2 (reading must not acknowledge)", i+1, len(res.Events))
		}
	}
}

// TestAckOlderRevision verifies that acknowledging an older revision leaves
// later revisions pending, and that re-acking is idempotent.
func TestAckOlderRevision(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v2")}, nil, nil))
	evs := events(t, st, "alice", "c1")
	// evs[1] is the initial thread observation (revision 1), evs[2] the revision.
	if got := eventKinds(evs); got[2] != "revision:thread:t1:2" {
		t.Fatalf("unexpected events: %v", got)
	}
	if res := mustAck(t, st, "alice", "c1", evs[1].ID); res.Acknowledged != 1 {
		t.Fatalf("first ack = %d, want 1", res.Acknowledged)
	}
	remaining := events(t, st, "alice", "c1")
	if len(remaining) != 2 || remaining[1].Kind != KindRevision || remaining[1].Revision != 2 {
		t.Fatalf("remaining = %v, want target initial + revision 2", eventKinds(remaining))
	}
	if res := mustAck(t, st, "alice", "c1", evs[1].ID); res.Acknowledged != 0 {
		t.Fatalf("re-ack = %d, want 0 (idempotent)", res.Acknowledged)
	}
}

// TestAckUnknownIDRejectsAll verifies that one unknown ID rejects the whole
// request, leaving nothing acknowledged.
func TestAckUnknownIDRejectsAll(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	for _, bad := range [][]string{{"e1", "e999"}, {"e1", "junk"}} {
		if _, err := st.Ack(context.Background(), AckInput{
			TargetID: testTarget().ID, Account: "alice", Consumer: "c1", EventIDs: bad,
		}); err == nil {
			t.Fatalf("ack %v succeeded, want rejection", bad)
		}
	}
	// The target has only events e1 (target) and e2 (t1): e2 in another
	// stream is out of scope and must not be ackable here either way; the
	// essential check is that e1 was not acknowledged by any failed request.
	if got := events(t, st, "alice", "c1"); len(got) != 2 {
		t.Fatalf("events after failed acks = %d, want 2 (nothing acknowledged)", len(got))
	}
}

// TestCrossAccountAckRejected verifies that event IDs from another account's
// stream are rejected even though the target URL is identical.
func TestCrossAccountAckRejected(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	publish(t, st, "bob", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))

	aliceEvs := events(t, st, "alice", "c1")
	if _, err := st.Ack(context.Background(), AckInput{
		TargetID: testTarget().ID, Account: "bob", Consumer: "c1", EventIDs: []string{aliceEvs[0].ID, aliceEvs[1].ID},
	}); err == nil {
		t.Fatal("cross-account ack succeeded, want rejection")
	}
	// Nothing from either stream was acknowledged.
	if got := events(t, st, "bob", "c1"); len(got) != 2 {
		t.Fatalf("bob events after rejected ack = %d, want 2", len(got))
	}
	if got := events(t, st, "alice", "c1"); len(got) != 2 {
		t.Fatalf("alice events after rejected ack = %d, want 2", len(got))
	}
}

// TestCrossAccountCursorRejected verifies that a cursor bound to another
// account (or consumer) is rejected before reading events.
func TestCrossAccountCursorRejected(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))
	publish(t, st, "bob", mkSnapshot("headA", []forge.Thread{thread("t1", "fix this")}, nil, nil))

	page, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: 1})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if !page.HasMore || page.NextCursor == "" {
		t.Fatal("expected a paged result with a cursor")
	}
	if _, err := st.Inbox(context.Background(), InboxInput{
		TargetID: testTarget().ID, Account: "bob", Consumer: "c1", Cursor: page.NextCursor,
	}); err == nil {
		t.Fatal("cross-account cursor accepted, want rejection")
	}
	if _, err := st.Inbox(context.Background(), InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "other", Cursor: page.NextCursor,
	}); err == nil {
		t.Fatal("cross-consumer cursor accepted, want rejection")
	}
}

// TestInboxPagination verifies a fixed high-water mark: events appended after
// the sequence started stay pending for the next sequence.
func TestInboxPagination(t *testing.T) {
	st := openTestStore(t)
	// Five events: target initial, t1 initial, t1 revision, t1 not_observed,
	// t1 revision (reappearance).
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v2")}, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v1")}, nil, nil))

	page1, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: 2})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(page1.Events) != 2 || !page1.HasMore || page1.HighWater != 5 {
		t.Fatalf("page1 = %v (hw=%d), want 2 events, more, hw 5", eventKinds(page1.Events), page1.HighWater)
	}
	page2, err := st.Inbox(context.Background(), InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: 2, Cursor: page1.NextCursor,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Events) != 2 || page2.HighWater != 5 {
		t.Fatalf("page2 = %v (hw=%d)", eventKinds(page2.Events), page2.HighWater)
	}
	page3, err := st.Inbox(context.Background(), InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: 2, Cursor: page2.NextCursor,
	})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3.Events) != 1 || page3.HasMore {
		t.Fatalf("page3 = %v, want the last event and no more", eventKinds(page3.Events))
	}

	// A new event after the sequence started is not part of its pages: the
	// same cursor still ends at the fixed high-water mark 5.
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "v2")}, nil, nil))
	bounded, err := st.Inbox(context.Background(), InboxInput{
		TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: 2, Cursor: page2.NextCursor,
	})
	if err != nil {
		t.Fatalf("bounded page: %v", err)
	}
	if len(bounded.Events) != 1 || bounded.Events[0].Seq != 5 {
		t.Fatalf("bounded page = %v, want only event 5 (hw fixed)", eventKinds(bounded.Events))
	}
	fresh, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1"})
	if err != nil {
		t.Fatalf("fresh inbox: %v", err)
	}
	if last := fresh.Events[len(fresh.Events)-1]; len(fresh.Events) != 6 || last.Seq != 6 || last.Kind != KindRevision {
		t.Fatalf("fresh inbox = %v, want 6 events ending with the new revision", eventKinds(fresh.Events))
	}
}

// TestInboxValidation verifies limit and consumer-name validation and the
// explicit unknown-stream error.
func TestInboxValidation(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "bad name!"}); err == nil {
		t.Fatal("invalid consumer name accepted")
	}
	if _, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: 300}); err == nil {
		t.Fatal("limit 300 accepted")
	}
	if _, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("malformed cursor accepted")
	}
	if _, err := st.Inbox(context.Background(), InboxInput{TargetID: testTarget().ID, Account: "nobody", Consumer: "c1"}); err == nil {
		t.Fatal("inbox for unknown stream returned no error")
	} else if se, ok := err.(*Error); !ok || se.Code != CodeUnknownStream {
		t.Fatalf("error = %v, want code %s", err, CodeUnknownStream)
	}
}

// TestSeparateAccountsAreSeparateStreams verifies that two accounts on the
// same target get distinct snapshots and event sequences.
func TestSeparateAccountsAreSeparateStreams(t *testing.T) {
	st := openTestStore(t)
	res := publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "only alice")}, nil, nil))
	_ = res
	// Bob's stream sees no alice feedback.
	publish(t, st, "bob", mkSnapshot("headA", nil, nil, nil))
	bobEvs := events(t, st, "bob", "c1")
	if len(bobEvs) != 1 || bobEvs[0].ObjectKind != objectKindTarget {
		t.Fatalf("bob events = %v, want only the target initial observation", eventKinds(bobEvs))
	}
	if n := countRows(t, st, "snapshots"); n != 2 {
		t.Fatalf("snapshots = %d, want 2 (one per stream)", n)
	}
	// The public target ID never incorporates the account name.
	if testTarget().ID != forge.NewTarget("github", "github.com", "owner/name", 7, "").ID {
		t.Fatal("target ID must be account-independent")
	}
}
