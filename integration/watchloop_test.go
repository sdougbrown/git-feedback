package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWatchLoopScript: examples/watch-loop.sh hoists the shared options
// into both wait and ack, acks the delivered events, and loops; argument
// validation exits 2. The fake git-feedback sequences the wait responses
// (events, timeout, then a sentinel and exit 42) so the loop terminates
// deterministically.
func TestWatchLoopScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not on PATH: %v", err)
	}
	script := filepath.Join(repoRoot, "examples", "watch-loop.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("examples/watch-loop.sh not found: %v", err)
	}

	dir := testState(t)

	// installShim writes a fake git-feedback (the canned envelopes are the
	// body) into its own temp dir and returns a run function that invokes
	// the script with the shim prepended to PATH, plus the argv log path.
	// jq and bash come from the inherited PATH.
	installShim := func(t *testing.T, body string) (func(...string) (int, string), string) {
		t.Helper()
		shimDir := t.TempDir()
		logFile := filepath.Join(shimDir, "shim.log")
		shim := "#!/usr/bin/env bash\n" +
			"LOG=" + logFile + "\n" +
			"printf '%s\\n' \"$*\" >> \"$LOG\"\n" +
			body
		if err := os.WriteFile(filepath.Join(shimDir, "git-feedback"), []byte(shim), 0o700); err != nil {
			t.Fatalf("write shim: %v", err)
		}
		var env []string
		for _, e := range os.Environ() {
			if strings.HasPrefix(e, "PATH=") {
				env = append(env, "PATH="+shimDir+string(os.PathListSeparator)+strings.TrimPrefix(e, "PATH="))
			} else {
				env = append(env, e)
			}
		}
		run := func(args ...string) (int, string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", script)
			cmd.Args = append(cmd.Args, args...)
			cmd.Env = env
			var out, errb strings.Builder
			cmd.Stdout = &out
			cmd.Stderr = &errb
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("script %v outlived the deadline (stdout=%q stderr=%q)", args, out.String(), errb.String())
			}
			code := 0
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("run script: %v (stderr=%q)", err, errb.String())
			}
			return code, errb.String()
		}
		return run, logFile
	}

	// readLog returns the argv log lines and the indices of the wait and
	// ack invocations.
	readLog := func(t *testing.T, logFile string) ([]string, []int, []int) {
		t.Helper()
		data, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("read shim log: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		var waitIdx, ackIdx []int
		for i, line := range lines {
			switch {
			case strings.HasPrefix(line, "wait "):
				waitIdx = append(waitIdx, i)
			case strings.HasPrefix(line, "ack "):
				ackIdx = append(ackIdx, i)
			}
		}
		return lines, waitIdx, ackIdx
	}

	t.Run("happy path", func(t *testing.T) {
		body := `case "$1" in
  wait)
    n=$(grep -c '^wait ' "$LOG")
    if [ "$n" -eq 1 ]; then
      printf '{"status":"events","events":[{"id":"e1"},{"id":"e2"}]}\n'
    elif [ "$n" -eq 2 ]; then
      printf '{"status":"timeout"}\n'
    else
      printf 'sentinel\n' >> "$LOG"
      exit 42
    fi
    ;;
  ack)
    printf '{"status":"ok"}\n'
    ;;
esac
`
		run, logFile := installShim(t, body)
		code, errb := run(testURL, "--consumer", consumer, "--state-dir", dir, "--account", "alice")
		if code != 42 {
			t.Fatalf("script exit = %d, want 42 (stderr=%q)", code, errb)
		}

		lines, waitIdx, ackIdx := readLog(t, logFile)
		if len(waitIdx) != 3 {
			t.Fatalf("wait invocations = %d, want 3 (log: %q)", len(waitIdx), strings.Join(lines, " | "))
		}
		if len(ackIdx) != 1 {
			t.Fatalf("ack invocations = %d, want 1 (log: %q)", len(ackIdx), strings.Join(lines, " | "))
		}

		// (a) The first wait carries the URL, the shared options, and --json.
		for _, want := range []string{testURL, "--consumer", consumer, "--state-dir", dir, "--account", "alice", "--json"} {
			if !strings.Contains(lines[waitIdx[0]], want) {
				t.Fatalf("first wait %q missing %q", lines[waitIdx[0]], want)
			}
		}

		// (b) Exactly one ack, with both events in order and the same shared
		// options as the wait.
		for _, want := range []string{testURL, "--consumer", consumer, "--state-dir", dir, "--account", "alice"} {
			if !strings.Contains(lines[ackIdx[0]], want) {
				t.Fatalf("ack %q missing %q", lines[ackIdx[0]], want)
			}
		}
		if !strings.Contains(lines[ackIdx[0]], "--event e1 --event e2") {
			t.Fatalf("ack %q does not carry --event e1 --event e2 in order", lines[ackIdx[0]])
		}

		// (c) The second wait follows the ack, and the third wait ran (the
		// sentinel is the shim's marker for it).
		if waitIdx[1] < ackIdx[0] {
			t.Fatalf("second wait (line %d) does not follow the ack (line %d)", waitIdx[1], ackIdx[0])
		}
		if !strings.Contains(strings.Join(lines, "\n"), "sentinel") {
			t.Fatalf("shim log missing the sentinel: the third wait never ran")
		}

		// Argument validation exits 2.
		if code, errb := run(); code != 2 {
			t.Fatalf("missing URL: exit = %d, want 2 (stderr=%q)", code, errb)
		}
		if code, errb := run("--timeout", "5s", testURL, "--consumer", consumer); code != 2 {
			t.Fatalf("flag before URL: exit = %d, want 2 (stderr=%q)", code, errb)
		}
		if code, errb := run(testURL); code != 2 {
			t.Fatalf("missing --consumer: exit = %d, want 2 (stderr=%q)", code, errb)
		}
	})

	t.Run("ack failure", func(t *testing.T) {
		// The first ack call fails (error envelope, exit 1) while wait
		// behaves normally. The loop must stop on the failed ack: no second
		// wait.
		body := `case "$1" in
  wait)
    n=$(grep -c '^wait ' "$LOG")
    if [ "$n" -eq 1 ]; then
      printf '{"status":"events","events":[{"id":"e1"},{"id":"e2"}]}\n'
    else
      printf '{"status":"timeout"}\n'
    fi
    ;;
  ack)
    n=$(grep -c '^ack ' "$LOG")
    if [ "$n" -eq 1 ]; then
      printf '{"status":"error","message":"boom"}\n'
      exit 1
    fi
    printf '{"status":"ok"}\n'
    ;;
esac
`
		run, logFile := installShim(t, body)
		code, errb := run(testURL, "--consumer", consumer, "--state-dir", dir, "--account", "alice")
		if code != 1 {
			t.Fatalf("script exit = %d, want 1 (stderr=%q)", code, errb)
		}
		if !strings.Contains(errb, "watch-loop: ack failed") {
			t.Fatalf("stderr %q missing %q", errb, "watch-loop: ack failed")
		}

		lines, waitIdx, ackIdx := readLog(t, logFile)
		if len(ackIdx) != 1 {
			t.Fatalf("ack invocations = %d, want 1 (log: %q)", len(ackIdx), strings.Join(lines, " | "))
		}
		if len(waitIdx) != 1 {
			t.Fatalf("wait invocations = %d, want 1 (the loop stopped after the failed ack; log: %q)", len(waitIdx), strings.Join(lines, " | "))
		}
	})

	t.Run("passthrough flags", func(t *testing.T) {
		// Happy path with a wait-only flag after the URL. The flag must
		// reach wait but not ack.
		body := `case "$1" in
  wait)
    n=$(grep -c '^wait ' "$LOG")
    if [ "$n" -eq 1 ]; then
      printf '{"status":"events","events":[{"id":"e1"}]}\n'
    elif [ "$n" -eq 2 ]; then
      printf '{"status":"timeout"}\n'
    else
      printf 'sentinel\n' >> "$LOG"
      exit 42
    fi
    ;;
  ack)
    printf '{"status":"ok"}\n'
    ;;
esac
`
		run, logFile := installShim(t, body)
		code, errb := run(testURL, "--consumer", consumer, "--state-dir", dir, "--account", "alice", "--timeout", "5s")
		if code != 42 {
			t.Fatalf("script exit = %d, want 42 (stderr=%q)", code, errb)
		}

		lines, waitIdx, ackIdx := readLog(t, logFile)
		if len(waitIdx) != 3 {
			t.Fatalf("wait invocations = %d, want 3 (log: %q)", len(waitIdx), strings.Join(lines, " | "))
		}
		if len(ackIdx) != 1 {
			t.Fatalf("ack invocations = %d, want 1 (log: %q)", len(ackIdx), strings.Join(lines, " | "))
		}

		// The first wait carries the wait-only flag.
		if !strings.Contains(lines[waitIdx[0]], "--timeout 5s") {
			t.Fatalf("first wait %q missing %q", lines[waitIdx[0]], "--timeout 5s")
		}
		// The ack does not carry the wait-only flag.
		for _, notWant := range []string{"--timeout", "5s"} {
			if strings.Contains(lines[ackIdx[0]], notWant) {
				t.Fatalf("ack %q must not contain %q", lines[ackIdx[0]], notWant)
			}
		}
	})
}
