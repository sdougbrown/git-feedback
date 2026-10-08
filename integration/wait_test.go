package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const consumer = "ci"

// testState returns a fresh state directory for one test.
func testState(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

// reconcileArgs builds the reconcile argv for the test target.
func reconcileArgs(dir string) []string {
	return append([]string{"reconcile", testURL, "--json"}, stateArg(dir)...)
}

// waitArgs builds the wait argv for the test target with the given flags.
func waitArgs(dir string, flags ...string) []string {
	return append([]string{"wait", testURL, "--consumer", consumer, "--json"}, append(flags, stateArg(dir)...)...)
}

// inboxArgs builds the inbox argv for the test target.
func inboxArgs(dir string) []string {
	return append([]string{"inbox", testURL, "--consumer", consumer, "--json"}, stateArg(dir)...)
}

// ackArgs builds the ack argv for one event ID.
func ackArgs(dir, id string) []string {
	return append([]string{"ack", testURL, "--consumer", consumer, "--event", id, "--json"}, stateArg(dir)...)
}

// assertStatusUpdated fails unless the envelope reports an updated publish.
func assertStatusUpdated(t *testing.T, env map[string]json.RawMessage) {
	t.Helper()
	if s := statusOf(t, env); s != "updated" {
		t.Fatalf("reconcile status = %q, want updated", s)
	}
}

// seedPublished runs one reconcile against the stub, requiring a publish.
func seedPublished(t *testing.T, env []string, dir string) {
	t.Helper()
	assertStatusUpdated(t, runOK(t, env, 90*time.Second, reconcileArgs(dir)...))
}

// ackAll acknowledges every currently pending event for the consumer.
func ackAll(t *testing.T, env []string, dir string) {
	t.Helper()
	page := runOK(t, env, 30*time.Second, inboxArgs(dir)...)
	for _, id := range eventIDs(t, page) {
		runOK(t, env, 30*time.Second, ackArgs(dir, id)...)
	}
}

// snapshotCounts copies the stub's request counters.
func snapshotCounts(stub *ghStub) map[string]int {
	return map[string]int{
		"verify":   stub.count("verify"),
		"head":     stub.count("head"),
		"threads":  stub.count("threads"),
		"reviews":  stub.count("reviews"),
		"comments": stub.count("comments"),
	}
}

// assertNoRequests fails when any counted endpoint received new requests
// since the baseline, proving the command stayed offline.
func assertNoRequests(t *testing.T, stub *ghStub, before map[string]int) {
	t.Helper()
	for key, n := range before {
		if got := stub.count(key); got != n {
			t.Fatalf("endpoint %q received %d requests after baseline %d; command touched the network", key, got, n)
		}
	}
}

// TestWaitColdStart: an empty store with no stored account never guesses.
// The first cycle bootstraps, verifies, collects, and publishes; the
// initial observation is delivered as pending events.
func TestWaitColdStart(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)

	envOut := runOK(t, env, 90*time.Second, waitArgs(dir, "--timeout", "60s")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	kinds := eventKinds(t, envOut)
	if len(kinds) == 0 {
		t.Fatalf("wait delivered no events")
	}
	for _, k := range kinds {
		if k != "initial_observation" {
			t.Fatalf("event kinds = %v, want initial observations", kinds)
		}
	}
	if acct := stringField(t, envOut, "account"); acct != "alice" {
		t.Fatalf("account = %q, want alice", acct)
	}
	if !nonNull(envOut, "snapshot") {
		t.Fatalf("wait on cold start delivered no snapshot")
	}
}

// TestPinnedWaitDeliversHistoricalBacklog: pending events are returned
// immediately without remote authentication; the stored observed head and
// the requested expected head travel with stale set on mismatch.
func TestPinnedWaitDeliversHistoricalBacklog(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	seedPublished(t, env, dir)

	before := snapshotCounts(stub)
	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--head", "pinhead", "--timeout", "5s")...)
	assertNoRequests(t, stub, before)

	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	if head := stringField(t, envOut, "observed_head"); head != "h1" {
		t.Fatalf("observed_head = %q, want h1", head)
	}
	if head := stringField(t, envOut, "expected_head"); head != "pinhead" {
		t.Fatalf("expected_head = %q, want pinhead", head)
	}
	if !staleField(t, envOut) {
		t.Fatalf("stale = false, want true when the stored head differs from --head")
	}
	if ids := eventIDs(t, envOut); len(ids) == 0 {
		t.Fatalf("wait delivered no events")
	}
}

// TestLostOutputReplays: dropping the first process's entire result still
// lets a second process receive the same event, delivered offline from the
// stored backlog.
func TestLostOutputReplays(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)

	// The first process's output is dropped entirely.
	seedPublished(t, env, dir)

	before := snapshotCounts(stub)
	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--timeout", "5s")...)
	assertNoRequests(t, stub, before)

	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	if ids := eventIDs(t, envOut); len(ids) == 0 {
		t.Fatalf("second process received no events; the dropped output was not replayable")
	}
}

// TestHeadOnlyChangeLostOutputReplays: after the earlier event is acked, a
// later head-only revision stays pending and is delivered to the next
// process even when the publishing process's output was lost.
func TestHeadOnlyChangeLostOutputReplays(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, map[string]string{"GIT_FEEDBACK_MIN_INTERVAL": "100ms"})
	dir := testState(t)

	seedPublished(t, env, dir)
	ackAll(t, env, dir)

	// A head-only change publishes the revision; its output is dropped.
	stub.setHead("h2")
	assertStatusUpdated(t, runOK(t, env, 90*time.Second, reconcileArgs(dir)...))

	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--timeout", "5s")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	kinds := eventKinds(t, envOut)
	if len(kinds) != 1 || kinds[0] != "head_changed" {
		t.Fatalf("event kinds = %v, want [head_changed] (the later revision stays pending after the ack)", kinds)
	}
	if head := stringField(t, envOut, "observed_head"); head != "h2" {
		t.Fatalf("observed_head = %q, want h2", head)
	}
}

// TestWaitDoesNotAck: delivery is not acknowledgement; a second read of the
// same inbox still returns the same pending events.
func TestWaitDoesNotAck(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	seedPublished(t, env, dir)

	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--timeout", "5s")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	delivered := eventIDs(t, envOut)
	if len(delivered) == 0 {
		t.Fatalf("wait delivered no events")
	}
	page := runOK(t, env, 30*time.Second, inboxArgs(dir)...)
	if again := eventIDs(t, page); len(again) != len(delivered) {
		t.Fatalf("inbox after wait has %d pending events, want %d: wait acknowledged delivery", len(again), len(delivered))
	}
}

// TestWaitFatalOnHeadMismatch: with --head and no pending backlog, a pinned
// head mismatch ends the wait immediately with both heads and no collection.
func TestWaitFatalOnHeadMismatch(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, map[string]string{"GIT_FEEDBACK_MIN_INTERVAL": "100ms"})
	dir := testState(t)
	seedPublished(t, env, dir)
	ackAll(t, env, dir)

	beforeThreads := stub.count("threads")
	beforeHead := stub.count("head")
	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--head", "deadbeef", "--timeout", "10s")...)

	if s := statusOf(t, envOut); s != "head_changed" {
		t.Fatalf("wait status = %q, want head_changed", s)
	}
	if head := stringField(t, envOut, "observed_head"); head != "h1" {
		t.Fatalf("observed_head = %q, want h1", head)
	}
	if head := stringField(t, envOut, "expected_head"); head != "deadbeef" {
		t.Fatalf("expected_head = %q, want deadbeef", head)
	}
	if got := stub.count("threads"); got != beforeThreads {
		t.Fatalf("threads requests = %d, want %d: mismatch must not collect feedback", got, beforeThreads)
	}
	if got := stub.count("head"); got != beforeHead+1 {
		t.Fatalf("head requests = %d, want %d: exactly the pinned-head check", got, beforeHead+1)
	}
}

// TestWaitDeadlineBoundsAllWork: a wait with a 2s deadline against a hanging
// remote exits promptly with the timeout status, not a hang.
func TestWaitDeadlineBoundsAllWork(t *testing.T) {
	stub := newStub(t)
	release := stub.hangVerification()
	t.Cleanup(func() { close(release) })
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)

	start := time.Now()
	envOut := runOK(t, env, 10*time.Second, waitArgs(dir, "--timeout", "2s")...)
	elapsed := time.Since(start)

	if s := statusOf(t, envOut); s != "timeout" {
		t.Fatalf("wait status = %q, want timeout", s)
	}
	// Durable evidence the wait actually entered the (hanging) verification
	// that the deadline had to bound: the stub observed the verify request.
	if got := stub.count("verify"); got < 1 {
		t.Fatalf("verify requests = %d, want >= 1: the wait never reached the hung verification", got)
	}
	// The deadline bounds all work: a 2s wait must not run grossly past its
	// deadline, but subprocess + fake-gh startup on a loaded runner can add
	// headroom, so allow up to 6s. A grossly exceeded deadline still fails.
	if elapsed > 6*time.Second {
		t.Fatalf("wait with a 2s deadline exited after %v, want under 6s", elapsed)
	}
}

// TestWaitDeadlineWhileStoreBusy: a second process holding the SQLite write
// lock cannot hold the wait past its deadline; lock waits are bounded by
// the remaining deadline, not the default 5000ms alone.
func TestWaitDeadlineWhileStoreBusy(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)

	// Initialize the store schema before taking the write lock so the wait
	// fails only on lock contention, not on a blocked migration.
	initStore(t, env, dir)

	// Hold a write transaction on the store's database for the whole run.
	lock := holdWriteLock(t, dir)
	defer lock()

	start := time.Now()
	envOut := runOK(t, env, 10*time.Second, waitArgs(dir, "--timeout", "2s")...)
	elapsed := time.Since(start)

	if s := statusOf(t, envOut); s != "timeout" {
		t.Fatalf("wait status = %q, want timeout (stderr has no bearing)", s)
	}
	// The 2s deadline must fire before the 5s SQLite busy timeout; the
	// status check above is the durable evidence that the deadline (not the
	// busy timeout) dominated. The 4.5s bound leaves headroom for startup
	// variance while still failing if the busy timeout were reached.
	if elapsed > 4500*time.Millisecond {
		t.Fatalf("wait under a busy store exited after %v, want under 4.5s (the 5s busy timeout must not dominate)", elapsed)
	}
}

// runSharedCadenceScenario runs the shared prefix for cadence and
// session-reuse tests: seed, ack, then a wait with the given timeout.
// It asserts the wait exits with timeout status and that exactly one
// verify request was made (session reuse). It returns the envelope, the
// stub, the pre-wait snapshot, and the start time for timing assertions.
func runSharedCadenceScenario(t *testing.T, timeout string) (map[string]json.RawMessage, *ghStub, map[string]int, time.Time) {
	t.Helper()
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, map[string]string{"GIT_FEEDBACK_MIN_INTERVAL": "2s"})
	dir := testState(t)
	seedPublished(t, env, dir)
	ackAll(t, env, dir)

	before := snapshotCounts(stub)
	start := time.Now()
	envOut := runOK(t, env, 90*time.Second, waitArgs(dir, "--timeout", timeout)...)
	if s := statusOf(t, envOut); s != "timeout" {
		t.Fatalf("wait status = %q, want timeout", s)
	}
	// One verification for the wait's own first cycle (the seed's is
	// separate): a busy loop would re-authenticate per cycle.
	if got := stub.count("verify") - before["verify"]; got != 1 {
		t.Fatalf("wait verify requests = %d, want 1: cycles re-authenticated (timeline: %v)", got, timeline(t, stub))
	}
	return envOut, stub, before, start
}

// TestWaitUsesSharedCadence: repeated wait cycles honor the persisted
// cadence (no busy-looping) and reuse the verified session instead of
// re-admitting per cycle.
func TestWaitUsesSharedCadence(t *testing.T) {
	_, stub, _, start := runSharedCadenceScenario(t, "15s")

	// The cadence invariant, load-tolerant: every gap between consecutive
	// collection requests is at least the persisted cadence (2s) minus
	// timing slop. A cycle that overruns the cadence can wake early (its
	// persisted NextDue is already past), so the count is not capped; a
	// back-to-back burst (gap under ~1.5s) is a busy loop and fails.
	times := stub.collectionTimesSince("threads", start)
	if len(times) < 1 {
		t.Fatalf("wait collections = 0, want >= 1 (timeline: %v)", timeline(t, stub))
	}
	const minGap = 1500 * time.Millisecond
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < minGap {
			t.Fatalf("collection gap = %v < %v: cadence not honored (timeline: %v)", gap, minGap, timeline(t, stub))
		}
	}
}

// TestWaitReusesVerifiedSession: the wait's first cycle verifies once; every
// later cycle in the same invocation collects without re-verifying.
func TestWaitReusesVerifiedSession(t *testing.T) {
	_, stub, before, _ := runSharedCadenceScenario(t, "14s")

	if got := stub.count("threads") - before["threads"]; got < 2 {
		t.Fatalf("wait completed %d collections, want at least 2 (the second reusing the session) (counts: %v)", got, snapshotCounts(stub))
	}
}

// TestWaitHonorsPersistedRateGate: a persisted GraphQL gate blocks even the
// viewer verification of a fresh caller; wait retries until the deadline
// without issuing a single GraphQL request.
func TestWaitHonorsPersistedRateGate(t *testing.T) {
	stub := newStub(t)
	stub.exhaustGraphQL(5, time.Now().Add(time.Hour))
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)

	// A reconcile persists the exhausted gate (and defers); it collects
	// nothing.
	runOK(t, env, 30*time.Second, reconcileArgs(dir)...)

	before := snapshotCounts(stub)
	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--timeout", "2s")...)
	assertNoRequests(t, stub, before)

	if s := statusOf(t, envOut); s != "timeout" {
		t.Fatalf("wait status = %q, want timeout", s)
	}
}

// TestWaitExcludeSelf: default wait delivery excludes the stream's own
// account's self-authored objects; --exclude-self restores them. The
// backlog is delivered without running a cycle, so no bootstrap lease is
// needed. Reading never acknowledges, so the second run (with the flag)
// sees the full unfiltered backlog — the designed behavior.
func TestWaitExcludeSelf(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	publishMixed(t, dir, "alice")

	// Default: the account's own objects are filtered; the rev-authored
	// comment and the target event are delivered.
	envOut := runOK(t, env, 30*time.Second, waitArgs(dir, "--timeout", "5s")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	authors := eventAuthors(t, envOut)
	for _, a := range authors {
		if a == "alice" {
			t.Fatalf("event has author alice, want filtered (authors: %v)", authors)
		}
	}
	if len(authors) != 2 {
		t.Fatalf("events = %d, want 2 (target + rev comment)", len(authors))
	}

	// --exclude-self: the account's own objects are restored. Reading never
	// acknowledges, so the same consumer sees the full unfiltered backlog.
	envOut = runOK(t, env, 30*time.Second, waitArgs(dir, "--timeout", "5s", "--exclude-self")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	authors = eventAuthors(t, envOut)
	hasOwn := false
	for _, a := range authors {
		if a == "alice" {
			hasOwn = true
		}
	}
	if !hasOwn {
		t.Fatalf("--exclude-self did not restore the account's own objects (authors: %v)", authors)
	}
	if len(authors) != 3 {
		t.Fatalf("events = %d, want 3 (target + own + rev)", len(authors))
	}
}

// TestWaitStatusLineTimeout: the stderr status summary reflects the timeout
// outcome without parsing the envelope. The backlog is drained first so the
// wait runs out the deadline against the persisted schedule; the fake gh and
// local stub keep the cycle hermetic.
func TestWaitStatusLineTimeout(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	seedPublished(t, env, dir)
	ackAll(t, env, dir)

	_, tagged := binPaths(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := runCLI(ctx, t, tagged, env, waitArgs(dir, "--timeout", "5s")...)
	if res.Code != 0 {
		t.Fatalf("exit = %d (stderr %q)", res.Code, res.Stderr)
	}
	var envOut map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Stdout), &envOut); err != nil {
		t.Fatalf("decode envelope: %v (%q)", err, res.Stdout)
	}
	if s := statusOf(t, envOut); s != "timeout" {
		t.Fatalf("wait status = %q, want timeout", s)
	}
	want := "git-feedback wait: status=timeout events=0 has_more=false\n"
	if !strings.Contains(res.Stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", res.Stderr, want)
	}
}
