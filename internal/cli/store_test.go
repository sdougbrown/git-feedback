package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// publishTargetURL publishes one thread-bearing snapshot for targetURL and
// fails the test on error.
func publishTargetURL(t *testing.T, st *store.Store, targetURL, head string) forge.Target {
	t.Helper()
	target := forge.NewTarget("github", "github.com", "owner/other", 8, targetURL)
	if targetURL == handlerURL {
		target = handlerTarget()
	}
	snap := &forge.Snapshot{
		Head:           head,
		CollectedStart: handlerBaseTime.Add(-time.Minute),
		CollectedEnd:   handlerBaseTime,
		Threads:        []forge.Thread{{ID: "t1", Author: "rev", Body: "fix this", Path: "main.go"}},
	}
	if _, err := st.Publish(context.Background(), store.PublishInput{
		Target: target, Account: "alice", Snapshot: snap,
	}); err != nil {
		t.Fatalf("publish %s: %v", targetURL, err)
	}
	return target
}

// TestStorePathCommand verifies that store path reports the resolved store
// directory without opening or creating the store.
func TestStorePathCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	code, env, _ := runCLI(t, []string{"store", "path", "--state-dir", dir})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if env["status"] != StatusOK {
		t.Errorf("status = %v, want %s", env["status"], StatusOK)
	}
	storeObj, ok := env["store"].(map[string]any)
	if !ok {
		t.Fatalf("store = %v, want object", env["store"])
	}
	if storeObj["path"] != dir {
		t.Errorf("store.path = %v, want %q", storeObj["path"], dir)
	}
	// The store must not have been created.
	if _, err := os.Stat(filepath.Join(dir, "feedback.db")); !os.IsNotExist(err) {
		t.Error("store path created the store database")
	}
}

// TestStoreResetRemovesStore verifies that store reset deletes the database
// files and reports updated, then unchanged on a second run with nothing to
// remove.
func TestStoreResetRemovesStore(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	code, env, _ := runCLI(t, []string{"store", "reset", "--state-dir", dir})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if env["status"] != StatusUpdated {
		t.Errorf("status = %v, want %s", env["status"], StatusUpdated)
	}
	if _, err := os.Stat(filepath.Join(dir, "feedback.db")); !os.IsNotExist(err) {
		t.Error("database file still present after reset")
	}

	code, env, _ = runCLI(t, []string{"store", "reset", "--state-dir", dir})
	if code != ExitOK {
		t.Fatalf("second reset exit code = %d, want 0", code)
	}
	if env["status"] != StatusUnchanged {
		t.Errorf("second reset status = %v, want %s", env["status"], StatusUnchanged)
	}

	// The reset store is usable again.
	st2, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("open after reset: %v", err)
	}
	st2.Close()
}

// TestStoreResetTargetScoped verifies that store reset --target deletes only
// that target's stream; another target's stream survives.
func TestStoreResetTargetScoped(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	other := publishTargetURL(t, st, "https://github.com/owner/other/pull/8", "head-other")
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	code, env, _ := runCLI(t, []string{"store", "reset", "--target", handlerURL, "--state-dir", dir})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if env["status"] != StatusUpdated {
		t.Errorf("status = %v, want %s", env["status"], StatusUpdated)
	}
	storeObj, ok := env["store"].(map[string]any)
	if !ok {
		t.Fatalf("store = %v, want object", env["store"])
	}
	targetObj, ok := storeObj["target"].(map[string]any)
	if !ok || targetObj["id"] != handlerTarget().ID {
		t.Errorf("store.target = %v, want id %q", storeObj["target"], handlerTarget().ID)
	}

	// The scoped target is gone; the other target survives.
	st2, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatalf("open after reset: %v", err)
	}
	defer st2.Close()
	if _, ok, err := st2.StreamID(context.Background(), handlerTarget().ID, "alice"); err != nil || ok {
		t.Fatalf("reset target stream still present (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := st2.StreamID(context.Background(), other.ID, "alice"); err != nil || !ok {
		t.Fatalf("surviving stream missing (ok=%v, err=%v)", ok, err)
	}
}

// TestLegacyStoreRefusedByCommands verifies that a store written before data
// versioning refuses local commands with the store path and remedy in the
// envelope, and that store reset (which never opens the store) still works
// as the remedy.
func TestLegacyStoreRefusedByCommands(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	st.Close()
	// Strip the data version to mimic a store written before data versioning.
	db, err := sql.Open("sqlite", filepath.Join(dir, "feedback.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM data_version`); err != nil {
		t.Fatalf("clear data_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	var stdout bytes.Buffer
	code := Run([]string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL}, &stdout, &bytes.Buffer{})
	if code != ExitOperational {
		t.Fatalf("exit code = %d, want 1", code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout is not one JSON value: %v (%q)", err, stdout.String())
	}
	errObj, ok := envelope["error"].(map[string]any)
	if !ok || errObj["code"] != store.CodeStoreDataOlder {
		t.Fatalf("error envelope = %v, want code %s", envelope["error"], store.CodeStoreDataOlder)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, filepath.Join(dir, "feedback.db")) || !strings.Contains(msg, "store reset") {
		t.Errorf("message %q does not name the path and remedy", msg)
	}

	// The remedy: a full reset never opens the store, so it works here.
	code, env, _ := runCLI(t, []string{"store", "reset", "--state-dir", dir})
	if code != ExitOK || env["status"] != StatusUpdated {
		t.Fatalf("remedy reset: exit %d status %v, want 0/%s", code, env["status"], StatusUpdated)
	}
	code, _, _ = runCLI(t, []string{"store", "reset", "--state-dir", dir})
	if code != ExitOK {
		t.Fatalf("post-reset store unusable: exit %d", code)
	}
}

// TestStoreResetTargetUnknownStream verifies that resetting a target with no
// stored stream reports unchanged, not an error.
func TestStoreResetTargetUnknownStream(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	code, env, _ := runCLI(t, []string{"store", "reset", "--target", handlerURL, "--state-dir", dir})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if env["status"] != StatusUnchanged {
		t.Errorf("status = %v, want %s", env["status"], StatusUnchanged)
	}
}

// TestStoreUsageErrors verifies the caller-error surface of the store
// command: a missing or unknown subcommand, a stray URL positional, and an
// unparseable --target URL all exit 2 with a usage envelope.
func TestStoreUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	tests := []struct {
		name string
		argv []string
	}{
		{"missing subcommand", []string{"store", "--state-dir", dir}},
		{"unknown subcommand", []string{"store", "bogus"}},
		{"stray positional", []string{"store", "path", "https://github.com/o/r/pull/1"}},
		{"reset stray positional", []string{"store", "reset", "https://github.com/o/r/pull/1"}},
		{"bad target URL", []string{"store", "reset", "--target", "not-a-url", "--state-dir", dir}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tc.argv, &stdout, &stderr)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("stdout is not one JSON value: %v (%q)", err, stdout.String())
			}
			if envelope["status"] != StatusError {
				t.Errorf("status = %v, want error", envelope["status"])
			}
			errObj, ok := envelope["error"].(map[string]any)
			if !ok || errObj["code"] != "usage" {
				t.Errorf("error envelope = %v, want code usage", envelope["error"])
			}
		})
	}
}

// TestStoreParseSubcommand verifies that the nested subcommand lands in
// Invocation.Sub.
func TestStoreParseSubcommand(t *testing.T) {
	inv, err := parseInvocation([]string{"store", "reset", "--target", handlerURL})
	if err != nil {
		t.Fatalf("parseInvocation: %v", err)
	}
	if inv.Sub != "reset" || inv.Flags["target"] != handlerURL {
		t.Fatalf("inv = %+v", inv)
	}
	inv, err = parseInvocation([]string{"store", "path"})
	if err != nil {
		t.Fatalf("parseInvocation: %v", err)
	}
	if inv.Sub != "path" {
		t.Fatalf("sub = %q, want path", inv.Sub)
	}
}
