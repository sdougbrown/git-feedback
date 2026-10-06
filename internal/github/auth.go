package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// credentialTimeout bounds the gh auth token lookup.
const credentialTimeout = 10 * time.Second

// CommandRunner shells out to external commands; injectable for tests.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the production CommandRunner.
type ExecRunner struct{}

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// BootstrapAdmission is the host-wide coordination barrier taken before any
// GraphQL identity verification. Stage 2 uses the in-memory implementation;
// Stage 4 supplies the lease-backed one.
type BootstrapAdmission interface {
	Acquire(ctx context.Context, host string) error
	Release(host string)
}

// MemoryAdmission is the in-memory BootstrapAdmission.
type MemoryAdmission struct {
	mu   sync.Mutex
	held map[string]bool
}

// NewMemoryAdmission returns an empty MemoryAdmission.
func NewMemoryAdmission() *MemoryAdmission {
	return &MemoryAdmission{held: map[string]bool{}}
}

// Acquire implements BootstrapAdmission.
func (a *MemoryAdmission) Acquire(ctx context.Context, host string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held[host] {
		return fmt.Errorf("bootstrap admission for %q is already held", host)
	}
	a.held[host] = true
	return nil
}

// Release implements BootstrapAdmission.
func (a *MemoryAdmission) Release(host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.held, host)
}

// Session is the authenticated view of one GitHub account; it carries the
// verified transport used for collection.
type Session struct {
	login string
	tp    *Transport
}

// Login implements forge.Session.
func (s *Session) Login() string { return s.login }

// Adapter implements forge.Adapter for GitHub.com.
type Adapter struct {
	apiBase   string
	runner    CommandRunner
	admission BootstrapAdmission
	gate      RateGate
	pacer     RequestPacer
	cache     HTTPCache
	clock     clock.Clock
}

// NewAdapter builds a GitHub adapter bound to the given scoped services.
// Nil services fall back to defaults: the real gh CLI, in-memory
// coordination, in-memory gate/cache, a one-second pacer, and the real clock.
func NewAdapter(apiBase string, runner CommandRunner, admission BootstrapAdmission, gate RateGate, pacer RequestPacer, cache HTTPCache, clk clock.Clock) *Adapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	if admission == nil {
		admission = NewMemoryAdmission()
	}
	if gate == nil {
		gate = NewMemoryGate()
	}
	if cache == nil {
		cache = NewMemoryCache()
	}
	if clk == nil {
		clk = clock.Real{}
	}
	if pacer == nil {
		pacer = NewFixedPacer(clk, time.Second)
	}
	return &Adapter{
		apiBase:   apiBase,
		runner:    runner,
		admission: admission,
		gate:      gate,
		pacer:     pacer,
		cache:     cache,
		clock:     clk,
	}
}

// Host implements forge.Adapter.
func (a *Adapter) Host() string { return "github.com" }

// ParseTarget implements forge.Adapter.
func (a *Adapter) ParseTarget(raw string) (forge.Target, error) { return ParseTarget(raw) }

const verifyQuery = `query{
  viewer{login}
  rateLimit{remaining limit resetAt}
}`

// Authenticate resolves the gh credential, takes host-wide bootstrap
// admission, and verifies the account with a gated viewer{login} plus
// rateLimit query through the same paced transport used for collection.
func (a *Adapter) Authenticate(ctx context.Context, account string) (forge.Session, error) {
	token, err := a.ghToken(ctx, account)
	if err != nil {
		return nil, err
	}
	host := "github.com"
	if err := a.admission.Acquire(ctx, host); err != nil {
		return nil, fmt.Errorf("%w: %v", forge.ErrAuth, err)
	}
	defer a.admission.Release(host)

	tp, err := NewTransport(a.apiBase, token, a.pacer, a.gate, a.clock)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", forge.ErrAuth, err)
	}
	data, gqlErrs, _, _, err := tp.GraphQL(ctx, verifyQuery, nil)
	if err != nil {
		var rl *forge.ErrRateLimited
		if errors.As(err, &rl) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", forge.ErrAuth, err)
	}
	if len(gqlErrs) > 0 {
		return nil, fmt.Errorf("%w: verification failed: %s", forge.ErrAuth, strings.Join(gqlErrs, "; "))
	}
	var payload struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		RateLimit *gqlRateLimit `json:"rateLimit"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("%w: malformed verification response: %v", forge.ErrAuth, err)
	}
	if payload.Viewer.Login == "" || payload.RateLimit == nil {
		return nil, fmt.Errorf("%w: incomplete verification response", forge.ErrAuth)
	}
	if account != "" && forge.CanonicalAccount(payload.Viewer.Login) != forge.CanonicalAccount(account) {
		return nil, fmt.Errorf("%w: verified %q but requested %q", forge.ErrAccountMismatch, payload.Viewer.Login, account)
	}
	a.gate.Record(forge.RateInfo{
		Resource:  ResourceGraphQL,
		Remaining: payload.RateLimit.Remaining,
		Limit:     payload.RateLimit.Limit,
		Reset:     payload.RateLimit.ResetAt,
	})
	return &Session{login: payload.Viewer.Login, tp: tp}, nil
}

// ghToken performs the bounded, non-interactive credential lookup.
func (a *Adapter) ghToken(ctx context.Context, account string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	args := []string{"auth", "token", "--hostname", "github.com"}
	if account != "" {
		args = append(args, "--user", account)
	}
	out, err := a.runner.Run(ctx, "gh", args...)
	if err != nil {
		return "", fmt.Errorf("%w: gh auth token: %v", forge.ErrAuth, err)
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", fmt.Errorf("%w: gh auth token returned no credential", forge.ErrAuth)
	}
	return token, nil
}
