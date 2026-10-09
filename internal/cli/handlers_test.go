package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// handlerBaseTime is the fake clock origin for handler tests.
var handlerBaseTime = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

// handlerURL is the canonical target URL for handler tests.
const handlerURL = "https://github.com/owner/name/pull/7"

// handlerTarget matches handlerURL.
func handlerTarget() forge.Target {
	return forge.NewTarget("github", "github.com", "owner/name", 7, handlerURL)
}

// handlerEnv installs the fake clock and returns the state directory with a
// fresh store for seeding.
func handlerEnv(t *testing.T) (dir string, st *store.Store, clk *clock.Fake) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "state")
	clk = clock.NewFake(handlerBaseTime)
	prev := newClock
	newClock = func() clock.Clock { return clk }
	t.Cleanup(func() { newClock = prev })
	var err error
	st, err = store.Open(dir, store.Options{Clock: clk})
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st, clk
}

// publish publishes one snapshot with feedback objects for account.
func publishHandler(t *testing.T, st *store.Store, account string) {
	t.Helper()
	snap := &forge.Snapshot{
		Head:           "headA",
		CollectedStart: handlerBaseTime.Add(-time.Minute),
		CollectedEnd:   handlerBaseTime,
		Threads:        []forge.Thread{{ID: "t1", Author: "rev", Body: "fix this", Path: "main.go"}},
		Reviews:        []forge.Review{{ID: "r1", Author: "rev", Body: "changes requested", State: "CHANGES_REQUESTED"}},
		Comments:       []forge.Comment{{ID: "c1", Author: "rev", Body: "note"}},
	}
	if _, err := st.Publish(context.Background(), store.PublishInput{
		Target: handlerTarget(), Account: account, Snapshot: snap,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// publishMixedHandler publishes one snapshot where account authors one of
// each feedback kind and "rev" authors the rest, so the self filter has
// something to catch.
func publishMixedHandler(t *testing.T, st *store.Store, account string) {
	t.Helper()
	snap := &forge.Snapshot{
		Head:           "headA",
		CollectedStart: handlerBaseTime.Add(-time.Minute),
		CollectedEnd:   handlerBaseTime,
		Threads: []forge.Thread{
			{ID: "t1", Author: account, Body: "own thread", Path: "a.go"},
			{ID: "t2", Author: "rev", Body: "rev thread", Path: "b.go"},
		},
		Reviews: []forge.Review{
			{ID: "r1", Author: account, Body: "own review", State: "APPROVED"},
			{ID: "r2", Author: "rev", Body: "rev review", State: "APPROVED"},
		},
		Comments: []forge.Comment{
			{ID: "c1", Author: account, Body: "own comment"},
			{ID: "c2", Author: "rev", Body: "rev comment"},
		},
	}
	if _, err := st.Publish(context.Background(), store.PublishInput{
		Target: handlerTarget(), Account: account, Snapshot: snap,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// runCLI runs one invocation and decodes the envelope.
func runCLI(t *testing.T, argv []string) (code int, env map[string]any, stdout string) {
	t.Helper()
	code, env, stdout, _ = runCLIStd(t, argv)
	return code, env, stdout
}

func runCLIStd(t *testing.T, argv []string) (code int, env map[string]any, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(argv, &out, &errBuf)
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v (%q)", err, out.String())
	}
	return code, env, out.String(), errBuf.String()
}

// assertContract asserts the envelope carries every contract field.
func assertContract(t *testing.T, env map[string]any) {
	t.Helper()
	for _, key := range []string{
		"schema", "command", "status", "target", "account", "observed_head",
		"expected_head", "snapshot", "attempt", "freshness", "reviewer_completion",
		"events", "has_more", "next_cursor", "export", "error",
	} {
		if _, ok := env[key]; !ok {
			t.Errorf("missing contract field %q", key)
		}
	}
	if env["schema"] != "git-feedback/v1" || env["reviewer_completion"] != "unknown" {
		t.Errorf("schema/reviewer_completion = %v/%v", env["schema"], env["reviewer_completion"])
	}
}

func TestReconcileBusyEnvelope(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	// A live bootstrap lease makes the engine return busy before any
	// credential lookup or network access.
	if err := st.PutLease(context.Background(), "github.com", "", "other", handlerBaseTime.Add(time.Minute)); err != nil {
		t.Fatalf("put lease: %v", err)
	}
	code, env, _ := runCLI(t, []string{"reconcile", "--state-dir", dir, handlerURL})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	if env["status"] != "busy" {
		t.Errorf("status = %v, want busy", env["status"])
	}
	target := env["target"].(map[string]any)
	if target["id"] != handlerTarget().ID || target["forge"] != "github" || target["host"] != "github.com" ||
		target["repo"] != "owner/name" || target["number"].(float64) != 7 {
		t.Errorf("target = %v", target)
	}
	if env["account"] != nil {
		t.Errorf("account = %v, want null without a verified session", env["account"])
	}
	if env["error"] != nil {
		t.Errorf("error = %v, want null", env["error"])
	}
}

func TestReconcileUnsupportedHostBeforeAuth(t *testing.T) {
	_, _, _ = handlerEnv(t)
	code, env, _ := runCLI(t, []string{
		"reconcile", "--state-dir", filepath.Join(t.TempDir(), "state"), "https://gitlab.com/o/r/pull/1",
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertContract(t, env)
	if env["status"] != "error" {
		t.Errorf("status = %v, want error", env["status"])
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "unsupported_host" {
		t.Errorf("error.code = %v, want unsupported_host", errObj["code"])
	}
	if env["target"] != nil {
		t.Errorf("target = %v, want null for an unsupported host", env["target"])
	}
}

func TestLocalWithoutStreamIsExplicitError(t *testing.T) {
	_, _, _ = handlerEnv(t)
	state := filepath.Join(t.TempDir(), "state")
	tests := []struct {
		name string
		argv []string
	}{
		{"snapshot", []string{"snapshot", "--snapshot", "s1", "--output", "out.json", "--state-dir", state, handlerURL}},
		{"inbox", []string{"inbox", "--consumer", "ci", "--state-dir", state, handlerURL}},
		{"ack", []string{"ack", "--consumer", "ci", "--event", "e1", "--state-dir", state, handlerURL}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, env, _ := runCLI(t, tc.argv)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			assertContract(t, env)
			errObj := env["error"].(map[string]any)
			if errObj["code"] != "unknown_stream" {
				t.Errorf("error.code = %v, want unknown_stream", errObj["code"])
			}
			if env["status"] != "error" {
				t.Errorf("status = %v, want error", env["status"])
			}
		})
	}
}

func TestAmbiguousAccountError(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	publishHandler(t, st, "bob")

	code, env, _ := runCLI(t, []string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "ambiguous_account" {
		t.Errorf("error.code = %v, want ambiguous_account", errObj["code"])
	}
}

func TestSnapshotCommand(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	t.Run("exports the requested snapshot", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "snap.json")
		code, env, _ := runCLI(t, []string{
			"snapshot", "--snapshot", "s1", "--output", out, "--state-dir", dir, handlerURL,
		})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		if env["status"] != "ok" {
			t.Errorf("status = %v, want ok", env["status"])
		}
		if env["account"] != "alice" {
			t.Errorf("account = %v, want alice", env["account"])
		}
		if obs, ok := env["observed_head"].(string); !ok || obs != "headA" {
			t.Errorf("observed_head = %v, want headA", env["observed_head"])
		}
		snap := env["snapshot"].(map[string]any)
		if snap["id"] != "s1" || snap["complete"] != true {
			t.Errorf("snapshot = %v", snap)
		}
		counts := snap["object_counts"].(map[string]any)
		if counts["thread"].(float64) != 1 || counts["review"].(float64) != 1 || counts["comment"].(float64) != 1 {
			t.Errorf("object_counts = %v", counts)
		}
		exp := env["export"].(map[string]any)
		if exp["path"] != out {
			t.Errorf("export.path = %v, want %s", exp["path"], out)
		}
		if digest, _ := exp["digest"].(string); len(digest) != 64 {
			t.Errorf("export.digest = %v", exp["digest"])
		}
	})

	t.Run("missing --snapshot is a usage error", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{
			"snapshot", "--output", filepath.Join(t.TempDir(), "x.json"), "--state-dir", dir, handlerURL,
		})
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		assertContract(t, env)
		if env["error"].(map[string]any)["code"] != "usage" {
			t.Errorf("error = %v, want usage", env["error"])
		}
	})

	t.Run("unknown snapshot is never substituted", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "missing.json")
		code, env, _ := runCLI(t, []string{
			"snapshot", "--snapshot", "s9", "--output", out, "--state-dir", dir, handlerURL,
		})
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if env["error"].(map[string]any)["code"] != "unknown_snapshot" {
			t.Errorf("error = %v, want unknown_snapshot", env["error"])
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("output file exists after failed export")
		}
	})
}

func TestInboxDoesNotAck(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	argv := []string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL}
	code, env, _ := runCLI(t, argv)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	events := env["events"].([]any)
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 (target + thread + review + comment)", len(events))
	}
	first := events[0].(map[string]any)
	if first["id"] != "e1" || first["kind"] != "initial_observation" {
		t.Errorf("first event = %v", first)
	}
	// The target event is author-less; the contract omits the "author" key
	// rather than rendering an empty value.
	if _, hasAuthor := first["author"]; hasAuthor {
		t.Errorf("target event carries an %q key, want omitted", "author")
	}
	if env["has_more"] != false || env["next_cursor"] != nil {
		t.Errorf("has_more/next_cursor = %v/%v, want false/null", env["has_more"], env["next_cursor"])
	}

	// A second read returns the same events: reading acknowledged nothing.
	code, env2, _ := runCLI(t, argv)
	if code != 0 || len(env2["events"].([]any)) != 4 {
		t.Fatalf("second read = exit %d, events %v; reading must not acknowledge", code, env2["events"])
	}
	page, err := st.Inbox(context.Background(), store.InboxInput{
		TargetID: handlerTarget().ID, Account: "alice", Consumer: "ci", Limit: store.MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("direct inbox: %v", err)
	}
	if len(page.Events) != 4 {
		t.Errorf("stored pending events = %d, want 4", len(page.Events))
	}
}

func TestInboxUsageErrors(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	base := []string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL}

	t.Run("bad limit", func(t *testing.T) {
		for _, raw := range []string{"abc", "0", "300"} {
			code, env, _ := runCLI(t, append(append(append([]string{}, base[:3]...), "--limit", raw), base[3:]...))
			if code != 2 {
				t.Fatalf("limit %q: exit = %d, want 2", raw, code)
			}
			assertContract(t, env)
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("limit %q: error = %v, want usage", raw, env["error"])
			}
		}
	})

	t.Run("malformed cursor", func(t *testing.T) {
		code, env, _ := runCLI(t, append(append(append([]string{}, base[:3]...), "--after", "!!!"), base[3:]...))
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if env["error"].(map[string]any)["code"] != "usage" {
			t.Errorf("error = %v, want usage", env["error"])
		}
	})
}

func TestInboxPagination(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	page1, env1, _ := runCLI(t, []string{
		"inbox", "--consumer", "ci", "--limit", "2", "--state-dir", dir, handlerURL,
	})
	if page1 != 0 || len(env1["events"].([]any)) != 2 || env1["has_more"] != true {
		t.Fatalf("page1 = exit %d, has_more %v, env %v", page1, env1["has_more"], env1["error"])
	}
	cursor := env1["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor empty with has_more")
	}
	page2, env2, _ := runCLI(t, []string{
		"inbox", "--consumer", "ci", "--limit", "2", "--after", cursor, "--state-dir", dir, handlerURL,
	})
	if page2 != 0 || len(env2["events"].([]any)) != 2 || env2["has_more"] != false {
		t.Fatalf("page2 = exit %d, has_more %v", page2, env2["has_more"])
	}
	// The two pages must not overlap.
	seen := map[string]bool{}
	for _, e := range env1["events"].([]any) {
		seen[e.(map[string]any)["id"].(string)] = true
	}
	for _, e := range env2["events"].([]any) {
		id := e.(map[string]any)["id"].(string)
		if seen[id] {
			t.Errorf("event %s appears on both pages", id)
		}
	}

	// A cursor from consumer ci is invalid for consumer ci2.
	code, env3, _ := runCLI(t, []string{
		"inbox", "--consumer", "ci2", "--after", cursor, "--state-dir", dir, handlerURL,
	})
	if code != 2 {
		t.Fatalf("cross-consumer cursor: exit = %d, want 2", code)
	}
	if env3["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("cross-consumer cursor error = %v, want usage", env3["error"])
	}
}

func TestInboxIDsOnly(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	argv := []string{"inbox", "--consumer", "ci", "--ids-only", "--state-dir", dir, handlerURL}
	code, env, _ := runCLI(t, argv)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	raw, ok := env["events"].([]any)
	if !ok || len(raw) != 4 {
		t.Fatalf("events = %v, want four ID strings", env["events"])
	}
	want := []string{"e1", "e2", "e3", "e4"}
	for i, e := range raw {
		id, isStr := e.(string)
		if !isStr || id != want[i] {
			t.Errorf("events[%d] = %v, want %q", i, e, want[i])
		}
	}
	if env["has_more"] != false || env["next_cursor"] != nil {
		t.Errorf("has_more/next_cursor = %v/%v, want false/null", env["has_more"], env["next_cursor"])
	}

	// --ids-only acknowledges nothing, like the default rendering.
	code, env2, _ := runCLI(t, argv)
	if code != 0 || len(env2["events"].([]any)) != 4 {
		t.Fatalf("second read = exit %d, events %v; reading must not acknowledge", code, env2["events"])
	}
	page, err := st.Inbox(context.Background(), store.InboxInput{
		TargetID: handlerTarget().ID, Account: "alice", Consumer: "ci", Limit: store.MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("direct inbox: %v", err)
	}
	if len(page.Events) != 4 {
		t.Errorf("stored pending events = %d, want 4", len(page.Events))
	}
}

// TestInboxExcludeSelf: default delivery excludes the stream's own account's
// self-authored objects; --exclude-self restores them. The contract holds in
// both.
func TestInboxExcludeSelf(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishMixedHandler(t, st, "alice")

	t.Run("default excludes own objects", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		events := env["events"].([]any)
		// 1 target + 3 rev-authored (thread, review, comment) = 4
		if len(events) != 4 {
			t.Fatalf("events = %d, want 4 (target + rev objects)", len(events))
		}
		for _, e := range events {
			if a, ok := e.(map[string]any)["author"].(string); ok && a == "alice" {
				t.Errorf("event has author alice, want filtered")
			}
		}
	})

	t.Run("--exclude-self restores own objects", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"inbox", "--consumer", "ci", "--exclude-self", "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		events := env["events"].([]any)
		// 1 target + 3 own + 3 rev (threads, reviews, comments) = 7
		if len(events) != 7 {
			t.Fatalf("events = %d, want 7 (target + all objects)", len(events))
		}
		var hasOwn bool
		for _, e := range events {
			if a, ok := e.(map[string]any)["author"].(string); ok && a == "alice" {
				hasOwn = true
			}
		}
		if !hasOwn {
			t.Error("--exclude-self did not restore the account's own objects")
		}
	})
}

// TestInboxSkipEmptyReviews: empty COMMENTED review containers are delivered
// by default; --skip-empty-reviews skips them at delivery while reviews with
// content or decision states still deliver.
func TestInboxSkipEmptyReviews(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	snap := &forge.Snapshot{
		Head:           "headA",
		CollectedStart: handlerBaseTime.Add(-time.Minute),
		CollectedEnd:   handlerBaseTime,
		Threads:        []forge.Thread{{ID: "t1", Author: "rev", Body: "fix this", Path: "a.go"}},
		Reviews: []forge.Review{
			{ID: "r1", Author: "rev", Body: "", State: "COMMENTED"},
			{ID: "r2", Author: "rev", Body: "", State: "APPROVED"},
			{ID: "r3", Author: "rev", Body: "real content", State: "COMMENTED"},
		},
	}
	if _, err := st.Publish(context.Background(), store.PublishInput{
		Target: handlerTarget(), Account: "alice", Snapshot: snap,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	t.Run("default delivers empty reviews", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		events := env["events"].([]any)
		// 1 target + 1 thread + 3 reviews = 5.
		if len(events) != 5 {
			t.Fatalf("events = %d, want 5 (target + thread + reviews)", len(events))
		}
		var hasR1 bool
		for _, e := range events {
			if m, ok := e.(map[string]any); ok && m["object_id"] == "r1" {
				hasR1 = true
			}
		}
		if !hasR1 {
			t.Error("empty review r1 not delivered by default")
		}
	})

	t.Run("--skip-empty-reviews skips empty COMMENTED containers", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"inbox", "--consumer", "ci", "--skip-empty-reviews", "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		events := env["events"].([]any)
		// r1 (empty COMMENTED) skipped: 1 target + 1 thread + r2 + r3 = 4.
		if len(events) != 4 {
			t.Fatalf("events = %d, want 4 (target + thread + r2 + r3)", len(events))
		}
		for _, e := range events {
			if m, ok := e.(map[string]any); ok && m["object_id"] == "r1" {
				t.Error("empty review r1 delivered under --skip-empty-reviews")
			}
		}
	})
}

func TestAckRejectsUnknownID(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	// One known ID plus one unknown ID rejects the whole request.
	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1", "--event=bogus", "--state-dir", dir, handlerURL,
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertContract(t, env)
	if env["error"].(map[string]any)["code"] != "unknown_event" {
		t.Errorf("error = %v, want unknown_event", env["error"])
	}
	// Nothing was acknowledged.
	page, err := st.Inbox(context.Background(), store.InboxInput{
		TargetID: handlerTarget().ID, Account: "alice", Consumer: "ci", Limit: store.MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("direct inbox: %v", err)
	}
	if len(page.Events) != 4 {
		t.Fatalf("pending events = %d, want 4 after rejected ack", len(page.Events))
	}

	// The valid request succeeds and is idempotent.
	code, env, _ = runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1", "--event=e1", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	if env["status"] != "ok" {
		t.Errorf("status = %v, want ok", env["status"])
	}
	page, err = st.Inbox(context.Background(), store.InboxInput{
		TargetID: handlerTarget().ID, Account: "alice", Consumer: "ci", Limit: store.MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("direct inbox: %v", err)
	}
	if len(page.Events) != 3 {
		t.Errorf("pending events = %d, want 3 after acking e1", len(page.Events))
	}
}

func TestWaitInvalidConsumerIsUsage(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	// A stored stream means the backlog inbox check runs before any
	// authentication, so the pinned-pattern rejection surfaces immediately.
	// The deadline must outlive store setup (open + account resolution);
	// the invalid consumer is rejected at the inbox check, so the wait
	// returns well before the deadline rather than waiting it out.
	code, env, _ := runCLI(t, []string{
		"wait", "--timeout", "5s", "--consumer", "bad name!", "--state-dir", dir, handlerURL,
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (%v)", code, env["error"])
	}
	assertContract(t, env)
	if env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("error = %v, want usage", env["error"])
	}
}

func TestAckUsageErrors(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	tests := []struct {
		name string
		argv []string
	}{
		{"missing consumer", []string{"ack", "--event", "e1", "--state-dir", dir, handlerURL}},
		{"missing events", []string{"ack", "--consumer", "ci", "--state-dir", dir, handlerURL}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, env, _ := runCLI(t, tc.argv)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error = %v, want usage", env["error"])
			}
		})
	}
}

// withStdin substitutes the package stdin reader for --events-from -.
func withStdin(t *testing.T, s string) {
	t.Helper()
	prev := stdin
	stdin = strings.NewReader(s)
	t.Cleanup(func() { stdin = prev })
}

// pendingCount returns the stream's stored pending event count.
func pendingCount(t *testing.T, st *store.Store) int {
	t.Helper()
	page, err := st.Inbox(context.Background(), store.InboxInput{
		TargetID: handlerTarget().ID, Account: "alice", Consumer: "ci", Limit: store.MaxInboxLimit,
	})
	if err != nil {
		t.Fatalf("direct inbox: %v", err)
	}
	return len(page.Events)
}

// assertAckReceipt checks the receipt echoes exactly the acknowledged IDs.
func assertAckReceipt(t *testing.T, env map[string]any, want ...string) {
	t.Helper()
	raw, ok := env["events"].([]any)
	if !ok || len(raw) != len(want) {
		t.Fatalf("events = %v, want %v", env["events"], want)
	}
	for i, w := range want {
		if raw[i] != w {
			t.Errorf("events[%d] = %v, want %q", i, raw[i], w)
		}
	}
}

func TestAckCommaSeparatedIDs(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1,e2", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	if env["status"] != "ok" {
		t.Errorf("status = %v, want ok", env["status"])
	}
	assertAckReceipt(t, env, "e1", "e2")
	if n := pendingCount(t, st); n != 2 {
		t.Errorf("pending events = %d, want 2 after comma ack", n)
	}
}

func TestAckCommaIDsDedupe(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1,e1", "--event=e1", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertAckReceipt(t, env, "e1")
	if n := pendingCount(t, st); n != 3 {
		t.Errorf("pending events = %d, want 3 after deduped ack", n)
	}
}

func TestAckEventWhitespaceAndEmptySegments(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	// Leading/trailing whitespace and empty segments are dropped; only
	// the real IDs are acknowledged.
	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event= e1 ,,e2 ", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	assertAckReceipt(t, env, "e1", "e2")
	if n := pendingCount(t, st); n != 2 {
		t.Errorf("pending events = %d, want 2 after whitespace/empty ack", n)
	}
}

func TestAckEventsFromStdin(t *testing.T) {
	t.Run("one id per line", func(t *testing.T) {
		dir, st, _ := handlerEnv(t)
		publishHandler(t, st, "alice")
		withStdin(t, "e1\ne2\n")

		code, env, _ := runCLI(t, []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		assertAckReceipt(t, env, "e1", "e2")
		if n := pendingCount(t, st); n != 2 {
			t.Errorf("pending events = %d, want 2 after stdin ack", n)
		}
	})

	t.Run("json array", func(t *testing.T) {
		dir, st, _ := handlerEnv(t)
		publishHandler(t, st, "alice")
		withStdin(t, `["e1", "e2"]`)

		code, env, _ := runCLI(t, []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertAckReceipt(t, env, "e1", "e2")
		if n := pendingCount(t, st); n != 2 {
			t.Errorf("pending events = %d, want 2 after json array ack", n)
		}
	})

	t.Run("inbox ids-only envelope", func(t *testing.T) {
		dir, st, _ := handlerEnv(t)
		publishHandler(t, st, "alice")
		// The full --ids-only envelope is consumed directly.
		_, _, inboxStdout, _ := runCLIStd(t, []string{
			"inbox", "--consumer", "ci", "--ids-only", "--state-dir", dir, handlerURL,
		})
		withStdin(t, inboxStdout)

		code, env, _ := runCLI(t, []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertAckReceipt(t, env, "e1", "e2", "e3", "e4")
		if n := pendingCount(t, st); n != 0 {
			t.Errorf("pending events = %d, want 0 after envelope ack", n)
		}
	})

	t.Run("blank line amid ids", func(t *testing.T) {
		dir, st, _ := handlerEnv(t)
		publishHandler(t, st, "alice")
		withStdin(t, "e1\n\ne2\n")

		code, env, _ := runCLI(t, []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		assertAckReceipt(t, env, "e1", "e2")
		if n := pendingCount(t, st); n != 2 {
			t.Errorf("pending events = %d, want 2 after blank-line ack", n)
		}
	})
}

func TestAckEventsFromCombinedWithEventFlag(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	withStdin(t, "e2\n")

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1", "--events-from", "-", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	assertAckReceipt(t, env, "e1", "e2")
	if n := pendingCount(t, st); n != 2 {
		t.Errorf("pending events = %d, want 2 after combined ack", n)
	}
}

func TestAckEventsFromUsageErrors(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	tests := []struct {
		name  string
		stdin string
		argv  []string
	}{
		{"empty stdin", "", []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		}},
		{"blank stdin", "\n  \n", []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		}},
		{"zero-id json array", "[]", []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		}},
		{"not a json array", "[\"e1\", 2]", []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		}},
		{"non-stdin value", "e1\n", []string{
			"ack", "--consumer", "ci", "--events-from", "file.txt", "--state-dir", dir, handlerURL,
		}},
		{"full event-records envelope", `{"events":[{"id":"e1","kind":"revision"},{"id":"e2","kind":"revision"}]}`, []string{
			"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withStdin(t, tc.stdin)
			code, env, _ := runCLI(t, tc.argv)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (%v)", code, env["error"])
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error = %v, want usage", env["error"])
			}
		})
	}
}

func TestAckEventsFromTruncatedEnvelope(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	withStdin(t, `{"events":["e1","e2"],"has_more":true}`)

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (%v)", code, env["error"])
	}
	if env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("error = %v, want usage", env["error"])
	}
	// Nothing was acknowledged.
	if n := pendingCount(t, st); n != 4 {
		t.Errorf("pending events = %d, want 4 after rejected truncated ack", n)
	}
}

func TestAckJSONFormsDropEmptyIDs(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	withStdin(t, `["e1", ""]`)

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	// The empty entry is dropped like a blank line, not forwarded to the store.
	assertAckReceipt(t, env, "e1")
	if n := pendingCount(t, st); n != 3 {
		t.Errorf("pending events = %d, want 3 after empty-entry json ack", n)
	}
}

func TestAckEventsFromDedupesAgainstEventFlag(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	// e1 arrives on both --event and stdin; the receipt and the pending
	// count must reflect one acknowledgement of each ID.
	withStdin(t, "e1\ne2\n")

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1", "--events-from", "-", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
	}
	assertContract(t, env)
	assertAckReceipt(t, env, "e1", "e2")
	if n := pendingCount(t, st); n != 2 {
		t.Errorf("pending events = %d, want 2 after cross-source dedupe ack", n)
	}
}

func TestAckEventFlagWithEmptyStdinIsUsageError(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")
	// The explicit stdin form is validated on its own: empty stdin is a
	// usage error even when --event supplied valid IDs.
	withStdin(t, "")

	code, env, _ := runCLI(t, []string{
		"ack", "--consumer", "ci", "--event=e1", "--events-from", "-", "--state-dir", dir, handlerURL,
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (%v)", code, env["error"])
	}
	if env["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("error = %v, want usage", env["error"])
	}
	// Nothing was acknowledged, including the valid --event ID.
	if n := pendingCount(t, st); n != 4 {
		t.Errorf("pending events = %d, want 4 after rejected empty-stdin ack", n)
	}
}

func TestWaitStatusLineOnStderr(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishHandler(t, st, "alice")

	code, env, _, stderr := runCLIStd(t, []string{
		"wait", "--timeout", "5s", "--consumer", "ci", "--state-dir", dir, handlerURL,
	})
	if code != 0 {
		t.Fatalf("exit = %d (%v)", code, env["error"])
	}
	if env["status"] != "events" {
		t.Fatalf("status = %v, want events", env["status"])
	}
	want := "git-feedback wait: status=events events=4 has_more=false\n"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
}
