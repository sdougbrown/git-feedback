package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
	graphql       map[string][]string // "threads" | "nested" | "other"
	rest          map[string][]restPageSpec
	heads         []string // head SHAs served in sequence; the last repeats
	restServed    int      // counts unconditional REST page bodies served
	nestedServed  int      // counts nested comment page bodies served
	threadsServed int      // counts outer thread page bodies served
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
			if key == "nested" {
				s.nestedServed++
			}
			if key == "threads" {
				s.threadsServed++
			}
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

func TestThreadActionFields(t *testing.T) {
	stub := baseStub(t)
	standardThreads(t, stub)
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	byID := map[string]forge.Thread{}
	for _, th := range inv.Snapshot.Threads {
		byID[th.ID] = th
	}

	live := byID["T1"]
	if live.Line == nil || *live.Line != 11 || live.OriginalLine == nil || *live.OriginalLine != 11 {
		t.Fatalf("T1 line fields wrong: %v %v", live.Line, live.OriginalLine)
	}
	if live.StartLine != nil || live.OriginalStartLine != nil {
		t.Fatalf("T1 start lines should be nil: %v %v", live.StartLine, live.OriginalStartLine)
	}
	if live.RootCommentID != "C100" || live.RootCommentDatabaseID != 5100 ||
		live.URL != "https://github.com/o/r/pull/1#discussion_r5100" {
		t.Fatalf("T1 root comment identifiers wrong: %+v", live)
	}
	if live.CommitOID != "c000000000000000000000000000000000000001" ||
		live.OriginalCommitOID != "a000000000000000000000000000000000000001" {
		t.Fatalf("T1 commit OIDs wrong: %q %q", live.CommitOID, live.OriginalCommitOID)
	}
	reply := live.Comments[0]
	if reply.ID != "C101" || reply.DatabaseID != 5101 || reply.URL != "https://github.com/o/r/pull/1#discussion_r5101" ||
		reply.CommitOID != live.CommitOID || reply.OriginalCommitOID != live.OriginalCommitOID {
		t.Fatalf("T1 reply fields wrong: %+v", reply)
	}

	// Outdated threads carry no current line and no current commit.
	outdated := byID["T0"]
	if outdated.Line != nil || outdated.StartLine != nil {
		t.Fatalf("T0 current lines should be nil: %v %v", outdated.Line, outdated.StartLine)
	}
	if outdated.OriginalLine == nil || *outdated.OriginalLine != 10 || outdated.OriginalStartLine == nil || *outdated.OriginalStartLine != 8 {
		t.Fatalf("T0 original lines wrong: %v %v", outdated.OriginalLine, outdated.OriginalStartLine)
	}
	if outdated.CommitOID != "" || outdated.OriginalCommitOID == "" ||
		outdated.Comments[0].CommitOID != "" || outdated.Comments[0].OriginalCommitOID == "" {
		t.Fatalf("T0 commit OIDs wrong: %+v", outdated)
	}
}

func TestNestedCommentActionFields(t *testing.T) {
	stub := baseStub(t)
	stub.graphql["threads"] = []string{fixture(t, "thread_deep_p1.json")}
	stub.graphql["nested"] = []string{fixture(t, "thread_deep_p2.json")}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	th := inv.Snapshot.Threads[0]
	if th.RootCommentDatabaseID != 5700 {
		t.Fatalf("root database id %d", th.RootCommentDatabaseID)
	}
	paged := th.Comments[100]
	if paged.ID != "C801" || paged.DatabaseID != 5801 || paged.URL == "" ||
		paged.CommitOID == "" || paged.OriginalCommitOID == "" {
		t.Fatalf("deep-paginated reply missing fields: %+v", paged)
	}
	if paged.Author != "bob[bot]" {
		t.Fatalf("deep-paginated Bot author should carry the REST [bot] suffix, got %q", paged.Author)
	}
}

func TestBotAuthorLoginMatchesREST(t *testing.T) {
	stub := baseStub(t)
	stub.graphql["threads"] = []string{fixture(t, "threads_bot.json")}
	// REST spells the GitHub App with the [bot] suffix it keeps in user.login.
	stub.rest["reviews"] = []restPageSpec{{body: `[{"id":1,"user":{"login":"umpire-bot[bot]"},"body":"review body","state":"APPROVED","commit_id":"abc"}]`}}
	stub.rest["comments"] = []restPageSpec{{body: "[]"}}
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	byID := map[string]forge.Thread{}
	for _, th := range inv.Snapshot.Threads {
		byID[th.ID] = th
	}
	// Bot root author gains the suffix; User reply stays bare.
	bot := byID["TB1"]
	if bot.Author != "umpire-bot[bot]" {
		t.Fatalf("Bot thread author should be umpire-bot[bot], got %q", bot.Author)
	}
	if len(bot.Comments) != 1 || bot.Comments[0].Author != "alice" {
		t.Fatalf("User reply should stay bare, got %+v", bot.Comments)
	}
	// User root author stays bare; Bot reply gains the suffix.
	user := byID["TU1"]
	if user.Author != "alice" {
		t.Fatalf("User thread author should stay alice, got %q", user.Author)
	}
	if len(user.Comments) != 1 || user.Comments[0].Author != "dep-bot[bot]" {
		t.Fatalf("Bot reply should be dep-bot[bot], got %+v", user.Comments)
	}
	// GraphQL and REST spell the same GitHub App identically.
	if len(inv.Snapshot.Reviews) != 1 || inv.Snapshot.Reviews[0].Author != bot.Author {
		t.Fatalf("review author %v should match thread author %q", inv.Snapshot.Reviews, bot.Author)
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

func TestRepeatedNestedCursorFails(t *testing.T) {
	// The nested comments page repeats its endCursor with non-empty nodes
	// and hasNextPage true. Without a seen-cursor guard this loops
	// forever; the deadline bounds the test so a regression fails fast.
	stub := baseStub(t)
	stub.graphql["threads"] = []string{`{"data": {"repository": {"pullRequest": {"reviewThreads": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "T7", "isOutdated": false, "isResolved": false, "path": "a.go", "comments": {"totalCount": 2, "pageInfo": {"hasNextPage": true, "endCursor": "nc1"}, "nodes": [{"id": "C1", "body": "root", "createdAt": "2024-01-01T00:00:00Z", "author": {"login": "alice"}}]}}]}}}, "rateLimit": {"remaining": 3990, "limit": 5000, "resetAt": "2030-01-01T00:00:00Z"}}, "errors": null}`}
	stub.graphql["nested"] = []string{`{"data": {"node": {"comments": {"totalCount": 2, "pageInfo": {"hasNextPage": true, "endCursor": "nc1"}, "nodes": [{"id": "C2", "body": "reply", "createdAt": "2024-01-01T00:00:00Z", "author": {"login": "bob"}}]}}, "rateLimit": {"remaining": 3989, "limit": 5000, "resetAt": "2030-01-01T00:00:00Z"}}, "errors": null}`}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := adapter.collect(ctx, sess.tp, testTarget(t), forge.CollectOptions{})
	if !errors.Is(err, forge.ErrIncomplete) {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if stub.nestedServed > 2 {
		t.Fatalf("nested pagination did not terminate: %d nested pages served", stub.nestedServed)
	}
}

func TestRepeatedOuterCursorFails(t *testing.T) {
	// The outer threads page repeats its endCursor with non-empty nodes
	// and hasNextPage true. Without a seen-cursor guard this loops
	// forever; the deadline bounds the test so a regression fails fast.
	stub := baseStub(t)
	stub.graphql["threads"] = []string{`{"data": {"repository": {"pullRequest": {"reviewThreads": {"totalCount": 1, "pageInfo": {"hasNextPage": true, "endCursor": "tc1"}, "nodes": [{"id": "T7", "isOutdated": false, "isResolved": false, "path": "a.go", "comments": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "C1", "body": "root", "createdAt": "2024-01-01T00:00:00Z", "author": {"login": "alice"}}]}}]}}}, "rateLimit": {"remaining": 3990, "limit": 5000, "resetAt": "2030-01-01T00:00:00Z"}}, "errors": null}`}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := adapter.collect(ctx, sess.tp, testTarget(t), forge.CollectOptions{})
	if !errors.Is(err, forge.ErrIncomplete) {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if stub.threadsServed > 2 {
		t.Fatalf("outer pagination did not terminate: %d thread pages served", stub.threadsServed)
	}
}

func TestMissingOuterCursorFails(t *testing.T) {
	// hasNextPage true with a null endCursor: the outer guard must fail
	// the attempt instead of looping on an empty cursor.
	stub := baseStub(t)
	stub.graphql["threads"] = []string{`{"data": {"repository": {"pullRequest": {"reviewThreads": {"totalCount": 1, "pageInfo": {"hasNextPage": true, "endCursor": null}, "nodes": [{"id": "T7", "isOutdated": false, "isResolved": false, "path": "a.go", "comments": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "C1", "body": "root", "createdAt": "2024-01-01T00:00:00Z", "author": {"login": "alice"}}]}}]}}}, "rateLimit": {"remaining": 3990, "limit": 5000, "resetAt": "2030-01-01T00:00:00Z"}}, "errors": null}`}
	standardREST(stub)
	adapter, sess := newCollectEnv(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := adapter.collect(ctx, sess.tp, testTarget(t), forge.CollectOptions{})
	if !errors.Is(err, forge.ErrIncomplete) {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if stub.threadsServed > 2 {
		t.Fatalf("outer pagination did not terminate: %d thread pages served", stub.threadsServed)
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

func TestAbsoluteNextLinkSelf(t *testing.T) {
	// GitHub returns ABSOLUTE rel="next" Link targets. The transport must
	// follow an absolute next URL that points back at the API host.
	stub := baseStub(t)
	standardThreads(t, stub)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	adapter := NewAdapter(srv.URL, recordingRunner{token: "tok"}, NewMemoryAdmission(), NewMemoryGate(), NewFixedPacer(clk, time.Second), NewMemoryCache(), clk)
	sess, err := adapter.Authenticate(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	stub.rest["reviews"] = []restPageSpec{{body: "[]"}}
	stub.rest["comments"] = []restPageSpec{
		{body: `[{"id":1,"user":{"login":"bob"},"body":"first","created_at":"2024-01-01T00:00:00Z"}]`, link: `<` + srv.URL + `/repos/o/r/issues/7/comments?page=2>; rel="next"`},
		{body: `[{"id":2,"user":{"login":"eve"},"body":"second","created_at":"2024-01-02T00:00:00Z"}]`},
	}
	inv, err := adapter.collect(context.Background(), sess.(*Session).tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect with an absolute self next link: %v", err)
	}
	if len(inv.Snapshot.Comments) != 2 {
		t.Fatalf("want 2 comments across both pages, got %d", len(inv.Snapshot.Comments))
	}
}

func TestNextLinkSemicolonInQuery(t *testing.T) {
	// A rel="next" target whose query value contains a semicolon must
	// still be parsed; otherwise pagination silently terminates and a
	// partial collection is reported as complete.
	stub := baseStub(t)
	standardThreads(t, stub)
	stub.rest["reviews"] = []restPageSpec{{body: "[]"}}
	stub.rest["comments"] = []restPageSpec{
		{body: `[{"id":1,"user":{"login":"bob"},"body":"first","created_at":"2024-01-01T00:00:00Z"}]`, link: `</issues/7/comments?page=2&x=a;b>; rel="next"`},
		{body: `[{"id":2,"user":{"login":"eve"},"body":"second","created_at":"2024-01-02T00:00:00Z"}]`},
	}
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(inv.Snapshot.Comments) != 2 {
		t.Fatalf("want 2 comments (page 2 must be fetched), got %d", len(inv.Snapshot.Comments))
	}
}

func TestNextLinkMalformedNoClosingBracket(t *testing.T) {
	// A part with an opening bracket but no closing bracket carries no
	// target and must be treated as having no next link.
	stub := baseStub(t)
	standardThreads(t, stub)
	stub.rest["reviews"] = []restPageSpec{{body: "[]"}}
	stub.rest["comments"] = []restPageSpec{
		{body: `[{"id":1,"user":{"login":"bob"},"body":"first","created_at":"2024-01-01T00:00:00Z"}]`, link: `</issues/7/comments?page=2; rel="next"`},
	}
	adapter, sess := newCollectEnv(t, stub)

	inv, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(inv.Snapshot.Comments) != 1 {
		t.Fatalf("want 1 comment (malformed link must not paginate), got %d", len(inv.Snapshot.Comments))
	}
}

func TestAbsoluteNextLinkCrossHost(t *testing.T) {
	// An absolute rel="next" target pointing at a different host must fail
	// the attempt rather than yield a smaller inventory.
	stub := baseStub(t)
	standardThreads(t, stub)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	adapter := NewAdapter(srv.URL, recordingRunner{token: "tok"}, NewMemoryAdmission(), NewMemoryGate(), NewFixedPacer(clk, time.Second), NewMemoryCache(), clk)
	sess, err := adapter.Authenticate(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	stub.rest["reviews"] = []restPageSpec{{body: "[]"}}
	stub.rest["comments"] = []restPageSpec{
		{body: `[{"id":1,"user":{"login":"bob"},"body":"first","created_at":"2024-01-01T00:00:00Z"}]`, link: `<https://evil.example/repos/o/r/issues/7/comments?page=2>; rel="next"`},
	}
	result, err := adapter.Collect(context.Background(), sess, testTarget(t), forge.CollectOptions{})
	if err == nil {
		t.Fatal("an absolute next link to a different host must fail the attempt")
	}
	if !strings.Contains(err.Error(), "different host") {
		t.Fatalf("want the cross-host rejection, got %v", err)
	}
	if result.Snapshot != nil || result.Complete {
		t.Fatal("a failed attempt must not publish a (smaller) inventory")
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

// gqlThreadsBody is a single-node threads page whose rateLimit reports the
// given remaining budget. hasNextPage is false so no further GraphQL request
// is issued; the reserve check is the only thing that can abort the attempt.
func gqlThreadsBody(remaining int) string {
	return `{"data": {"repository": {"pullRequest": {"reviewThreads": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "T1", "isOutdated": false, "isResolved": false, "path": "a.go", "comments": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "C1", "body": "root", "createdAt": "2024-01-01T00:00:00Z", "author": {"login": "alice"}}]}}]}}}, "rateLimit": {"remaining": ` + strconv.Itoa(remaining) + `, "limit": 5000, "resetAt": "2030-01-01T00:00:00Z"}}, "errors": null}`
}

func TestNullAuthorTolerated(t *testing.T) {
	stub := baseStub(t)
	// Thread with author: null (deleted account).
	stub.graphql["threads"] = []string{`{"data": {"repository": {"pullRequest": {"reviewThreads": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "T1", "isOutdated": false, "isResolved": false, "path": "a.go", "comments": {"totalCount": 1, "pageInfo": {"hasNextPage": false, "endCursor": null}, "nodes": [{"id": "C1", "body": "root", "createdAt": "2024-01-01T00:00:00Z", "author": null}]}}]}}}, "rateLimit": {"remaining": 3990, "limit": 5000, "resetAt": "2030-01-01T00:00:00Z"}}, "errors": null}`}
	// Review with user: null.
	stub.rest["reviews"] = []restPageSpec{{body: `[{"id":1,"user":null,"body":"review body","state":"APPROVED","commit_id":"abc"}]`}}
	// Comment with user: null.
	stub.rest["comments"] = []restPageSpec{{body: `[{"id":1,"user":null,"body":"comment body","created_at":"2024-01-01T00:00:00Z"}]`}}
	adapter, sess := newCollectEnv(t, stub)

	inv1, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("collect with null authors must not fail: %v", err)
	}
	if len(inv1.Snapshot.Threads) != 1 || len(inv1.Snapshot.Reviews) != 1 || len(inv1.Snapshot.Comments) != 1 {
		t.Fatalf("want 1/1/1, got %d/%d/%d", len(inv1.Snapshot.Threads), len(inv1.Snapshot.Reviews), len(inv1.Snapshot.Comments))
	}
	if inv1.Snapshot.Threads[0].Author != "" {
		t.Fatalf("null thread author should normalize to empty string, got %q", inv1.Snapshot.Threads[0].Author)
	}
	if inv1.Snapshot.Reviews[0].Author != "" {
		t.Fatalf("null review author should normalize to empty string, got %q", inv1.Snapshot.Reviews[0].Author)
	}
	if inv1.Snapshot.Comments[0].Author != "" {
		t.Fatalf("null comment author should normalize to empty string, got %q", inv1.Snapshot.Comments[0].Author)
	}
	// Fingerprint stability: a second collect must produce identical fingerprints.
	inv2, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if len(inv1.Objects) != len(inv2.Objects) {
		t.Fatalf("object count changed between collects: %d → %d", len(inv1.Objects), len(inv2.Objects))
	}
	for i := 1; i < len(inv1.Objects); i++ { // skip synthetic target (no fingerprint)
		if inv1.Objects[i].Fingerprint != inv2.Objects[i].Fingerprint {
			t.Fatalf("fingerprint not stable for %s: %s != %s", inv1.Objects[i].ProviderID, inv1.Objects[i].Fingerprint, inv2.Objects[i].Fingerprint)
		}
	}
}

func TestGQLRateLimitAborts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining int
		wantRate  bool
	}{
		{name: "exhausted", remaining: 0, wantRate: true},
		{name: "below-reserve", remaining: 5, wantRate: true},
		{name: "ample", remaining: 500, wantRate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := baseStub(t)
			stub.graphql["threads"] = []string{gqlThreadsBody(tc.remaining)}
			standardREST(stub)
			adapter, sess := newCollectEnv(t, stub)

			_, err := adapter.collect(context.Background(), sess.tp, testTarget(t), forge.CollectOptions{})
			var rlErr *forge.ErrRateLimited
			if tc.wantRate {
				if !errors.As(err, &rlErr) {
					t.Fatalf("remaining %d: want ErrRateLimited, got %v", tc.remaining, err)
				}
			} else if err != nil {
				t.Fatalf("remaining %d: want no error, got %v", tc.remaining, err)
			}
		})
	}
}
