package tracker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// holdBootstrap keeps a live foreign bootstrap lease on the host, so every
// wait cycle ends busy before any account is verified: no cycle ever
// carries an account or a session, and the wake stays on the 1s backoff.
// The lease is acquired synchronously before the caller runs Wait, so the
// wait can never win the race and verify/collect/publish first. The
// returned func stops the refresher.
func holdBootstrap(e *env) func() {
	if err := e.st.AcquireBootstrapLease(context.Background(), testHost, "other", time.Hour, e.clk.Now()); err != nil {
		e.t.Fatalf("acquire bootstrap lease: %v", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = e.st.RefreshLease(context.Background(), testHost, "", "other", time.Hour, e.clk.Now())
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop); <-done }
}

// publishBacklog publishes an initial snapshot directly into the shared
// store, as a concurrent collector would, appending pending events for the
// wait's consumer.
func publishBacklog(t *testing.T, e *env) {
	t.Helper()
	target, err := github.ParseTarget(testURL)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if _, err := e.st.Publish(context.Background(), store.PublishInput{
		Target:  target,
		Account: "alice",
		Snapshot: &forge.Snapshot{
			Head:           "h1",
			CollectedStart: baseTime,
			CollectedEnd:   baseTime,
			Threads: []forge.Thread{{
				ID:       "T1",
				Path:     "main.go",
				Comments: []forge.ThreadComment{{ID: "TC1", Author: "rev", Body: "please fix", CreatedAt: baseTime}},
			}},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// runWait runs Wait in a goroutine and returns its outcome; the test fails
// if Wait is still running after ceiling (a hang, not a result).
func runWait(t *testing.T, e *env, in WaitInput, ctx context.Context, ceiling time.Duration) (WaitResult, error) {
	t.Helper()
	type outcome struct {
		res WaitResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := e.eng.Wait(ctx, in)
		ch <- outcome{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-time.After(ceiling):
		t.Fatalf("wait did not return within %v (a cycle, sleep, or store wait outlived the deadline)", ceiling)
		return WaitResult{}, nil
	}
}

// TestWaitDeliversBacklogAppearingMidRun: cycles whose admission defers
// carry no account, so the loop must keep resolving the backlog account
// from the store; once a concurrent collector appends events mid-run, the
// wait exits with those events instead of cycling to the deadline.
func TestWaitDeliversBacklogAppearingMidRun(t *testing.T) {
	e := newEnvAt(t, baseTime)
	stop := holdBootstrap(e)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type outcome struct {
		res WaitResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := e.eng.Wait(ctx, WaitInput{URL: testURL, Consumer: "watcher"})
		ch <- outcome{res, err}
	}()

	// Let the first cycle end busy, then append backlog mid-run and advance
	// the fake clock so the loop keeps cycling without ever verifying an
	// account.
	time.Sleep(20 * time.Millisecond)
	publishBacklog(t, e)

	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		select {
		case o := <-ch:
			if o.err != nil {
				t.Fatalf("wait: %v", o.err)
			}
			if o.res.Status != StatusEvents {
				t.Fatalf("wait status = %s, want events (backlog was appended mid-run)", o.res.Status)
			}
			if len(o.res.Events) == 0 {
				t.Fatal("wait delivered no events")
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("wait never delivered the mid-run backlog (it cycled to the deadline instead)")
		}
		e.clk.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

// TestWaitDeadlineBoundsPacedRequest: a pace row far in the future (as a
// backward clock correction leaves behind) must not hold a cycle past the
// wait deadline; the paced sleep is capped by the remaining deadline and
// the wait returns the timeout result at the deadline.
func TestWaitDeadlineBoundsPacedRequest(t *testing.T) {
	e := newEnvAt(t, baseTime)
	// Real clock here: the fake clock's Sleep advances time instantly and
	// could never reproduce a hanging sleep.
	e.eng.Clock = clock.Real{}
	realSt, err := store.Open(e.dir, store.Options{})
	if err != nil {
		t.Fatalf("open real-clock store: %v", err)
	}
	defer realSt.Close()
	e.eng.Store = realSt

	// Poison the bootstrap scope's pace row an hour into the real future.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dir, "feedback.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open seeder: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO leases (host, account, pace_ms) VALUES ('github.com', '', ?)`,
		time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("seed pace row: %v", err)
	}

	const deadline = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	res, err := runWait(t, e, WaitInput{URL: testURL, Consumer: "watcher"}, ctx, 3*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Status != StatusTimeout {
		t.Fatalf("wait status = %s, want timeout", res.Status)
	}
	if elapsed >= 2*deadline {
		t.Fatalf("wait returned %v after the %v deadline: a paced sleep outlived the deadline", elapsed, deadline)
	}
}

// TestWaitTimeoutAtDeadlineAcrossDeferredCycles: cycles that keep deferring
// on a short gate must not push the wait past the deadline — the timeout
// result fires at (not long after) the deadline, with many cycles in
// between.
func TestWaitTimeoutAtDeadlineAcrossDeferredCycles(t *testing.T) {
	e := newEnvAt(t, baseTime)
	stop := holdBootstrap(e)
	defer stop()

	const deadline = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	type outcome struct {
		res WaitResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := e.eng.Wait(ctx, WaitInput{URL: testURL, Consumer: "watcher"})
		ch <- outcome{res, err}
	}()

	// Drive the fake clock so every one-second busy backoff elapses
	// instantly, producing many cycles before the real deadline. Each
	// advance unblocks exactly one wake, so it counts the cycles that ran.
	bound := time.Now().Add(2 * time.Second)
	cycles := 0
	for {
		select {
		case o := <-ch:
			if o.err != nil {
				t.Fatalf("wait: %v", o.err)
			}
			if o.res.Status != StatusTimeout {
				t.Fatalf("wait status = %s, want timeout", o.res.Status)
			}
			if cycles < 3 {
				t.Fatalf("busy cycles = %d, want >= 3 (the loop must keep cycling until the deadline)", cycles)
			}
			return
		default:
		}
		if time.Now().After(bound) {
			t.Fatal("wait never returned: the deadline did not bound the cycling")
		}
		e.clk.Advance(time.Second)
		cycles++
		time.Sleep(time.Millisecond)
	}
}

// TestWaitExcludeSelf filters out events whose author equals the stream's
// own account by default; ExcludeSelf restores them. The backlog is
// delivered without running a cycle, so no bootstrap lease is needed.
func TestWaitExcludeSelf(t *testing.T) {
	e := newEnvAt(t, baseTime)

	// Publish a backlog where alice (the stream's account) authors one
	// comment and rev authors another.
	target, err := github.ParseTarget(testURL)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if _, err := e.st.Publish(context.Background(), store.PublishInput{
		Target:  target,
		Account: "alice",
		Snapshot: &forge.Snapshot{
			Head:           "h1",
			CollectedStart: baseTime,
			CollectedEnd:   baseTime,
			Comments: []forge.Comment{
				{ID: "C1", Author: "alice", Body: "alice comment", CreatedAt: baseTime},
				{ID: "C2", Author: "rev", Body: "rev comment", CreatedAt: baseTime},
			},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	t.Run("default excludes own-authored", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := runWait(t, e, WaitInput{URL: testURL, Consumer: "watcher"}, ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if res.Status != StatusEvents {
			t.Fatalf("wait status = %s, want events", res.Status)
		}
		// Only rev's comment is delivered; alice's is filtered. The target
		// event (author '') is also delivered.
		if len(res.Events) != 2 {
			t.Fatalf("events = %d, want 2 (target + rev comment)", len(res.Events))
		}
		for _, ev := range res.Events {
			if ev.Author == "alice" {
				t.Errorf("event %s has author alice, want filtered", ev.ID)
			}
		}
	})

	t.Run("ExcludeSelf delivers everything", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := runWait(t, e, WaitInput{URL: testURL, Consumer: "watcher", ExcludeSelf: true}, ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if res.Status != StatusEvents {
			t.Fatalf("wait status = %s, want events", res.Status)
		}
		// Target + alice comment + rev comment = 3.
		if len(res.Events) != 3 {
			t.Fatalf("events = %d, want 3 (target + alice + rev)", len(res.Events))
		}
	})
}

// TestWaitDeliversEventsAppendedByOwnCycle: a cycle whose collection
// publishes a snapshot exits with the appended events instead of sleeping
// on the cadence.
func TestWaitDeliversEventsAppendedByOwnCycle(t *testing.T) {
	e := newEnvAt(t, baseTime)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	res, err := runWait(t, e, WaitInput{URL: testURL, Consumer: "watcher"}, ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("wait took %v; expected delivery right after the publishing cycle", elapsed)
	}
	if res.Status != StatusEvents {
		t.Fatalf("wait status = %s, want events (a cycle appended events)", res.Status)
	}
	if len(res.Events) == 0 {
		t.Fatal("wait delivered no events")
	}
}

// TestWaitHeadPinCheckedBeforeBacklogDelivery: a wait whose --head does not
// match the stored snapshot head must fail with the pinned-head mismatch
// before delivering stored backlog, even when the consumer has pending
// events. The pin is checked against the stored head, so no cycle (and no
// remote request) is needed to surface the mismatch.
func TestWaitHeadPinCheckedBeforeBacklogDelivery(t *testing.T) {
	e := newEnvAt(t, baseTime)
	publishBacklog(t, e) // stored head h1, pending events for the consumer

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := runWait(t, e, WaitInput{URL: testURL, Consumer: "watcher", Head: "deadbeef"}, ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Status != StatusHeadChange {
		t.Fatalf("wait status = %s, want head_changed (the pin must be checked before backlog delivery)", res.Status)
	}
	if res.ObservedHead != "h1" || res.ExpectedHead != "deadbeef" {
		t.Fatalf("heads = %s/%s, want h1/deadbeef", res.ObservedHead, res.ExpectedHead)
	}
	if len(res.Events) != 0 {
		t.Fatalf("events = %d, want 0 (backlog must not be delivered on a pin mismatch)", len(res.Events))
	}
	if res.Snapshot == nil {
		t.Fatal("mismatch result must carry the stored snapshot")
	}
	if n := e.stub.count("head"); n != 0 {
		t.Fatalf("head reads = %d, want 0 (no cycle ran before the pin check)", n)
	}
	if n := e.stub.count("verify"); n != 0 {
		t.Fatalf("verify requests = %d, want 0 (no cycle ran before the pin check)", n)
	}
}

// TestWaitHeadPinMatchesStoredBacklog: a wait whose --head matches the
// stored snapshot head delivers the backlog exactly as before the pin
// check existed.
func TestWaitHeadPinMatchesStoredBacklog(t *testing.T) {
	e := newEnvAt(t, baseTime)
	publishBacklog(t, e) // stored head h1, pending events for the consumer

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := runWait(t, e, WaitInput{URL: testURL, Consumer: "watcher", Head: "h1"}, ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Status != StatusEvents {
		t.Fatalf("wait status = %s, want events (matching pin must deliver the backlog)", res.Status)
	}
	if len(res.Events) == 0 {
		t.Fatal("wait delivered no events")
	}
	if res.ObservedHead != "h1" || res.ExpectedHead != "h1" {
		t.Fatalf("heads = %s/%s, want h1/h1", res.ObservedHead, res.ExpectedHead)
	}
}
