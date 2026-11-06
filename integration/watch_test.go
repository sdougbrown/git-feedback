package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
)

func TestWatchResumesAndReplaysWithoutAck(t *testing.T) {
	stub := newStub(t)
	env := testEnv(fakeGHPath(t), stub.srv.URL, nil)
	dir := testState(t)
	seedPublished(t, env, dir)
	baseline := runOK(t, env, 30*time.Second, append([]string{"baseline", testURL}, stateArg(dir)...)...)
	cursor := stringField(t, baseline, "next_cursor")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mismatch := runCLI(ctx, t, filepath.Join(repoRoot, "bin", "git-feedback-test"), env, append([]string{"watch", testURL, "--after", cursor, "--account", "bob", "--stop-on-events"}, stateArg(dir)...)...)
	if mismatch.Code != 2 || statusOf(t, mismatch.Env) != "error" || errorCode(t, mismatch.Env) != "usage" {
		t.Fatalf("account mismatch = %+v", mismatch)
	}
	target, err := github.ParseTarget(testURL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Publish(context.Background(), store.PublishInput{Target: target, Account: "alice", Snapshot: &forge.Snapshot{Head: "h1", HasHead: true, Comments: []forge.Comment{{ID: "new", Author: "reviewer", Body: "feedback"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"watch", testURL, "--after", cursor, "--stop-on-events", "--timeout", "5s"}, stateArg(dir)...)
	first := runOK(t, env, 30*time.Second, args...)
	if statusOf(t, first) != "events" || len(eventIDs(t, first)) == 0 {
		t.Fatalf("first = %+v", first)
	}
	replay := runOK(t, env, 30*time.Second, args...)
	if eventIDs(t, first)[0] != eventIDs(t, replay)[0] {
		t.Fatalf("replay differs: %+v %+v", first, replay)
	}
	next := stringField(t, first, "next_cursor")
	result := runOK(t, env, 30*time.Second, append([]string{"watch", testURL, "--after", next, "--stop-on-events", "--timeout", "100ms"}, stateArg(dir)...)...)
	if statusOf(t, result) != "timeout" {
		t.Fatalf("resume = %+v", result)
	}
	streamArgs := append([]string{"watch", testURL, "--after", cursor, "--limit", "1", "--timeout", "200ms"}, stateArg(dir)...)
	cmd := exec.CommandContext(ctx, filepath.Join(repoRoot, "bin", "git-feedback-test"), streamArgs...)
	cmd.Env = env
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("continuous watch: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	count, last := 0, ""
	for {
		var envelope map[string]json.RawMessage
		if err := decoder.Decode(&envelope); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		last = statusOf(t, envelope)
		if last == "events" {
			count++
		}
	}
	if count < 2 || last != "timeout" {
		t.Fatalf("continuous output = %s", output)
	}
	cmd = exec.CommandContext(ctx, filepath.Join(repoRoot, "bin", "git-feedback-test"), append([]string{"watch", testURL, "--after", cursor, "--limit", "1"}, stateArg(dir)...)...)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read streamed batch: %v", err)
	}
	var batch map[string]json.RawMessage
	if err := json.Unmarshal(line, &batch); err != nil || statusOf(t, batch) != "events" {
		t.Fatalf("first streamed line: %s (%v)", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 143 {
		t.Fatalf("watch signal result = %v, want exit 143", err)
	}
	runOK(t, env, 30*time.Second, append([]string{"store", "reset", "--target", testURL}, stateArg(dir)...)...)
	stale := runCLI(ctx, t, filepath.Join(repoRoot, "bin", "git-feedback-test"), env, append([]string{"watch", testURL, "--after", cursor, "--stop-on-events", "--timeout", "5s"}, stateArg(dir)...)...)
	if stale.Code != 2 || statusOf(t, stale.Env) != "error" || errorCode(t, stale.Env) != "usage" {
		t.Fatalf("stale cursor = %+v", stale)
	}
}
