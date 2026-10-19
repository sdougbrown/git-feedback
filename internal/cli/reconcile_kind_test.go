//go:build testhooks

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot is the repository root, derived from this file's location.
var cliRepoRoot = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate repository root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}()

// fakeGHPath returns an executable copy of the fake gh credential helper.
func fakeGHPath(t *testing.T) string {
	t.Helper()
	src := filepath.Join(cliRepoRoot, "integration", "testdata", "fake-gh")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fake gh: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "fake-gh")
	if err := os.WriteFile(dst, data, 0o700); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return dst
}

// kindFlipStub is a minimal fake GitHub API that answers the viewer
// verification query so admission succeeds; the kind-flip check fires
// before any collection, so no collection endpoint is needed.
func kindFlipStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "viewer") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"alice"},"rateLimit":{"remaining":5000,"limit":5000,"resetAt":"2026-02-01T00:00:00Z"}},"errors":null}`))
	}))
}

// TestReconcileKindMismatchEnvelope: a reconcile whose URL names a
// different kind than the stored stream is a caller error: it fails
// with the usage error (exit 2) naming the kind flip through
// mapReconcileError, before any collection. This is the
// reconcile counterpart of TestLocalKindFlipEnvelope (which covers
// the local-command path).
func TestReconcileKindMismatchEnvelope(t *testing.T) {
	stub := kindFlipStub(t)
	t.Cleanup(stub.Close)
	t.Setenv("GIT_FEEDBACK_API_BASE_URL", stub.URL)
	t.Setenv("GIT_FEEDBACK_GH_BIN", fakeGHPath(t))

	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	code, env, _ := runCLI(t, []string{"reconcile", "--state-dir", dir, "https://github.com/owner/name/issues/7"})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (%v)", code, env["error"])
	}
	assertContract(t, env)
	if env["status"] != "error" {
		t.Errorf("status = %v, want error", env["status"])
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "usage" {
		t.Errorf("error.code = %v, want usage", errObj["code"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "store reset --target") {
		t.Errorf("error.message = %q, want the kind-flip remedy", msg)
	}
}
