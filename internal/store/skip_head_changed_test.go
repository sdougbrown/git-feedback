package store

import (
	"context"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// TestInboxSkipHeadChanged filters out target head_changed events under
// SkipHeadChanged; the flag-off default delivers them. Skipped events stay
// pending, so a flagless read still delivers them later.
func TestInboxSkipHeadChanged(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Two publications with different heads and identical feedback: the
	// second emits one target head_changed event on top of the initial
	// observations.
	publish(t, st, "alice", &forge.Snapshot{
		Head:           "headA",
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime,
		Threads:        []forge.Thread{thread("t1", "fix this")},
		Reviews:        []forge.Review{{ID: "r1", Author: "rev", Body: "looks good", State: "APPROVED"}},
	})
	publish(t, st, "alice", &forge.Snapshot{
		Head:           "headB",
		CollectedStart: baseTime.Add(-time.Minute),
		CollectedEnd:   baseTime.Add(time.Minute),
		Threads:        []forge.Thread{thread("t1", "fix this")},
		Reviews:        []forge.Review{{ID: "r1", Author: "rev", Body: "looks good", State: "APPROVED"}},
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
	isHeadChanged := func(ev Event) bool {
		return ev.Kind == KindHeadChanged && ev.ObjectKind == objectKindTarget
	}

	// 1. Flag off (default): every event is delivered — unchanged behavior.
	all := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit})
	// 1 target + t1 + r1 initial + head_changed = 4.
	if len(all.Events) != 4 {
		t.Fatalf("default events = %v, want 4", ids(all.Events))
	}
	var headDelivered bool
	for _, ev := range all.Events {
		if isHeadChanged(ev) {
			headDelivered = true
		}
	}
	if !headDelivered {
		t.Fatal("target head_changed event missing from default delivery")
	}

	// 2. Flag on: the head_changed event is skipped; review and thread
	// events still deliver.
	filtered := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit, SkipHeadChanged: true})
	if len(filtered.Events) != 3 {
		t.Fatalf("filtered events = %v, want 3 (target + thread + review)", ids(filtered.Events))
	}
	for _, ev := range filtered.Events {
		if isHeadChanged(ev) {
			t.Errorf("event %s (head_changed) was delivered under SkipHeadChanged", ev.ID)
		}
	}

	// 3. The skipped event stays pending: a flagless read — the same
	// consumer without the flag — still receives it, and a different
	// consumer sees it untouched by the filtered consumer's read.
	flagless := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit})
	other := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c2", Limit: MaxInboxLimit})
	if n := countWhere(flagless.Events, isHeadChanged); n != 1 {
		t.Fatalf("flagless re-read head_changed events = %d, want 1", n)
	}
	if n := countWhere(other.Events, isHeadChanged); n != 1 {
		t.Fatalf("other consumer's head_changed events = %d, want 1", n)
	}

	// 4. Acknowledging the delivered events does not resurrect the skipped
	// event under the filter, and the flagless read still holds it.
	acked := make([]string, 0, len(filtered.Events))
	for _, ev := range filtered.Events {
		acked = append(acked, ev.ID)
	}
	mustAck(t, st, "alice", "c1", acked...)
	again := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit, SkipHeadChanged: true})
	if len(again.Events) != 0 {
		t.Fatalf("filtered re-read after ack = %v, want none (skipped events are not resurrected)", ids(again.Events))
	}
	flagless = read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c1", Limit: MaxInboxLimit})
	if len(flagless.Events) != 1 || !isHeadChanged(flagless.Events[0]) {
		t.Fatalf("flagless re-read after ack = %v, want only the head_changed event", ids(flagless.Events))
	}

	// 5. Pagination with the filter terminates with no duplicates and no
	// misses beyond the skipped head_changed event.
	seen := map[string]bool{}
	var total int
	cursor := ""
	for i := 0; i < 20; i++ {
		page := read(InboxInput{TargetID: testTarget().ID, Account: "alice", Consumer: "c3", Cursor: cursor, Limit: 2, SkipHeadChanged: true})
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
	if total != 3 {
		t.Fatalf("paged events = %d, want 3 (no duplicates, no misses beyond skipped)", total)
	}
}

// countWhere counts the events matching pred.
func countWhere(evs []Event, pred func(Event) bool) int {
	n := 0
	for _, ev := range evs {
		if pred(ev) {
			n++
		}
	}
	return n
}
