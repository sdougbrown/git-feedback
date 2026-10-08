package tracker

import (
	"testing"
	"time"
)

// TestReconcileReportsAttempt: published results carry the attempt just
// recorded, and deferred results carry the stream's stored latest attempt
// with its original timestamp and next_due.
func TestReconcileReportsAttempt(t *testing.T) {
	e := newEnv(t)
	res1 := e.reconcile(ReconcileInput{URL: testURL})
	if res1.Status != StatusUpdated {
		t.Fatalf("first status = %s", res1.Status)
	}
	a1 := res1.Attempt
	if a1 == nil || !a1.HasNextDue || !a1.Complete || !a1.NextDue.Equal(a1.At.Add(time.Minute)) {
		t.Fatalf("updated attempt = %+v, want complete with next_due = at+1m", a1)
	}

	e.clk.Advance(10 * time.Second)
	res2 := e.reconcile(ReconcileInput{URL: testURL})
	if res2.Status != StatusDeferred {
		t.Fatalf("second status = %s, want deferred", res2.Status)
	}
	if a := res2.Attempt; a == nil || !a.At.Equal(a1.At) || !a.NextDue.Equal(a1.NextDue) {
		t.Fatalf("deferred attempt = %+v, want stored attempt %+v", a, a1)
	}

	e.clk.Advance(61 * time.Second)
	res3 := e.reconcile(ReconcileInput{URL: testURL})
	if res3.Status != StatusUnchanged {
		t.Fatalf("third status = %s, want unchanged", res3.Status)
	}
	if a := res3.Attempt; a == nil || !a.At.After(a1.At) || !a.NextDue.Equal(a.At.Add(time.Minute)) {
		t.Fatalf("unchanged attempt = %+v, want a new attempt with next_due = at+1m", a)
	}
}
