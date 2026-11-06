package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type failingWatchWriter struct{ calls int }

func (w *failingWatchWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("broken output")
}

func TestWatchStopsWritingAfterOutputFailure(t *testing.T) {
	out := &failingWatchWriter{}
	var errout bytes.Buffer
	code := Run([]string{"watch", "https://github.com/a/b/pull/1", "--after", "bad"}, out, &errout)
	if code != ExitOperational || out.calls != 1 {
		t.Fatalf("exit = %d, writes = %d, stderr = %q", code, out.calls, errout.String())
	}
}

func TestWatchWritesErrorEnvelopeForInvalidCursor(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run([]string{"watch", "https://github.com/a/b/pull/1", "--after", "bad"}, &out, &errout)
	var result Result
	if err := json.Unmarshal(out.Bytes(), &result); code != ExitUsage || err != nil || result.Error == nil || result.Error.Code != "usage" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, decode = %v", code, out.String(), errout.String(), err)
	}
}

func TestWatchRejectsMissingPosition(t *testing.T) {
	var out, errout bytes.Buffer
	code := Run([]string{"watch", "https://github.com/a/b/pull/1", "--stop-on-events"}, &out, &errout)
	if code != ExitUsage {
		t.Fatalf("exit = %d, output %s", code, out.String())
	}
	var result Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Error == nil {
		t.Fatalf("invalid error envelope: %s (%v)", out.String(), err)
	}
	if !strings.Contains(result.Error.Message, "--after") {
		t.Fatalf("missing cursor guidance: %s", out.String())
	}
}
