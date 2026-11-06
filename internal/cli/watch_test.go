package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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
