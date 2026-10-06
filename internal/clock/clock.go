// Package clock provides the time seam used by every component that reads
// time or sleeps. Real code uses Real; tests inject Fake.
package clock

import (
	"sync"
	"time"
)

// Clock abstracts the passage of time.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
}

// Real is the production Clock backed by the standard library.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) Sleep(d time.Duration) { time.Sleep(d) }

func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
}

// Fake is a controllable Clock for tests. Sleep advances the fake clock and
// fires any timers whose deadline has been reached; After returns a channel
// that receives the fake time when Advance moves past its deadline.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFake returns a Fake clock starting at t.
func NewFake(t time.Time) *Fake {
	return &Fake{now: t}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Sleep(d time.Duration) {
	f.Advance(d)
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{deadline: f.now.Add(d), ch: make(chan time.Time, 1)}
	if !t.deadline.After(f.now) {
		t.ch <- t.deadline
	} else {
		f.timers = append(f.timers, t)
	}
	return t.ch
}

// Advance moves the fake clock forward by d, firing due timers.
func (f *Fake) Advance(d time.Duration) {
	f.SetTo(f.now.Add(d))
}

// SetTo moves the fake clock to t, firing timers whose deadline is at or
// before t.
func (f *Fake) SetTo(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		return
	}
	f.now = t
	var pending []*fakeTimer
	for _, tm := range f.timers {
		if !tm.deadline.After(t) {
			tm.ch <- tm.deadline
		} else {
			pending = append(pending, tm)
		}
	}
	f.timers = pending
}
