package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// issueURL is the canonical issue URL. It shares the pull request number
// sequence on purpose: pull requests and issues never collide on a number.
const issueURL = "https://github.com/owner/name/issues/9"

// issueReconcileArgs builds the reconcile argv for the issue target.
func issueReconcileArgs(dir string) []string {
	return append([]string{"reconcile", issueURL, "--json"}, stateArg(dir)...)
}

// issueWaitArgs builds the wait argv for the issue target with the given
// flags.
func issueWaitArgs(dir string, flags ...string) []string {
	return append([]string{"wait", issueURL, "--consumer", consumer, "--json"}, append(flags, stateArg(dir)...)...)
}

// issueInboxArgs builds the inbox argv for the issue target.
func issueInboxArgs(dir string) []string {
	return append([]string{"inbox", issueURL, "--consumer", consumer, "--json"}, stateArg(dir)...)
}

// issueAckArgs builds the ack argv for one event ID on the issue target.
func issueAckArgs(dir, id string) []string {
	return append([]string{"ack", issueURL, "--consumer", consumer, "--event", id, "--json"}, stateArg(dir)...)
}

// eventObjectIDs returns the object ID of each event in the envelope.
func eventObjectIDs(t *testing.T, env map[string]json.RawMessage) []string {
	t.Helper()
	var evs []struct {
		ObjectID string `json:"object_id"`
	}
	if err := json.Unmarshal(env["events"], &evs); err != nil {
		t.Fatalf("decode events: %v (%v)", err, env["events"])
	}
	ids := make([]string, 0, len(evs))
	for _, ev := range evs {
		ids = append(ids, ev.ObjectID)
	}
	return ids
}

// TestIssueTargetEndToEnd: reconcile → wait → ack on an issue URL reads only
// the conversation comments endpoint, reports null heads, and delivers a
// newly posted comment as one comment event.
func TestIssueTargetEndToEnd(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, map[string]string{"GIT_FEEDBACK_MIN_INTERVAL": "100ms"})
	dir := testState(t)

	envOut := runOK(t, env, 90*time.Second, issueReconcileArgs(dir)...)
	assertStatusUpdated(t, envOut)
	if !nonNull(envOut, "target") {
		t.Fatalf("reconcile reported no target")
	}
	if nonNull(envOut, "observed_head") || nonNull(envOut, "expected_head") {
		t.Fatalf("heads = %s/%s, want null/null for an issue target", envOut["observed_head"], envOut["expected_head"])
	}
	for _, key := range []string{"head", "threads", "nested", "reviews"} {
		if n := stub.count(key); n != 0 {
			t.Fatalf("endpoint %q served %d requests, want 0 for an issue target", key, n)
		}
	}
	if stub.count("comments") == 0 {
		t.Fatalf("the conversation comments endpoint was never read")
	}

	// Drain the baseline (the target initial observation) so the next wait
	// reports only the new comment.
	for _, id := range eventIDs(t, runOK(t, env, 30*time.Second, issueInboxArgs(dir)...)) {
		runOK(t, env, 30*time.Second, issueAckArgs(dir, id)...)
	}

	// Post a reviewer comment alongside one authored by the stream account
	// (alice, the fake gh login). The default self filter must drop
	// alice's event and deliver only rev's, pinning the documented
	// delivery-time self filter on issue targets.
	stub.setComments(`[
		{"id":31,"user":{"login":"rev"},"body":"contract accepted","created_at":"2026-01-01T00:00:00Z","html_url":"https://github.com/owner/name/issues/9#issuecomment-31"},
		{"id":32,"user":{"login":"alice"},"body":"on it","created_at":"2026-01-01T00:01:00Z","html_url":"https://github.com/owner/name/issues/9#issuecomment-32"}
	]`)

	// The pull-request-only flags have nothing to act on here and are
	// accepted without changing the outcome.
	envOut = runOK(t, env, 60*time.Second,
		issueWaitArgs(dir, "--timeout", "30s", "--head", "deadbeef", "--skip-head-changed", "--skip-empty-reviews")...)
	if s := statusOf(t, envOut); s != "events" {
		t.Fatalf("wait status = %q, want events", s)
	}
	if nonNull(envOut, "observed_head") || nonNull(envOut, "expected_head") {
		t.Fatalf("heads = %s/%s, want null/null despite --head on an issue target", envOut["observed_head"], envOut["expected_head"])
	}
	kinds := eventKinds(t, envOut)
	if len(kinds) != 1 || kinds[0] != "initial_observation" {
		t.Fatalf("event kinds = %v, want one first-sight comment observation", kinds)
	}
	if ids := eventObjectIDs(t, envOut); len(ids) != 1 || ids[0] != "31" {
		t.Fatalf("event object IDs = %v, want [31] (the self-authored 32 is filtered by the default self filter)", ids)
	}
	if authors := eventAuthors(t, envOut); len(authors) != 1 || authors[0] != "rev" {
		t.Fatalf("event authors = %v, want [rev]", authors)
	}
	if stub.count("head") != 0 || stub.count("threads") != 0 || stub.count("reviews") != 0 {
		t.Fatalf("pull request endpoints were read for an issue target")
	}

	for _, id := range eventIDs(t, envOut) {
		runOK(t, env, 30*time.Second, issueAckArgs(dir, id)...)
	}
	if ids := eventIDs(t, runOK(t, env, 30*time.Second, issueInboxArgs(dir)...)); len(ids) != 0 {
		t.Fatalf("pending events after ack = %v, want none", ids)
	}
}
