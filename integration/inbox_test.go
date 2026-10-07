package integration

import (
	"testing"
	"time"
)

// TestInboxOffline: an inbox-only watcher is blind — the local store
// never refreshes by itself; reading the inbox makes no request.
func TestInboxOffline(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	seedPublished(t, env, dir)

	before := snapshotCounts(stub)
	envOut := runOK(t, env, 30*time.Second, inboxArgs(dir)...)
	assertNoRequests(t, stub, before)

	if s := statusOf(t, envOut); s != "ok" {
		t.Fatalf("inbox status = %q, want ok", s)
	}
	if ids := eventIDs(t, envOut); len(ids) == 0 {
		t.Fatalf("inbox delivered no events")
	}
}
