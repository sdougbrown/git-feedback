package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
)

// Read-only transport resources.
const (
	ResourceREST      = "rest"
	ResourceGraphQL   = "graphql"
	ResourceSecondary = "secondary"
)

// DefaultAPIBase is the GitHub REST/GraphQL API root used when no override
// is supplied. Request paths are joined with a leading slash, so the base
// carries no trailing slash.
const DefaultAPIBase = "https://api.github.com"

// Transport performs read-only HTTP requests against the GitHub API. Every
// request — including viewer verification and unconditional refetches — is
// gated and paced before it leaves the process.
type Transport struct {
	client *http.Client
	apiURL *url.URL
	token  string
	pacer  RequestPacer
	gate   RateGate
	clock  clock.Clock
}

// NewTransport builds a Transport for the API at apiBase (e.g.
// https://api.github.com) authenticated with token. An empty apiBase
// selects the GitHub API default; the production build never injects a
// test-only override.
func NewTransport(apiBase, token string, pacer RequestPacer, gate RateGate, clk clock.Clock) (*Transport, error) {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	u, err := url.Parse(apiBase)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid API base URL %q", apiBase)
	}
	apiHost := u.Host
	client := &http.Client{}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != apiHost {
			return fmt.Errorf("refusing redirect to different host %q", req.URL.Host)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &Transport{
		client: client,
		apiURL: u,
		token:  token,
		pacer:  pacer,
		gate:   gate,
		clock:  clk,
	}, nil
}

// Get performs a read-only GET against path (relative to the API base, or
// an absolute URL on the API host, as returned in a rel="next" Link
// header). Cross-host absolute URLs are rejected before the request is
// issued.
func (t *Transport) Get(ctx context.Context, path string, cond *CacheEntry) (int, http.Header, []byte, forge.RateInfo, bool, error) {
	extra := map[string]string{}
	if cond != nil && cond.ETag != "" {
		extra["If-None-Match"] = cond.ETag
	}
	u, err := url.Parse(path)
	if err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	var rawURL string
	if u.IsAbs() {
		if u.Host != t.apiURL.Host {
			return 0, nil, nil, forge.RateInfo{}, false, fmt.Errorf("refusing request to different host %q", u.Host)
		}
		rawURL = u.String()
	} else {
		rawURL = t.apiURL.String() + path
	}
	return t.do(ctx, http.MethodGet, rawURL, "", nil, ResourceREST, extra)
}

// GraphQL performs a read-only GraphQL POST and returns the raw data along
// with any response-level errors.
func (t *Transport) GraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, []string, forge.RateInfo, bool, error) {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, forge.RateInfo{}, false, err
	}
	status, _, respBody, info, ok, err := t.do(ctx, http.MethodPost, t.apiURL.String()+"/graphql", "application/json", body, ResourceGraphQL, nil)
	if err != nil {
		return nil, nil, forge.RateInfo{}, false, err
	}
	if status != http.StatusOK {
		return nil, nil, forge.RateInfo{}, false, fmt.Errorf("graphql returned HTTP %d", status)
	}
	var parsed struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, nil, forge.RateInfo{}, false, fmt.Errorf("malformed GraphQL response: %w", err)
	}
	msgs := make([]string, 0, len(parsed.Errors))
	for _, e := range parsed.Errors {
		msgs = append(msgs, e.Message)
	}
	return parsed.Data, msgs, info, ok, nil
}

func (t *Transport) do(ctx context.Context, method, rawURL, contentType string, payload []byte, resource string, extra map[string]string) (int, http.Header, []byte, forge.RateInfo, bool, error) {
	if method != http.MethodGet && method != http.MethodPost {
		return 0, nil, nil, forge.RateInfo{}, false, fmt.Errorf("method %q is not permitted by the read-only transport", method)
	}
	if err := t.gate.Check(ctx, resource, t.clock.Now()); err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	if err := t.pacer.Wait(ctx); err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Attach the token only to the configured API host. Cross-host redirects
	// are rejected by CheckRedirect regardless.
	if u.Host == t.apiURL.Host && t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, forge.RateInfo{}, false, err
	}
	info, ok := rateInfoFromHeader(resp.Header, resource)
	if ok {
		t.gate.Record(info)
	}
	if until, hasBackoff := backoffFromHeader(resp.Header, t.clock.Now()); hasBackoff {
		if b, isBackoffGate := t.gate.(BackoffGate); isBackoffGate {
			b.Backoff(resource, until)
		}
	}
	return resp.StatusCode, resp.Header, body, info, ok, nil
}

// rateInfoFromHeader extracts primary rate-budget metadata from the REST
// rate-limit headers. ok is false when the response carries no budget.
func rateInfoFromHeader(h http.Header, resource string) (forge.RateInfo, bool) {
	rem := h.Get("X-RateLimit-Remaining")
	if rem == "" {
		return forge.RateInfo{}, false
	}
	info := forge.RateInfo{Resource: resource}
	info.Remaining, _ = strconv.Atoi(rem)
	if lim := h.Get("X-RateLimit-Limit"); lim != "" {
		info.Limit, _ = strconv.Atoi(lim)
	}
	if reset := h.Get("X-RateLimit-Reset"); reset != "" {
		if sec, err := strconv.ParseInt(reset, 10, 64); err == nil {
			info.Reset = time.Unix(sec, 0).UTC()
		}
	}
	return info, true
}

// backoffFromHeader extracts an out-of-band backoff window from
// Retry-After or X-Poll-Interval, in seconds.
func backoffFromHeader(h http.Header, now time.Time) (time.Time, bool) {
	for _, name := range []string{"Retry-After", "X-Poll-Interval"} {
		if v := h.Get(name); v != "" {
			if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
				return now.Add(time.Duration(sec) * time.Second), true
			}
		}
	}
	return time.Time{}, false
}
