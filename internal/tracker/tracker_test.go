package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

const testURL = "https://github.com/owner/name/pull/7"
const testTargetID = "github:github.com:owner/name:7"
const testHost = "github.com"

// fakeRunner stands in for the gh credential helper.
type fakeRunner struct {
	mu    sync.Mutex
	token string
	calls int
}

func (r *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.token, nil
}

// ghStub is the fake GitHub API server. It counts requests per kind and can
// block the verification response behind a barrier for concurrency tests.
type ghStub struct {
	mu           sync.Mutex
	verify       string
	threads      string
	nested       string
	reviews      string
	comments     string
	heads        []string
	hi           int
	graphqlCount map[string]int
	restCount    map[string]int
	barrier      chan struct{}
}

func newStub() *ghStub {
	return &ghStub{
		verify:       `{"data":{"viewer":{"login":"alice"},"rateLimit":{"remaining":5000,"limit":5000,"resetAt":"2026-02-01T00:00:00Z"}},"errors":null}`,
		threads:      `{"data":{"repository":{"pullRequest":{"reviewThreads":{"totalCount":1,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"T1","isOutdated":false,"isResolved":false,"path":"main.go","comments":{"totalCount":1,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"TC1","body":"please fix","createdAt":"2026-01-01T00:00:00Z","author":{"login":"rev"}}]}}]}}},"rateLimit":{"remaining":4990,"limit":5000,"resetAt":"2026-02-01T00:00:00Z"}},"errors":null}`,
		nested:       `{"data":{"node":{"comments":{"totalCount":0,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}},"rateLimit":{"remaining":4980,"limit":5000,"resetAt":"2026-02-01T00:00:00Z"}},"errors":null}`,
		reviews:      `[]`,
		comments:     `[]`,
		heads:        []string{"h1"},
		graphqlCount: map[string]int{},
		restCount:    map[string]int{},
	}
}

func (s *ghStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graphql":
			body, _ := io.ReadAll(r.Body)
			q := string(body)
			s.mu.Lock()
			var key, resp string
			switch {
			case strings.Contains(q, "PullRequestReviewThread"):
				key, resp = "nested", s.nested
			case strings.Contains(q, "reviewThreads"):
				key, resp = "threads", s.threads
			default:
				key, resp = "verify", s.verify
			}
			s.graphqlCount[key]++
			barrier := s.barrier
			s.mu.Unlock()
			if key == "verify" && barrier != nil {
				<-barrier
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(json.RawMessage(resp))
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			s.mu.Lock()
			s.restCount["reviews"]++
			body, status := s.reviews, http.StatusOK
			s.mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/comments"):
			s.mu.Lock()
			s.restCount["comments"]++
			body := s.comments
			s.mu.Unlock()
			_, _ = w.Write([]byte(body))
		default: // head read: /repos/{repo}/pulls/{n}
			s.mu.Lock()
			sha := s.heads[len(s.heads)-1]
			if s.hi < len(s.heads) {
				sha = s.heads[s.hi]
				s.hi++
			}
			s.restCount["head"]++
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"head":{"sha":"` + sha + `"}}`))
		}
	}
}

// setHeadQueue installs a fresh head sequence consumed by successive head
// reads; the final value repeats.
func (s *ghStub) setHeadQueue(heads ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heads, s.hi = heads, 0
}

func (s *ghStub) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.graphqlCount[kind]; ok {
		return n
	}
	return s.restCount[kind]
}

// env wires one engine against one fake store, clock, and server.
type env struct {
	t    *testing.T
	dir  string
	st   *store.Store
	clk  *clock.Fake
	stub *ghStub
	srv  *httptest.Server
	run  *fakeRunner
	eng  *Engine
}

var baseTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(baseTime)
	dir := filepath.Join(t.TempDir(), "state")
	st, err := store.Open(dir, store.Options{Clock: clk})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	stub := newStub()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	run := &fakeRunner{token: "tok"}
	eng := &Engine{Store: st, Clock: clk, APIBase: srv.URL, Runner: run}
	return &env{t: t, dir: dir, st: st, clk: clk, stub: stub, srv: srv, run: run, eng: eng}
}

// reconcile runs one cycle and fails the test on an unexpected error.
func (e *env) reconcile(in ReconcileInput) Result {
	e.t.Helper()
	res, err := e.eng.Reconcile(context.Background(), in)
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

// countTable reads a raw table count through a separate read-only connection.
func countTable(t *testing.T, dir, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "feedback.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestConcurrentCollectors: two same-stream cycles race; exactly one
// collects and the loser is busy during ownership.
func TestConcurrentCollectors(t *testing.T) {
	e := newEnv(t)
	e.stub.barrier = make(chan struct{})
	type call struct {
		res Result
		err error
	}
	results := make(chan call, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := e.eng.Reconcile(context.Background(), ReconcileInput{URL: testURL})
			results <- call{res, err}
		}()
	}
	var busy, other *call
	deadline := time.After(5 * time.Second)
	for busy == nil {
		select {
		case c := <-results:
			if c.res.Status == StatusBusy {
				busy = &c
			} else {
				other = &c
			}
		case <-deadline:
			t.Fatal("timed out waiting for racing reconciles")
		}
	}
	if busy == nil {
		t.Fatalf("one racer must be busy; got %+v and %+v", busy, other)
	}
	close(e.stub.barrier)
	for other == nil {
		select {
		case c := <-results:
			other = &c
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the winner")
		}
	}
	if other.res.Status != StatusUpdated && other.res.Status != StatusUnchanged {
		t.Fatalf("winner status = %s, want updated/unchanged", other.res.Status)
	}
	if n := countTable(t, e.dir, "snapshots"); n != 1 {
		t.Fatalf("snapshots = %d, want exactly one collection", n)
	}
	if n := e.stub.count("head"); n != 2 {
		t.Fatalf("head reads = %d, want exactly one collection's two reads", n)
	}
}

// TestDueCheckAfterWinnerCompletes: after the winner persists next_due and
// releases, a loser must be deferred with the current snapshot, not collect
// again.
func TestDueCheckAfterWinnerCompletes(t *testing.T) {
	e := newEnv(t)
	res1 := e.reconcile(ReconcileInput{URL: testURL})
	if res1.Status != StatusUpdated {
		t.Fatalf("first status = %s, want updated", res1.Status)
	}
	res2 := e.reconcile(ReconcileInput{URL: testURL})
	if res2.Status != StatusDeferred {
		t.Fatalf("second status = %s, want deferred", res2.Status)
	}
	if res2.Snapshot == nil || res2.Snapshot.ID != "s1" {
		t.Fatalf("deferred snapshot = %+v, want s1", res2.Snapshot)
	}
	if n := e.stub.count("head"); n != 2 {
		t.Fatalf("head reads = %d, want 2 (no second collection)", n)
	}
}

// TestViewerHonorsPersistedGate: a persisted GraphQL gate blocks the viewer
// verification of a fresh caller before any request reaches the server.
func TestViewerHonorsPersistedGate(t *testing.T) {
	e := newEnv(t)
	e.st.NewGate(testHost, "").Backoff(github.ResourceGraphQL, e.clk.Now().Add(time.Hour))
	res := e.reconcile(ReconcileInput{URL: testURL})
	if res.Status != StatusDeferred {
		t.Fatalf("status = %s, want deferred", res.Status)
	}
	if n := e.stub.count("verify"); n != 0 {
		t.Fatalf("verification requests = %d, want 0", n)
	}
}

// TestRestSecondaryBackoffBlocksViewer: a persisted secondary backoff
// recorded from a REST response blocks the GraphQL viewer request too.
func TestRestSecondaryBackoffBlocksViewer(t *testing.T) {
	e := newEnv(t)
	e.st.NewGate(testHost, "alice").Backoff(github.ResourceREST, e.clk.Now().Add(time.Hour))
	res := e.reconcile(ReconcileInput{URL: testURL})
	if res.Status != StatusDeferred {
		t.Fatalf("status = %s, want deferred", res.Status)
	}
	if n := e.stub.count("verify"); n != 0 {
		t.Fatalf("verification requests = %d, want 0", n)
	}
}

// TestBootstrapMismatchDoesNotPublish: an account mismatch must leave only
// admission/gate metadata, never a stream, snapshot, or events.
func TestBootstrapMismatchDoesNotPublish(t *testing.T) {
	e := newEnv(t)
	_, err := e.eng.Reconcile(context.Background(), ReconcileInput{URL: testURL, Account: "bob"})
	if !errors.Is(err, forge.ErrAccountMismatch) {
		t.Fatalf("err = %v, want ErrAccountMismatch", err)
	}
	for _, tbl := range []string{"targets", "snapshots", "events"} {
		if n := countTable(t, e.dir, tbl); n != 0 {
			t.Fatalf("%s = %d, want 0 after account mismatch", tbl, n)
		}
	}
	// Admission was released: a fresh bootstrap lease can be taken.
	if err := e.st.AcquireBootstrapLease(context.Background(), testHost, "x", time.Minute, e.clk.Now()); err != nil {
		t.Fatalf("bootstrap lease after mismatch: %v", err)
	}
}

// TestHeadChangedCountsAsAttempt: head drift after a successful collection
// records a head_changed attempt with cadence and releases the lease,
// leaving the previous snapshot intact.
func TestHeadChangedCountsAsAttempt(t *testing.T) {
	e := newEnv(t)
	res1 := e.reconcile(ReconcileInput{URL: testURL})
	if res1.Status != StatusUpdated {
		t.Fatalf("first status = %s", res1.Status)
	}
	e.clk.Advance(61 * time.Second) // pass the persisted next_due
	e.stub.setHeadQueue("h1", "h2")
	before := e.clk.Now()
	res2 := e.reconcile(ReconcileInput{URL: testURL})
	if res2.Status != StatusHeadChange {
		t.Fatalf("second status = %s, want head_changed", res2.Status)
	}
	if res2.ObservedHead != "h2" || res2.ExpectedHead != "h1" {
		t.Fatalf("heads = %s/%s, want h2/h1", res2.ObservedHead, res2.ExpectedHead)
	}
	att, ok, err := e.st.LatestAttempt(context.Background(), testTargetID, "alice")
	if err != nil || !ok {
		t.Fatalf("latest attempt: %v %v", ok, err)
	}
	if att.Outcome != store.OutcomeHeadChange || !att.HasNextDue {
		t.Fatalf("attempt = %+v, want head_changed with next_due", att)
	}
	if d := att.NextDue.Sub(before); d < 60*time.Second || d > 70*time.Second {
		t.Fatalf("next_due offset = %v, want ~60s", d)
	}
	// The lease was released by the fenced finalization.
	if err := e.st.AcquireAccountLease(context.Background(), testHost, "alice", "next", time.Minute, e.clk.Now()); err != nil {
		t.Fatalf("lease after head_changed: %v", err)
	}
	if n := countTable(t, e.dir, "snapshots"); n != 1 {
		t.Fatalf("snapshots = %d, want 1 (previous snapshot intact)", n)
	}
}

// TestFailedReadKeepsSnapshot: a malformed page fails the attempt, records
// it, releases the lease, and leaves the previous snapshot and events
// intact.
func TestFailedReadKeepsSnapshot(t *testing.T) {
	e := newEnv(t)
	res1 := e.reconcile(ReconcileInput{URL: testURL})
	if res1.Status != StatusUpdated {
		t.Fatalf("first status = %s", res1.Status)
	}
	eventsBefore := e.inboxLen()
	e.clk.Advance(61 * time.Second)
	e.stub.reviews = "{"
	res2, err := e.eng.Reconcile(context.Background(), ReconcileInput{URL: testURL})
	if err == nil {
		t.Fatalf("expected an operational error, got %+v", res2)
	}
	if !errors.Is(err, forge.ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if countTable(t, e.dir, "snapshots") != 1 {
		t.Fatal("previous snapshot must survive a failed read")
	}
	if got := e.inboxLen(); got != eventsBefore {
		t.Fatalf("events = %d, want %d (failed read must not change history)", got, eventsBefore)
	}
	att, ok, err := e.st.LatestAttempt(context.Background(), testTargetID, "alice")
	if err != nil || !ok {
		t.Fatalf("latest attempt: %v %v", ok, err)
	}
	if att.Outcome != store.OutcomeFailed || !att.HasErrorCode || att.ErrorCode != "incomplete" {
		t.Fatalf("attempt = %+v, want failed/incomplete", att)
	}
	// Lease released: a fresh acquisition succeeds.
	if err := e.st.AcquireAccountLease(context.Background(), testHost, "alice", "next", time.Minute, e.clk.Now()); err != nil {
		t.Fatalf("lease after failure: %v", err)
	}
}

func (e *env) inboxLen() int {
	res, err := e.st.Inbox(context.Background(), store.InboxInput{TargetID: testTargetID, Account: "alice", Consumer: "probe", Limit: store.MaxInboxLimit})
	if err != nil {
		e.t.Fatalf("inbox: %v", err)
	}
	return len(res.Events)
}

// TestPinnedHeadMismatch: a --head mismatch returns head_changed with both
// SHAs and performs no feedback collection.
func TestPinnedHeadMismatch(t *testing.T) {
	e := newEnv(t)
	res := e.reconcile(ReconcileInput{URL: testURL, Head: "cafecafe"})
	if res.Status != StatusHeadChange {
		t.Fatalf("status = %s, want head_changed", res.Status)
	}
	if res.ObservedHead != "h1" || res.ExpectedHead != "cafecafe" {
		t.Fatalf("heads = %s/%s, want h1/cafecafe", res.ObservedHead, res.ExpectedHead)
	}
	if n := countTable(t, e.dir, "snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want 0 (no collection on pinned mismatch)", n)
	}
	if n := countTable(t, e.dir, "objects"); n != 0 {
		t.Fatalf("objects = %d, want 0 (no collection on pinned mismatch)", n)
	}
	if n := e.stub.count("head"); n != 1 {
		t.Fatalf("head reads = %d, want 1 (head check only)", n)
	}
}

// TestDeferredReturnsCurrentSnapshot: not-due cycles return deferred with
// the current snapshot summary.
func TestDeferredReturnsCurrentSnapshot(t *testing.T) {
	e := newEnv(t)
	res1 := e.reconcile(ReconcileInput{URL: testURL})
	if res1.Status != StatusUpdated {
		t.Fatalf("first status = %s", res1.Status)
	}
	res2 := e.reconcile(ReconcileInput{URL: testURL})
	if res2.Status != StatusDeferred {
		t.Fatalf("second status = %s, want deferred", res2.Status)
	}
	if res2.Snapshot == nil || res2.Snapshot.ID != "s1" {
		t.Fatalf("deferred snapshot = %+v, want s1", res2.Snapshot)
	}
	if res2.Freshness.SnapshotObservedAt.IsZero() {
		t.Fatal("deferred result must carry the snapshot's observed_at")
	}
	// A rate gate also defers: even past next_due, a blocked gate returns
	// the current snapshot without collecting.
	e.clk.Advance(61 * time.Second)
	e.st.NewGate(testHost, "alice").Record(forge.RateInfo{
		Resource: github.ResourceREST, Remaining: 5, Limit: 5000, Reset: e.clk.Now().Add(time.Hour),
	})
	res3 := e.reconcile(ReconcileInput{URL: testURL})
	if res3.Status != StatusDeferred {
		t.Fatalf("gated status = %s, want deferred", res3.Status)
	}
	if res3.Snapshot == nil || res3.Snapshot.ID != "s1" {
		t.Fatalf("gated snapshot = %+v, want s1", res3.Snapshot)
	}
	if n := e.stub.count("head"); n != 2 {
		t.Fatalf("head reads = %d, want 2 (gated cycle must not collect)", n)
	}
}

// storeOpenErr mimics the CLI's statusError wrap around a store open
// failure, so the test exercises the same errors.As path as production.
type storeOpenErr struct {
	code string
	msg  string
	err  error
}

func (e *storeOpenErr) Error() string { return e.msg }
func (e *storeOpenErr) Unwrap() error { return e.err }

// TestWaitStoreOpenErrorPlumbing: a corrupt (or newer-schema) store is not
// transient; wait must surface the store error immediately. A transient
// CodeStore lock-contention error must be retried, not fail fast.
func TestWaitStoreOpenErrorPlumbing(t *testing.T) {
	e := newEnv(t)
	e.eng.Clock = nil // real clock: the retry backoff actually elapses

	for _, code := range []string{store.CodeStoreCorrupt, store.CodeStoreNewer} {
		t.Run(code+" fails fast", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			e.eng.OpenStore = func(busy time.Duration) (*store.Store, error) {
				return nil, &storeOpenErr{code: "store_error", msg: "injected", err: &store.Error{Code: code, Message: "injected open failure"}}
			}
			start := time.Now()
			_, err := e.eng.Wait(ctx, WaitInput{URL: testURL, Consumer: "c"})
			var se *store.Error
			if !errors.As(err, &se) || se.Code != code {
				t.Fatalf("wait: expected %s to fail fast, got %v", code, err)
			}
			if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
				t.Fatalf("wait: took %v, expected immediate failure before the deadline", elapsed)
			}
		})
	}

	t.Run("CodeStore retries", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		attempts := 0
		e.eng.OpenStore = func(busy time.Duration) (*store.Store, error) {
			attempts++
			return nil, &storeOpenErr{code: "store_error", msg: "lock contention", err: &store.Error{Code: store.CodeStore, Message: "lock contention"}}
		}
		_, err := e.eng.Wait(ctx, WaitInput{URL: testURL, Consumer: "c"})
		// Must not fail fast: the store error must not surface directly.
		var se *store.Error
		if errors.As(err, &se) {
			t.Fatalf("wait: transient CodeStore must not fail fast, got %v", err)
		}
		// Must have retried at least twice before the deadline.
		if attempts < 2 {
			t.Fatalf("wait: attempts = %d, want >= 2 (retry must bite)", attempts)
		}
	})
}

// TestEngineDefaultsKeepGatesDurable is a smoke check that the engine's
// default services are the store-backed ones (gates survive a reopen).
func TestEngineDefaultsKeepGatesDurable(t *testing.T) {
	e := newEnv(t)
	e.reconcile(ReconcileInput{URL: testURL})
	// The verification quota must have been adopted to the verified account.
	var remaining int
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dir, "feedback.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT remaining FROM rate_gates WHERE host = ? AND account = 'alice' AND resource = 'graphql'`, testHost).Scan(&remaining); err != nil {
		t.Fatalf("adopted graphql gate missing: %v", err)
	}
	// The collection's threads response refreshed the quota under the
	// verified account's scope.
	if remaining != 4990 {
		t.Fatalf("persisted remaining = %d, want 4990", remaining)
	}
}

// fakeSession is a forge.Session for tests that supply a verified session
// (skipping admission and verification).
type fakeSession struct{ login string }

func (s *fakeSession) Login() string { return s.login }

// incompleteAdapter is a forge.Adapter whose Collect returns a nil error
// with an incomplete result, exercising the adapter-contract-bug exit.
type incompleteAdapter struct{}

func (a *incompleteAdapter) Host() string { return testHost }
func (a *incompleteAdapter) ParseTarget(raw string) (forge.Target, error) {
	return forge.Target{}, errors.New("not used")
}
func (a *incompleteAdapter) Authenticate(ctx context.Context, account string) (forge.Session, error) {
	return nil, errors.New("not used")
}
func (a *incompleteAdapter) Collect(ctx context.Context, s forge.Session, t forge.Target, o forge.CollectOptions) (forge.CollectResult, error) {
	return forge.CollectResult{Complete: false}, nil
}

// TestNilErrorIncompleteReleasesLease: a nil error with an incomplete
// result (an adapter contract bug) releases the lease via fenced
// finalization, so a second Reconcile does not report lease_busy.
func TestNilErrorIncompleteReleasesLease(t *testing.T) {
	e := newEnv(t)
	// Establish the stream (and a baseline snapshot) with a real cycle.
	res1 := e.reconcile(ReconcileInput{URL: testURL})
	if res1.Status != StatusUpdated {
		t.Fatalf("first status = %s, want updated", res1.Status)
	}
	e.clk.Advance(61 * time.Second) // pass the persisted next_due
	// Now substitute a fake adapter that returns (nil, incomplete).
	e.eng.NewAdapter = func(svcs Services) forge.Adapter {
		return &incompleteAdapter{}
	}
	sess := &fakeSession{login: "alice"}
	_, err := e.eng.Reconcile(context.Background(), ReconcileInput{URL: testURL, Session: sess})
	if !errors.Is(err, forge.ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	// The lease was released: a second cycle acquires and defers on the
	// persisted next_due (not lease_busy).
	res := e.reconcile(ReconcileInput{URL: testURL, Session: sess})
	if res.Status == StatusBusy {
		t.Fatalf("second status = %s, want not busy (lease leaked)", res.Status)
	}
	if res.Status != StatusDeferred {
		t.Fatalf("second status = %s, want deferred", res.Status)
	}
}

// TestInjectedGateNoSplitBrain: when collection services are injected, the
// lease acquisition and collection must consult the same gate. An injected
// in-memory gate's backoff is not persisted to the store, so a split-brain
// (lease check on the store gate, collection on the in-memory gate) would
// rate-limit the collection while the lease check admits it. With a single
// source of truth, both use the store gate and the cycle is not deferred.
// The backoff is set on REST only so the GraphQL viewer verification in the
// admit path is not blocked.
func TestInjectedGateNoSplitBrain(t *testing.T) {
	e := newEnv(t)
	memGate := github.NewMemoryGate()
	memGate.Backoff(github.ResourceREST, e.clk.Now().Add(time.Hour))
	e.eng.NewServices = func(host, account string) Services {
		return Services{
			Gate:  memGate,
			Pacer: e.st.NewPacer(host, account),
			Cache: e.st.NewHTTPCache(host, account),
		}
	}
	res, err := e.eng.Reconcile(context.Background(), ReconcileInput{URL: testURL})
	if err != nil {
		t.Fatalf("reconcile: %v (split-brain: collection gated while lease admitted)", err)
	}
	if res.Status != StatusUpdated && res.Status != StatusUnchanged {
		t.Fatalf("status = %s, want updated/unchanged", res.Status)
	}
}
