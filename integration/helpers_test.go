// Package integration runs the compiled git-feedback binaries as
// subprocesses against a local fake GitHub API and a fake gh. The helpers
// locate the two binaries built by the Makefile targets from the repository
// root; they never build recursively. Tests that need the environment
// overrides use the testhooks-tagged binary; the default-build override
// test uses the production binary.
package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// repoRoot is the repository root, derived from this file's location.
var repoRoot = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate repository root")
	}
	return filepath.Dir(filepath.Dir(file))
}()

// testURL is the canonical pull-request URL every test uses.
const testURL = "https://github.com/owner/name/pull/7"

// binPaths returns the absolute production and testhooks binaries, skipping
// the test when the Makefile build targets have not run.
func binPaths(t *testing.T) (prod, tagged string) {
	t.Helper()
	prod = filepath.Join(repoRoot, "bin", "git-feedback")
	tagged = filepath.Join(repoRoot, "bin", "git-feedback-test")
	for _, p := range []string{prod, tagged} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary %s not built; run make test (or make verify) first", p)
		}
	}
	return prod, tagged
}

// fakeGHPath returns the absolute path to the fake gh credential helper,
// ensuring it is executable.
func fakeGHPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot, "integration", "testdata", "fake-gh")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatalf("chmod fake gh: %v", err)
	}
	return p
}

// ghStub is the fake GitHub API server. It counts requests per endpoint,
// can sequence the observed head, gate verification behind a barrier, hang
// requests, and inject a persisted rate-gate exhaustion.
type ghStub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	login    string
	heads    []string // consumed in order; the last entry repeats
	hi       int
	threads  string
	reviews  string
	comments string
	// rateRemaining, when set below the reserve, makes the fake persist a
	// GraphQL gate exhausted until rateReset.
	rateRemaining int
	rateReset     time.Time
	// barrier, when non-nil, blocks the first verification request until
	// closed.
	barrier chan struct{}
	counts  map[string]int
	log     []stubHit
}

type stubHit struct {
	key string
	at  time.Time
}

func newStub(t *testing.T) *ghStub {
	t.Helper()
	s := &ghStub{
		login:    "alice",
		heads:    []string{"h1"},
		threads:  `{"data":{"repository":{"pullRequest":{"reviewThreads":{"totalCount":1,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"T1","isOutdated":false,"isResolved":false,"path":"main.go","comments":{"totalCount":1,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"TC1","body":"please fix","createdAt":"2026-01-01T00:00:00Z","author":{"login":"rev"}}]}}]}}},"rateLimit":{"remaining":5000,"limit":5000,"resetAt":"2026-02-01T00:00:00Z"}},"errors":null}`,
		reviews:  `[]`,
		comments: `[]`,
		counts:   map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// setHead swaps the head sequence for the next collection.
func (s *ghStub) setHead(sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heads, s.hi = []string{sha}, 0
}

// count returns the request counter for an endpoint key.
func (s *ghStub) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[key]
}

// waitForCount polls until the endpoint reaches n or the deadline expires.
func (s *ghStub) waitForCount(t *testing.T, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.count(key) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("endpoint %q never reached %d requests (at %d)", key, n, s.count(key))
}

func (s *ghStub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/graphql":
		s.serveGraphQL(w, r)
	case strings.HasSuffix(r.URL.Path, "/reviews"):
		s.bump("reviews")
		s.serveREST(w, r, s.reviews)
	case strings.HasSuffix(r.URL.Path, "/comments"):
		s.bump("comments")
		s.serveREST(w, r, s.comments)
	default: // head read: /repos/{owner}/{repo}/pulls/{n}
		s.bump("head")
		sha := s.nextHead()
		s.serveREST(w, r, `{"head":{"sha":"`+sha+`"}}`)
	}
}

func (s *ghStub) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	q := string(body)
	var key, resp string
	switch {
	case strings.Contains(q, "node(id:"):
		key, resp = "nested", `{"data":{"node":{"comments":{"totalCount":0,"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}},"rateLimit":{"remaining":5000,"limit":5000,"resetAt":"2026-02-01T00:00:00Z"}},"errors":null}`
	case strings.Contains(q, "reviewThreads"):
		key, resp = "threads", s.threads
	default:
		key, resp = "verify", s.verifyResponse()
	}
	s.bump(key)
	if key == "verify" {
		barrier := s.takeBarrier()
		if barrier != nil {
			select {
			case <-barrier:
			case <-r.Context().Done():
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(resp))
}

// verifyResponse builds the viewer verification payload, optionally
// exhausting the persisted GraphQL budget.
func (s *ghStub) verifyResponse() string {
	remaining, reset := 5000, "2026-02-01T00:00:00Z"
	if s.rateRemaining != 0 {
		remaining = s.rateRemaining
		reset = s.rateReset.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(`{"data":{"viewer":{"login":%q},"rateLimit":{"remaining":%d,"limit":5000,"resetAt":%q}},"errors":null}`,
		s.login, remaining, reset)
}

// takeBarrier consumes the one-shot verification barrier.
func (s *ghStub) takeBarrier() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.barrier
	s.barrier = nil
	return b
}

// hangVerification blocks the next verification request until the returned
// channel is closed.
func (s *ghStub) hangVerification() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.barrier = make(chan struct{})
	return s.barrier
}

// nextHead consumes the head sequence, repeating the final entry.
func (s *ghStub) nextHead() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hi < len(s.heads) {
		sha := s.heads[s.hi]
		s.hi++
		return sha
	}
	return s.heads[len(s.heads)-1]
}

// serveREST writes one REST page with rate-budget headers and honors
// If-None-Match with a 304 so the conditional cache stays exercised.
func (s *ghStub) serveREST(w http.ResponseWriter, r *http.Request, body string) {
	etag := fmt.Sprintf(`"%x"`, hash(body))
	w.Header().Set("ETag", etag)
	w.Header().Set("X-RateLimit-Remaining", "5000")
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Reset", "1893456000")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write([]byte(body))
}

func hash(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func (s *ghStub) bump(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[key]++
	s.log = append(s.log, stubHit{key, time.Now()})
}

// timeline returns the stub's request log as "key@offset" strings relative
// to the first recorded request, for pacing diagnostics.
func timeline(t *testing.T, stub *ghStub) []string {
	t.Helper()
	stub.mu.Lock()
	defer stub.mu.Unlock()
	out := make([]string, 0, len(stub.log))
	for i, e := range stub.log {
		if i == 0 {
			out = append(out, e.key+"@0")
			continue
		}
		out = append(out, fmt.Sprintf("%s@%dms", e.key, e.at.Sub(stub.log[0].at).Milliseconds()))
	}
	return out
}

// boundedCtx returns a context bounding one subprocess run.
func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// cliResult captures one subprocess run of a git-feedback binary.
type cliResult struct {
	Code   int
	Stdout string
	Stderr string
	Env    map[string]json.RawMessage
}

// runCLI runs one subprocess invocation. The context bounds the whole run;
// the test fails when the binary outlives it.
func runCLI(ctx context.Context, t *testing.T, bin string, env []string, args ...string) cliResult {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%s %s outlived the test deadline (stdout=%q stderr=%q)", filepath.Base(bin), strings.Join(args, " "), out.String(), errb.String())
		return cliResult{}
	case err := <-done:
		res := cliResult{Stdout: out.String(), Stderr: errb.String()}
		if exit, ok := err.(*exec.ExitError); ok {
			res.Code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("run %s: %v (stderr=%q)", bin, err, errb.String())
		}
		if res.Stdout != "" {
			if err := json.Unmarshal([]byte(res.Stdout), &res.Env); err != nil {
				t.Fatalf("stdout is not one JSON value: %v (%q)", err, res.Stdout)
			}
		}
		return res
	}
}

// testEnv builds a minimal environment for a tagged-binary run against the
// fake server and fake gh.
func testEnv(fakeGH, apiURL string, extra map[string]string) []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + os.TempDir(),
		"GIT_FEEDBACK_API_BASE_URL=" + apiURL,
		"GIT_FEEDBACK_GH_BIN=" + fakeGH,
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// stateArg returns the --state-dir argument pair for one test's store.
func stateArg(dir string) []string { return []string{"--state-dir", dir} }

// runOK runs one tagged-binary invocation that must exit 0 and returns the
// decoded envelope.
func runOK(t *testing.T, env []string, timeout time.Duration, args ...string) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res := runCLI(ctx, t, filepath.Join(repoRoot, "bin", "git-feedback-test"), env, args...)
	if res.Stderr != "" {
		t.Logf("stderr: %s", res.Stderr)
	}
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q stdout=%s)", res.Code, res.Stderr, res.Stdout)
	}
	return res.Env
}

// envelope helpers -------------------------------------------------------------

func statusOf(t *testing.T, env map[string]json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(env["status"], &s); err != nil {
		t.Fatalf("decode status: %v (%v)", err, env["status"])
	}
	return s
}

func stringField(t *testing.T, env map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := env[key]
	if !ok || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode %s: %v (%v)", key, err, raw)
	}
	return s
}

func boolField(t *testing.T, env map[string]json.RawMessage, key string) bool {
	t.Helper()
	var b bool
	if err := json.Unmarshal(env[key], &b); err != nil {
		t.Fatalf("decode %s: %v (%v)", key, err, env[key])
	}
	return b
}

// staleField reads freshness.stale from the envelope.
func staleField(t *testing.T, env map[string]json.RawMessage) bool {
	t.Helper()
	var fr struct {
		Stale bool `json:"stale"`
	}
	if err := json.Unmarshal(env["freshness"], &fr); err != nil {
		t.Fatalf("decode freshness: %v (%v)", err, env["freshness"])
	}
	return fr.Stale
}

// nonNull reports whether an envelope field is present and not null.
func nonNull(env map[string]json.RawMessage, key string) bool {
	raw, ok := env[key]
	return ok && string(raw) != "null"
}

// initStore creates the store schema via a local command that never needs
// the network (its unknown_stream failure is expected and ignored).
func initStore(t *testing.T, env []string, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = runCLI(ctx, t, filepath.Join(repoRoot, "bin", "git-feedback-test"), env, inboxArgs(dir)...)
}

// holdWriteLock opens the store's database directly and holds a write
// transaction (BEGIN IMMEDIATE) until the returned release function runs.
func holdWriteLock(t *testing.T, dir string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "feedback.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open lock db: %v", err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		db.Close()
		t.Fatalf("begin write lock: %v", err)
	}
	return func() {
		_, _ = db.ExecContext(context.Background(), `ROLLBACK`)
		db.Close()
	}
}

// eventIDs returns the event IDs of the envelope's events array.
func eventIDs(t *testing.T, env map[string]json.RawMessage) []string {
	t.Helper()
	var evs []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(env["events"], &evs); err != nil {
		t.Fatalf("decode events: %v (%v)", err, env["events"])
	}
	ids := make([]string, 0, len(evs))
	for _, ev := range evs {
		ids = append(ids, ev.ID)
	}
	return ids
}

// eventKinds returns the kinds of the envelope's events array.
func eventKinds(t *testing.T, env map[string]json.RawMessage) []string {
	t.Helper()
	var evs []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(env["events"], &evs); err != nil {
		t.Fatalf("decode events: %v (%v)", err, env["events"])
	}
	kinds := make([]string, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}
