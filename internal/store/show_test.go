package store

import (
	"context"
	"errors"
	"testing"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// TestShow: one event's object content is retrievable as recorded in the
// event's snapshot, read-only and independent of acknowledgements.
func TestShow(t *testing.T) {
	st := openTestStore(t)
	snap := mkSnapshot("headA",
		[]forge.Thread{thread("t1", "root body", forge.ThreadComment{ID: "tc1", Author: "reviewer", Body: "nested"})},
		[]forge.Review{{ID: "r1", Author: "reviewer", Body: "marker head=headA", State: "COMMENTED"}},
		[]forge.Comment{{ID: "c1", Author: "reviewer", Body: "top-level"}})
	publish(t, st, "alice", snap)
	evs := events(t, st, "alice", "ci")

	find := func(t *testing.T, objectID string) Event {
		t.Helper()
		for _, ev := range evs {
			if ev.ObjectID == objectID {
				return ev
			}
		}
		t.Fatalf("no event for object %q", objectID)
		return Event{}
	}
	show := func(t *testing.T, id string) ShowResult {
		t.Helper()
		res, err := st.Show(context.Background(), ShowInput{TargetID: testTarget().ID, Account: "alice", EventID: id})
		if err != nil {
			t.Fatalf("show %s: %v", id, err)
		}
		return res
	}

	t.Run("review body and state", func(t *testing.T) {
		res := show(t, find(t, "r1").ID)
		if res.Event.Kind != KindInitialObservation || res.Event.ObjectKind != "review" {
			t.Errorf("event kind/object = %s/%s, want initial_observation/review", res.Event.Kind, res.Event.ObjectKind)
		}
		r, ok := res.Object.(*forge.Review)
		if !ok {
			t.Fatalf("object type %T, want *forge.Review", res.Object)
		}
		if r.Body != "marker head=headA" || r.State != "COMMENTED" || r.Author != "reviewer" {
			t.Errorf("review = %+v", r)
		}
	})

	t.Run("comment body", func(t *testing.T) {
		res := show(t, find(t, "c1").ID)
		c, ok := res.Object.(*forge.Comment)
		if !ok {
			t.Fatalf("object type %T, want *forge.Comment", res.Object)
		}
		if c.Body != "top-level" {
			t.Errorf("comment body = %q", c.Body)
		}
	})

	t.Run("thread with comments", func(t *testing.T) {
		res := show(t, find(t, "t1").ID)
		th, ok := res.Object.(*forge.Thread)
		if !ok {
			t.Fatalf("object type %T, want *forge.Thread", res.Object)
		}
		if th.Body != "root body" || len(th.Comments) != 1 || th.Comments[0].Body != "nested" {
			t.Errorf("thread = %+v", th)
		}
	})

	t.Run("target event has no object", func(t *testing.T) {
		var targetID string
		for _, ev := range evs {
			if ev.ObjectKind == "target" {
				targetID = ev.ID
			}
		}
		if targetID == "" {
			t.Fatal("no target event delivered")
		}
		res := show(t, targetID)
		if res.Object != nil {
			t.Errorf("target event object = %T, want nil", res.Object)
		}
		if res.Event.HeadAfter != "headA" {
			t.Errorf("target event head_after = %q, want headA", res.Event.HeadAfter)
		}
	})

	t.Run("unknown event", func(t *testing.T) {
		_, err := st.Show(context.Background(), ShowInput{TargetID: testTarget().ID, Account: "alice", EventID: "e999"})
		var se *Error
		if !errors.As(err, &se) || se.Code != CodeUnknownEvent {
			t.Fatalf("err = %v, want %s", err, CodeUnknownEvent)
		}
	})

	t.Run("event of another stream is unknown", func(t *testing.T) {
		other := mkSnapshot("headB", nil, nil, nil)
		otherTarget := forge.NewTarget("github", "github.com", "other/repo", 1, "https://github.com/other/repo/pull/1")
		if _, err := st.Publish(context.Background(), PublishInput{Target: otherTarget, Account: "alice", Snapshot: other}); err != nil {
			t.Fatalf("publish other: %v", err)
		}
		_, err := st.Show(context.Background(), ShowInput{TargetID: otherTarget.ID, Account: "alice", EventID: find(t, "r1").ID})
		var se *Error
		if !errors.As(err, &se) || se.Code != CodeUnknownEvent {
			t.Fatalf("err = %v, want %s", err, CodeUnknownEvent)
		}
	})

	t.Run("still retrievable after ack", func(t *testing.T) {
		mustAck(t, st, "alice", "ci", find(t, "r1").ID)
		res := show(t, find(t, "r1").ID)
		if res.Object == nil {
			t.Error("acked event no longer shows its object")
		}
	})
}

// TestShowNotObserved: an event for an object absent from its snapshot body
// (disappearance) shows a nil object with the event record intact. A
// reappearance revision event's snapshot contains the object, so show
// returns the reapplied content.
func TestShowNotObserved(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", nil,
		[]forge.Review{{ID: "r1", Author: "reviewer", Body: "gone soon", State: "COMMENTED"}}, nil))
	publish(t, st, "alice", mkSnapshot("headB", nil, nil, nil))

	var gone Event
	for _, ev := range events(t, st, "alice", "ci") {
		if ev.Kind == KindNotObserved && ev.ObjectID == "r1" {
			gone = ev
		}
	}
	if gone.ID == "" {
		t.Fatal("no not_observed event for r1")
	}
	res, err := st.Show(context.Background(), ShowInput{TargetID: testTarget().ID, Account: "alice", EventID: gone.ID})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if res.Object != nil {
		t.Errorf("not_observed object = %T, want nil", res.Object)
	}
	if res.Event.Kind != KindNotObserved || res.Event.ObjectKind != "review" {
		t.Errorf("event kind/object = %s/%s", res.Event.Kind, res.Event.ObjectKind)
	}
}

// TestShowReappearance: the revision event that records an object's return
// carries the current snapshot, which contains the object, so show returns
// the reapplied content.
func TestShowReappearance(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", nil,
		[]forge.Review{{ID: "r1", Author: "reviewer", Body: "first pass", State: "COMMENTED"}}, nil))
	publish(t, st, "alice", mkSnapshot("headB", nil, nil, nil))
	publish(t, st, "alice", mkSnapshot("headC", nil,
		[]forge.Review{{ID: "r1", Author: "reviewer", Body: "back again", State: "CHANGES_REQUESTED"}}, nil))

	var back Event
	for _, ev := range events(t, st, "alice", "ci") {
		if ev.Kind == KindRevision && ev.ObjectKind == "review" && ev.ObjectID == "r1" {
			back = ev
		}
	}
	if back.ID == "" {
		t.Fatal("no revision event for the reappearance of r1")
	}
	res, err := st.Show(context.Background(), ShowInput{TargetID: testTarget().ID, Account: "alice", EventID: back.ID})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	r, ok := res.Object.(*forge.Review)
	if !ok {
		t.Fatalf("object type %T, want *forge.Review", res.Object)
	}
	if r.Body != "back again" || r.State != "CHANGES_REQUESTED" {
		t.Errorf("review = %+v, want the reapplied content", r)
	}
}
