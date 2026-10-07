package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// spyGate records Check/Backoff calls while delegating to a MemoryGate.
type spyGate struct {
	*MemoryGate
	checks   []string
	backoffs []string
}

func (g *spyGate) Check(ctx context.Context, resource string, now time.Time) error {
	g.checks = append(g.checks, resource)
	return g.MemoryGate.Check(ctx, resource, now)
}

func (g *spyGate) Backoff(resource string, until time.Time) {
	g.backoffs = append(g.backoffs, resource)
	g.MemoryGate.Backoff(resource, until)
}

// testEnv wires an Adapter against a stub HTTP server with a fake clock.
type testEnv struct {
	adapter  *Adapter
	gate     *spyGate
	server   *httptest.Server
	requests *[]http.Request
}

func newTestEnv(t *testing.T, handler http.HandlerFunc) *testEnv {
	t.Helper()
	var requests []http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, *r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	gate := &spyGate{MemoryGate: NewMemoryGate()}
	adapter := NewAdapter(srv.URL, recordingRunner{token: "tok"}, NewMemoryAdmission(), gate, NewFixedPacer(clk, time.Second), NewMemoryCache(), clk)
	return &testEnv{adapter: adapter, gate: gate, server: srv, requests: &requests}
}

// verifyResponse is the GraphQL payload returned for the verification query.
func verifyResponse(login string) string {
	return `{"data":{"viewer":{"login":"` + login + `"},"rateLimit":{"remaining":4990,"limit":5000,"resetAt":"2030-01-01T00:00:00Z"}}}`
}

// recordingRunner is a CommandRunner returning token, or failing when run
// is set and returns an error.
type recordingRunner struct {
	token string
	run   func(ctx context.Context) error
}

func (r recordingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name != "gh" {
		return "", errors.New("unexpected command " + name)
	}
	if r.run != nil {
		if err := r.run(ctx); err != nil {
			return "", err
		}
	}
	return r.token + "\n", nil
}

func TestGhAuthBounded(t *testing.T) {
	var deadline time.Time
	runner := recordingRunner{run: func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("expected credential lookup context to carry a deadline")
		}
		deadline = d
		// exec.CommandContext kills the process at the deadline.
		return errors.New("signal: killed")
	}}
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(verifyResponse("alice")))
	})
	env.adapter.runner = runner
	_, err := env.adapter.Authenticate(context.Background(), "alice")
	if !errors.Is(err, forge.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	bound := credentialTimeout
	if d := time.Until(deadline); d <= 0 || d > bound {
		t.Fatalf("credential lookup not bounded near %v: %v", bound, d)
	}
}

func TestAccountMismatch(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(verifyResponse("bob")))
	})
	_, err := env.adapter.Authenticate(context.Background(), "alice")
	if !errors.Is(err, forge.ErrAccountMismatch) {
		t.Fatalf("want ErrAccountMismatch, got %v", err)
	}
}

func TestVerificationUsesRateGate(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(verifyResponse("alice")))
	})
	// Pre-block the GraphQL resource: verification must honor the gate.
	env.gate.Backoff(ResourceGraphQL, time.Now().Add(time.Hour))
	_, err := env.adapter.Authenticate(context.Background(), "alice")
	var rateLimited *forge.ErrRateLimited
	if !errors.As(err, &rateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if len(*env.requests) != 0 {
		t.Fatalf("no request should have been issued while gated, got %d", len(*env.requests))
	}
	if len(env.gate.checks) == 0 || env.gate.checks[0] != ResourceGraphQL {
		t.Fatalf("verification did not check the GraphQL gate: %v", env.gate.checks)
	}

	// Unblocked verification goes through the same transport and records quota.
	env2 := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(verifyResponse("alice")))
	})
	sess, err := env2.adapter.Authenticate(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if sess.Login() != "alice" {
		t.Fatalf("want login alice, got %q", sess.Login())
	}
	seenGraphQL := false
	for _, r := range *env2.requests {
		if r.URL.Path == "/graphql" && r.Method == http.MethodPost {
			seenGraphQL = true
		}
	}
	if !seenGraphQL {
		t.Fatal("verification did not POST to /graphql")
	}
}

func TestReadOnlyTransport(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	tp, err := NewTransport(env.server.URL, "tok", NewFixedPacer(clock.NewFake(time.Now()), 0), NewMemoryGate(), clock.NewFake(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		t.Fatalf("GET should be allowed: %v", err)
	}
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodHead} {
		if _, _, _, _, _, err := tp.do(ctx, method, env.server.URL+"/x", "", nil, ResourceREST, nil); err == nil {
			t.Fatalf("%s should be denied", method)
		}
	}
}

func TestRejectsCrossHostRedirect(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	})
	tp, err := NewTransport(env.server.URL, "sekret", NewFixedPacer(clock.NewFake(time.Now()), 0), NewMemoryGate(), clock.NewFake(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, err = tp.Get(context.Background(), "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "different host") {
		t.Fatalf("want cross-host redirect rejection, got %v", err)
	}
}

func TestRetryAfterGates(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "60")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	gate := NewMemoryGate()
	tp, _ := NewTransport(srv.URL, "tok", NewFixedPacer(clk, 0), gate, clk)
	ctx := context.Background()
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		var rateLimited *forge.ErrRateLimited
		if !errors.As(err, &rateLimited) {
			t.Fatalf("second request should be rate limited, got %v", err)
		}
	}
	clk.Advance(61 * time.Second)
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		t.Fatalf("request after backoff should pass: %v", err)
	}
}

func TestPollIntervalHeader(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Poll-Interval", "30")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	gate := NewMemoryGate()
	tp, _ := NewTransport(srv.URL, "tok", NewFixedPacer(clk, 0), gate, clk)
	ctx := context.Background()
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, err := tp.Get(ctx, "/x", nil)
	var rateLimited *forge.ErrRateLimited
	if !errors.As(err, &rateLimited) {
		t.Fatalf("X-Poll-Interval should gate the next request, got %v", err)
	}
	clk.Advance(31 * time.Second)
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		t.Fatalf("request after poll interval should pass: %v", err)
	}
}

func TestReserveRemaining(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "9")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Reset", itoa(reset))
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	gate := NewMemoryGate()
	tp, _ := NewTransport(srv.URL, "tok", NewFixedPacer(clk, 0), gate, clk)
	ctx := context.Background()
	if _, _, _, _, _, err := tp.Get(ctx, "/x", nil); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, err := tp.Get(ctx, "/x", nil)
	var rateLimited *forge.ErrRateLimited
	if !errors.As(err, &rateLimited) {
		t.Fatalf("remaining below the reserve should gate the next request, got %v", err)
	}
}

func itoa(i int64) string {
	return strconv.FormatInt(i, 10)
}
