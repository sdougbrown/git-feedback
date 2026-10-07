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
// The returned func stops the holder.
func holdBootstrap(e *env) func() {
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
			_ = e.st.AcquireBootstrapLease(context.Background(), testHost, "other", time.Hour, e.clk.Now())
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
