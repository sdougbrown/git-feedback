package store

import (
	"context"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// TestInboxSkipEmptyReviews filters out review events whose review, as
// recorded in the event's own snapshot body, is an empty COMMENTED container
// (GitHub's inline-reply containers); the flag-off default delivers them.
func TestInboxSkipEmptyReviews(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// headA: one thread plus four reviews — an empty COMMENTED container, a
	// whitespace-only COMMENTED review, a real COMMENTED review, and an empty
	// APPROVED review (a decision is content even without a body).
	publish(t, st, "alice", &forge.Snapshot{
		Head:           "headA",
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime,
		Threads:        []forge.Thread{thread("t1", "fix this")},
		Reviews: []forge.Review{
			{ID: "r1", Author: "rev", Body: "", State: "COMMENTED"},
			{ID: "r2", Author: "rev", Body: "   ", State: "COMMENTED"},
			{ID: "r3", Author: "rev", Body: "real content", State: "COMMENTED"},
			{ID: "r4", Author: "rev", Body: "", State: "APPROVED"},
		},
	})
	// headB: r1 gains a body (revision, deliverable), empty r5 appears, r3
	// disappears (not_observed, deliverable under the filter).
	publish(t, st, "alice", &forge.Snapshot{
		Head:           "headB",
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime.Add(time.Minute),
		Threads:        []forge.Thread{thread("t1", "fix this")},
		Reviews: []forge.Review{
			{ID: "r1", Author: "rev", Body: "now with content", State: "COMMENTED"},
			{ID: "r2", Author: "rev", Body: "   ", State: "COMMENTED"},
			{ID: "r4", Author: "rev", Body: "", State: "APPROVED"},
			{ID: "r5", Author: "rev", Body: "", State: "COMMENTED"},
		},
	})

	read := func(in InboxInput) InboxResult {
		res, err := st.Inbox(ctx, in)
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		return res
	}
	ids := func(evs []Event) []string {
		out := make([]string, 0, len(evs))
		for _, ev := range evs {
			out = append(out, ev.ObjectKind+":"+ev.ObjectID+":"+ev.Kind)
		}
		return out
	}

	// 1. Flag off (default): every event is delivered — unchanged behavior.
	all := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit})
	// 1 target + t1 + r1–r4 initial + head_changed + r1 revision + r5 initial
	// + r3 not_observed = 10.
	if len(all.Events) != 10 {
		t.Fatalf("default events = %v, want 10", ids(all.Events))
	}

	// 2. Flag on: empty COMMENTED review events are skipped, including a
	// whitespace-only body; decision states and content reviews deliver.
	filtered := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit, SkipEmptyReviews: true})
	// Skipped: r1 initial (empty at headA), r2 initial + revision (whitespace
	// only at both heads), r5 initial. Delivered: target, t1, r3 initial,
	// r4 initial, head_changed, r1 revision (non-empty at headB), r3
	// not_observed = 7.
	if len(filtered.Events) != 7 {
		t.Fatalf("filtered events = %v, want 7", ids(filtered.Events))
	}
	for _, ev := range filtered.Events {
		if ev.ObjectKind == "review" && (ev.ObjectID == "r2" || ev.ObjectID == "r5") {
			t.Errorf("event %s for an empty COMMENTED review was delivered", ev.ID)
		}
	}
	var hasR1Revision, hasR3NotObserved, hasR4Initial bool
	for _, ev := range filtered.Events {
		switch {
		case ev.ObjectID == "r1" && ev.Kind == "revision":
			hasR1Revision = true
		case ev.ObjectID == "r3" && ev.Kind == "not_observed":
			hasR3NotObserved = true
		case ev.ObjectID == "r4" && ev.Kind == "initial_observation":
			hasR4Initial = true
		}
	}
	if !hasR1Revision || !hasR3NotObserved || !hasR4Initial {
		t.Errorf("filtered delivery missing r1 revision (%v), r3 not_observed (%v), or r4 initial (%v)",
			hasR1Revision, hasR3NotObserved, hasR4Initial)
	}

	// 3. The r1 initial event is skipped because its own snapshot (headA)
	// recorded it empty — even though r1 is non-empty in the current
	// snapshot. Its content history is intact: acking a delivered event does
	// not resurrect the skipped initial event.
	var r1InitialPending bool
	for _, ev := range all.Events {
		if ev.ObjectID == "r1" && ev.Kind == "initial_observation" {
			r1InitialPending = true
		}
	}
	if !r1InitialPending {
		t.Fatal("r1 initial event missing from unfiltered delivery")
	}
	mustAck(t, st, "alice", "c1", filtered.Events[0].ID)
	again := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit, SkipEmptyReviews: true})
	if len(again.Events) != len(filtered.Events)-1 {
		t.Fatalf("re-read after ack = %v, want one fewer than %v", ids(again.Events), ids(filtered.Events))
	}

	// 4. Pagination with the filter terminates with no duplicates and no
	// misses beyond the skipped empty-review events.
	seen := map[string]bool{}
	var total int
	cursor := ""
	for i := 0; i < 20; i++ {
		page := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c2", Cursor: cursor, Limit: 2, SkipEmptyReviews: true})
		for _, ev := range page.Events {
			if seen[ev.ID] {
				t.Fatalf("event %s appears on multiple pages", ev.ID)
			}
			seen[ev.ID] = true
			total++
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if total != 7 {
		t.Fatalf("paged events = %d, want 7 (no duplicates, no misses beyond skipped)", total)
	}
}
