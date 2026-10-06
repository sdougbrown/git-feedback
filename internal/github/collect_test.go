package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// restPageSpec is one REST response in a per-path queue.
type restPageSpec struct {
	body   string
	link   string
	etag   string
	status int
}

// stubServer serves fixture pages in sequence. A conditional request whose
// If-None-Match matches the next page's ETag gets a 304 without consuming
// the page, so tests can model both unchanged and changed representations.
type stubServer struct {
	graphql    map[string][]string // "threads" | "nested" | "other"
	rest       map[string][]restPageSpec
	heads      []string // head SHAs served in sequence; the last repeats
	restServed int      // counts unconditional REST page bodies served
}

func (s *stubServer) handler() http.HandlerFunc {
	gcount := map[string]int{}
	rcount := map[string]int{}
	hi := 0
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graphql":
			body, _ := io.ReadAll(r.Body)
			q := string(body)
			key := "other"
			switch {
			case strings.Contains(q, "PullRequestReviewThread"):
				key = "nested"
			case strings.Contains(q, "reviewThreads"):
				key = "threads"
			}
			pages := s.graphql[key]
			if len(pages) == 0 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if key == "other" {
				w.Write([]byte(pages[0]))
				return
			}
			gi := gcount[key]
			if gi >= len(pages) {
				// Replay the final page so repeat collects terminate.
				gi = len(pages) - 1
			}
			gcount[key] = gi + 1
			w.Write([]byte(pages[gi]))
		case strings.Contains(r.URL.Path, "/reviews") || strings.Contains(r.URL.Path, "/comments"):
			key := restKey(r)
			pages := s.rest[key]
			ri := rcount[key]
			if ri >= len(pages) {
				// Replay the final page so repeat collects terminate.
				ri = len(pages) - 1
			}
			p := pages[ri]
			if r.Header.Get("If-None-Match") != "" && r.Header.Get("If-None-Match") == p.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			rcount[key] = ri + 1
			s.restServed++
			status := p.status
			if status == 0 {
				status = http.StatusOK
			}
			if p.etag != "" {
				w.Header().Set("ETag", p.etag)
			}
			if p.link != "" {
				w.Header().Set("Link", p.link)
			}
			w.WriteHeader(status)
			w.Write([]byte(p.body))
		default: // head read: /repos/{repo}/pulls/{n}
			sha := s.heads[len(s.heads)-1]
			if hi < len(s.heads) {
				sha = s.heads[hi]
				hi++
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"head":{"sha":"` + sha + `"}}`))
		}
	}
}

func restKey(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/reviews") {
		return "reviews"
	}
	return "comments"
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// baseStub returns a stub whose verification query serves verify.json.
func baseStub(t *testing.T) *stubServer {
	return &stubServer{
		graphql: map[string][]string{"other": {fixture(t, "verify.json")}},
		rest:    map[string][]restPageSpec{},
		heads:   []string{"1111111111111111111111111111111111111111"},
	}
}

// newCollectEnv wires an adapter with an authenticated session against the
// stub server, using a fake clock so pacing never blocks.
func newCollectEnv(t *testing.T, stub *stubServer) (*Adapter, *Session) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	adapter := NewAdapter(srv.URL, recordingRunner{token: "tok"}, NewMemoryAdmission(), NewMemoryGate(), NewFixedPacer(clk, time.Second), NewMemoryCache(), clk)
	sess, err := adapter.Authenticate(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return adapter, sess.(*Session)
}

func testTarget(t *testing.T) forge.Target {
	t.Helper()
	target, err := ParseTarget("https://github.com/o/r/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func standardREST(stub *stubServer) {
	stub.rest["reviews"] = []restPageSpec{{body: "[]"}}
	stub.rest["comments"] = []restPageSpec{{body: "[]"}}
}

// standardThreads serves the full 150-thread fixture pagination.
func standardThreads(t *testing.T, stub *stubServer) {
	stub.graphql["threads"] = []string{fixture(t, "threads_p1.json"), fixture(t, "threads_p2.json")}
}

func TestCollectAllPages(t *testing.T) {
	stub := baseStub(t)
	stub.graphql["threads"] = []string{fixture(t, "threads_p1.json"), fixture(t, "threads_p2.json")}
	stub.rest["reviews"] = []restPageSpec{{body: fixture(t, "reviews.json")}}
	stub.rest["comments"] = []restPageSpec{{body: fixture(t, "comments_empty.json")}}
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(inv.Snapshot.Threads) != 150 {
		t.Fatalf("want 150 threads, got %d", len(inv.Snapshot.Threads))
	}
	if len(inv.Snapshot.Reviews) != 1 || len(inv.Snapshot.Comments) != 0 {
		t.Fatalf("unexpected review/comment counts: %d/%d", len(inv.Snapshot.Reviews), len(inv.Snapshot.Comments))
	}
	if inv.Head != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected head %q", inv.Head)
	}
	// Synthetic target object plus one object per feedback item.
	if len(inv.Objects) != 152 {
		t.Fatalf("want 152 objects, got %d", len(inv.Objects))
	}
	if inv.Objects[0].Kind != "target" || inv.Objects[0].ProviderID != inv.Target.ID || inv.Objects[0].Head != inv.Head {
		t.Fatalf("target object wrong: %+v", inv.Objects[0])
	}
	for _, o := range inv.Objects[1:] {
		if o.Fingerprint == "" {
			t.Fatalf("object %s missing fingerprint", o.ProviderID)
		}
	}
}

func TestNestedCommentPagination(t *testing.T) {
	stub := baseStub(t)
	stub.graphql["threads"] = []string{fixture(t, "thread_deep_p1.json")}
	stub.graphql["nested"] = []string{fixture(t, "thread_deep_p2.json")}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(inv.Snapshot.Threads) != 1 {
		t.Fatalf("want 1 thread, got %d", len(inv.Snapshot.Threads))
	}
	th := inv.Snapshot.Threads[0]
	// 150 comments total; the root is the thread body, the rest are replies.
	if len(th.Comments) != 149 {
		t.Fatalf("want 149 thread comments, got %d", len(th.Comments))
	}
	if th.Comments[0].ID != "C701" || th.Comments[148].ID != "C849" {
		t.Fatalf("unexpected comment span: %s..%s", th.Comments[0].ID, th.Comments[148].ID)
	}
}

func TestEditedOldReview(t *testing.T) {
	stub := baseStub(t)
	standardThreads(t, stub)
	stub.rest["reviews"] = []restPageSpec{{body: `[{"id":1,"user":{"login":"carol"},"body":"edited body","state":"CHANGES_REQUESTED","commit_id":"oldcommit"}]`}}
	stub.rest["comments"] = []restPageSpec{{body: "[]"}}
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	rv := inv.Snapshot.Reviews[0]
	if rv.CommitID != "oldcommit" || rv.Body != "edited body" {
		t.Fatalf("review normalization wrong: %+v", rv)
	}
	// The fingerprint comes from forge.Fingerprint, which deliberately
	// excludes commit IDs: an edited review keeps its original association
	// without the fingerprint claiming evaluation against that head.
	want := forge.Fingerprint(forge.FingerprintInput{
		ID: rv.ID, Kind: forge.KindReview, Author: rv.Author, Body: rv.Body, ReviewState: rv.State,
	})
	got := ""
	for _, o := range inv.Objects {
		if o.ProviderID == rv.ID && o.Kind == forge.KindReview {
			got = o.Fingerprint
		}
	}
	if got != want {
		t.Fatalf("review fingerprint mismatch: %s != %s", got, want)
	}
}

func TestMalformedPagination(t *testing.T) {
	stub := baseStub(t)
	stub.graphql["threads"] = []string{fixture(t, "threads_p1.json"), "this is not json"}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	_, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if !errors.Is(err, forge.ErrIncomplete) {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
}

func TestGraphQLErrorPreservesFailure(t *testing.T) {
	stub := baseStub(t)
	stub.graphql["threads"] = []string{fixture(t, "graphql_error.json")}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	_, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if !errors.Is(err, forge.ErrIncomplete) || !strings.Contains(err.Error(), "Something is not right") {
		t.Fatalf("want ErrIncomplete preserving the GraphQL failure, got %v", err)
	}
}

func TestHeadDrift(t *testing.T) {
	stub := baseStub(t)
	stub.heads = []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"}
	standardThreads(t, stub)
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	result, err := adapter.Collect(context.Background(), sess, testTarget(t), forge.CollectOptions{})
	if !errors.Is(err, forge.ErrHeadChanged) {
		t.Fatalf("want ErrHeadChanged, got %v", err)
	}
	if result.Snapshot != nil || result.Complete {
		t.Fatal("no publishable snapshot may accompany head drift")
	}
	if result.HeadBefore == "" || result.HeadAfter == "" {
		t.Fatalf("head drift must carry both SHAs: before=%q after=%q", result.HeadBefore, result.HeadAfter)
	}
}

func TestConditionalLaterPageChanged(t *testing.T) {
	stub := baseStub(t)
	standardThreads(t, stub)
	// Two collects, so queue one empty reviews page per collect.
	stub.rest["reviews"] = []restPageSpec{{body: "[]"}, {body: "[]"}}
	link2 := `</issues/7/comments?page=2>; rel="next"`
	stub.rest["comments"] = []restPageSpec{
		// Run 1: two pages, both fetched unconditionally.
		{body: `[{"id":1,"user":{"login":"bob"},"body":"first","created_at":"2024-01-01T00:00:00Z"}]`, link: link2, etag: "e1"},
		{body: `[{"id":2,"user":{"login":"eve"},"body":"original","created_at":"2024-01-02T00:00:00Z"}]`, etag: "e2"},
		// Run 2: page 1 revalidates to 304; page 2 changed and serves new
		// content under a new ETag.
		{body: `[{"id":2,"user":{"login":"eve"},"body":"changed","created_at":"2024-01-02T00:00:00Z"}]`, etag: "e2b"},
	}
	adapter, sess := newCollectEnv(t, stub)

	if _, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{}); err != nil {
		t.Fatalf("first collect: %v", err)
	}
	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	foundChanged := false
	for _, c := range inv.Snapshot.Comments {
		if c.Body == "changed" {
			foundChanged = true
		}
	}
	if !foundChanged {
		t.Fatal("changed later page content was not picked up")
	}
}

func TestMissingCachedBodyRefetches(t *testing.T) {
	stub := baseStub(t)
	standardThreads(t, stub)
	stub.rest["reviews"] = []restPageSpec{{body: fixture(t, "reviews.json"), etag: "e1"}}
	stub.rest["comments"] = []restPageSpec{{body: "[]"}}
	adapter, sess := newCollectEnv(t, stub)

	// Seed a cache entry that has an ETag but no body or pagination metadata:
	// a 304 must be rejected and one unconditional refetch performed.
	adapter.cache.Put(adapter.apiBase+reviewsPath(testTarget(t)), CacheEntry{ETag: "e1"})

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(inv.Snapshot.Reviews) != 1 {
		t.Fatalf("want the refetched review, got %d", len(inv.Snapshot.Reviews))
	}
}

func TestConditionalLastPageCached(t *testing.T) {
	stub := baseStub(t)
	standardThreads(t, stub)
	// Single-page REST responses with ETags but no Link headers (final pages).
	stub.rest["reviews"] = []restPageSpec{{body: `[{"id":1,"user":{"login":"carol"},"body":"review body","state":"APPROVED","commit_id":"abc"}]`, etag: "e1"}}
	stub.rest["comments"] = []restPageSpec{{body: "[]", etag: "e2"}}
	adapter, sess := newCollectEnv(t, stub)

	inv1, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if len(inv1.Snapshot.Reviews) != 1 {
		t.Fatalf("want 1 review, got %d", len(inv1.Snapshot.Reviews))
	}
	servedAfterFirst := stub.restServed

	// Second collect: the final pages should be served from the 304 cache,
	// not refetched unconditionally.
	inv2, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if len(inv2.Snapshot.Reviews) != 1 {
		t.Fatalf("want 1 review from cache, got %d", len(inv2.Snapshot.Reviews))
	}
	if inv2.Snapshot.Reviews[0].Body != "review body" {
		t.Fatalf("cached review body mismatch: %q", inv2.Snapshot.Reviews[0].Body)
	}
	if stub.restServed != servedAfterFirst {
		t.Fatalf("final page was refetched instead of served from 304 cache: served %d → %d", servedAfterFirst, stub.restServed)
	}
}
