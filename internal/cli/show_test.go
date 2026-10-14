package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/store"
)

// publishShowHandler publishes a review whose body carries a completion
// marker, the shape the show command exists to serve.
func publishShowHandler(t *testing.T, st *store.Store) {
	t.Helper()
	snap := &forge.Snapshot{
		Head:           "headA",
		CollectedStart: handlerBaseTime.Add(-time.Minute),
		CollectedEnd:   handlerBaseTime,
		Reviews: []forge.Review{
			{ID: "r1", Author: "rev", Body: "<!-- umpire:round=1 head=headA verdict=COMMENT -->", State: "COMMENTED"},
		},
	}
	if _, err := st.Publish(context.Background(), store.PublishInput{
		Target: handlerTarget(), Account: "alice", Snapshot: snap,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// reviewEventID returns the review r1 event's ID for the consumer.
func reviewEventID(t *testing.T, dir, consumer string) string {
	t.Helper()
	code, env, _ := runCLI(t, []string{"inbox", "--consumer", consumer, "--state-dir", dir, handlerURL})
	if code != 0 {
		t.Fatalf("inbox exit = %d (%v)", code, env["error"])
	}
	for _, e := range env["events"].([]any) {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if m["object_id"] == "r1" {
			id, _ := m["id"].(string)
			if id != "" {
				return id
			}
		}
	}
	t.Fatal("no event for review r1")
	return ""
}

// TestShow: the event's object content is delivered in the envelope, the
// event record renders in events, and the command is local-only.
func TestShow(t *testing.T) {
	dir, st, _ := handlerEnv(t)
	publishShowHandler(t, st)
	st.Close()
	eventID := reviewEventID(t, dir, "ci")

	t.Run("delivers event and object", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"show", "--event", eventID, "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (%v)", code, env["error"])
		}
		assertContract(t, env)
		if env["status"] != "ok" {
			t.Errorf("status = %v, want ok", env["status"])
		}
		events := env["events"].([]any)
		if len(events) != 1 {
			t.Fatalf("events = %d records, want 1", len(events))
		}
		rec := events[0].(map[string]any)
		if rec["id"] != eventID || rec["object_id"] != "r1" || rec["object_kind"] != "review" {
			t.Errorf("event record = %v", rec)
		}
		obj, ok := env["object"].(map[string]any)
		if !ok {
			t.Fatalf("object = %v, want an object", env["object"])
		}
		if obj["id"] != "r1" || obj["state"] != "COMMENTED" {
			t.Errorf("object = %v", obj)
		}
		if body, _ := obj["body"].(string); !strings.Contains(body, "head=headA") {
			t.Errorf("object body = %q, want the completion marker", body)
		}
	})

	t.Run("object is null for the target event", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"inbox", "--consumer", "ci", "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("inbox exit = %d", code)
		}
		var targetID string
		for _, e := range env["events"].([]any) {
			if m, ok := e.(map[string]any); ok && m["object_kind"] == "target" {
				targetID, _ = m["id"].(string)
			}
		}
		if targetID == "" {
			t.Fatal("no target event")
		}
		code, env, _ = runCLI(t, []string{"show", "--event", targetID, "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("exit = %d (%v)", code, env["error"])
		}
		if env["object"] != nil {
			t.Errorf("object = %v, want null", env["object"])
		}
	})

	t.Run("requires --event", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"show", "--state-dir", dir, handlerURL})
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		if env["error"].(map[string]any)["code"] != "usage" {
			t.Errorf("error.code = %v, want usage", env["error"])
		}
	})

	t.Run("malformed event ID is a usage error", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"show", "--event", "x1", "--state-dir", dir, handlerURL})
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		err := env["error"].(map[string]any)
		if err["code"] != "usage" || !strings.Contains(err["message"].(string), "e<digits>") {
			t.Errorf("error = %v, want usage naming e<digits>", err)
		}
	})

	t.Run("unknown event is operational", func(t *testing.T) {
		code, env, _ := runCLI(t, []string{"show", "--event", "e999", "--state-dir", dir, handlerURL})
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		err := env["error"].(map[string]any)
		if err["code"] != "unknown_event" {
			t.Errorf("error.code = %v, want unknown_event", err["code"])
		}
	})

	t.Run("envelope pipes into ack --events-from -", func(t *testing.T) {
		// The consumer has not acked yet, so the event is still pending; show
		// it, then feed the show envelope back through ack.
		code, _, stdout := runCLI(t, []string{"show", "--event", eventID, "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("show exit = %d", code)
		}
		withStdin(t, stdout)
		code, env, _ := runCLI(t, []string{"ack", "--consumer", "ci", "--events-from", "-", "--state-dir", dir, handlerURL})
		if code != 0 {
			t.Fatalf("ack exit = %d (%v)", code, env["error"])
		}
		ids := env["events"].([]any)
		if len(ids) != 1 || ids[0] != eventID {
			t.Errorf("acked events = %v, want [%s]", ids, eventID)
		}
	})
}
