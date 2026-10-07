package integration

// Stage 6 registry smoke tests: exercise the shipped cli-registry manifest
// through the reference cli-registry binary. These tests skip unless
// CLI_REGISTRY_BIN is set. The shipped manifest is validated with the
// production binary on PATH; fake-server calls run through a per-test copy
// of the manifest whose command[0] points at the tagged binary and whose env
// forwards the testhook overrides.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// registryBin returns the reference cli-registry binary, skipping the test
// when CLI_REGISTRY_BIN is unset.
func registryBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("CLI_REGISTRY_BIN")
	if bin == "" {
		t.Skip("CLI_REGISTRY_BIN not set; skipping registry smoke test")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("CLI_REGISTRY_BIN %s: %v", bin, err)
	}
	return bin
}

// registryManifest writes a per-test copy of the shipped manifest with every
// tool's command[0] rewritten to the tagged binary and test-hook env entries
// added to that copy only.
func registryManifest(t *testing.T) string {
	t.Helper()
	_, tagged := binPaths(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, "examples", "cli-registry", "git-feedback.json"))
	if err != nil {
		t.Fatalf("read shipped manifest: %v", err)
	}
	var man map[string]any
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("parse shipped manifest: %v", err)
	}
	tools, ok := man["tools"].(map[string]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("shipped manifest has no tools")
	}
	for name, tv := range tools {
		tool, ok := tv.(map[string]any)
		if !ok {
			t.Fatalf("tool %s: not an object", name)
		}
		cmd, ok := tool["command"].([]any)
		if !ok || len(cmd) == 0 {
			t.Fatalf("tool %s: missing command array", name)
		}
		cmd[0] = tagged
		env, ok := tool["env"].(map[string]any)
		if !ok {
			t.Fatalf("tool %s: missing env object", name)
		}
		env["GIT_FEEDBACK_API_BASE_URL"] = "env:GIT_FEEDBACK_API_BASE_URL"
		env["GIT_FEEDBACK_GH_BIN"] = "env:GIT_FEEDBACK_GH_BIN"
	}
	out, err := json.Marshal(man)
	if err != nil {
		t.Fatalf("marshal manifest copy: %v", err)
	}
	p := filepath.Join(t.TempDir(), "git-feedback-test.json")
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatalf("write manifest copy: %v", err)
	}
	return p
}

// registrySetup returns the reference binary and the rewritten manifest
// copy, after validating the shipped manifest with the production binary on
// PATH.
func registrySetup(t *testing.T) (regBin, manifest string) {
	t.Helper()
	regBin = registryBin(t)
	validateShippedManifest(t, regBin)
	return regBin, registryManifest(t)
}

// validateShippedManifest runs cli-registry validate on the shipped manifest
// with the repository's production binary first on PATH.
func validateShippedManifest(t *testing.T, regBin string) {
	t.Helper()
	prod, _ := binPaths(t)
	ctx := boundedCtx(t)
	cmd := exec.CommandContext(ctx, regBin, "validate",
		filepath.Join(repoRoot, "examples", "cli-registry", "git-feedback.json"))
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(prod)+string(filepath.ListSeparator)+os.Getenv("PATH"))
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("validate shipped manifest: %v (stdout=%q stderr=%q)", err, out.String(), errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "ok" {
		t.Fatalf("validate shipped manifest: stdout=%q want ok (stderr=%q)", got, errb.String())
	}
}

// registryResult is one cli-registry run: the process exit code plus the
// decoded child envelope, when the registry could decode one.
type registryResult struct {
	Exit    int
	Decoded map[string]json.RawMessage
	Stderr  string
}

// runRegistryTool invokes one registered tool through the actual registry
// binary and returns the process exit code and the wrapper's .decoded field.
func runRegistryTool(t *testing.T, regBin, manifest, apiURL, tool string, args map[string]any) registryResult {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", tool, err)
	}
	env := append(os.Environ(),
		"GIT_FEEDBACK_API_BASE_URL="+apiURL,
		"GIT_FEEDBACK_GH_BIN="+fakeGHPath(t),
	)
	res := runCLI(boundedCtx(t), t, regBin, env, "run", "-registry", manifest, tool, string(argsJSON))
	rr := registryResult{Exit: res.Code, Stderr: res.Stderr}
	if raw, ok := res.Env["decoded"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &rr.Decoded); err != nil {
			t.Fatalf("decode .decoded: %v (%v)", err, raw)
		}
	}
	return rr
}

// decodedOf fails when the registry returned no decoded envelope.
func decodedOf(t *testing.T, rr registryResult) map[string]json.RawMessage {
	t.Helper()
	if rr.Decoded == nil {
		t.Fatalf("registry run produced no .decoded (exit=%d stderr=%q)", rr.Exit, rr.Stderr)
	}
	return rr.Decoded
}

// requireExit fails unless the registry process exited with code.
func requireExit(t *testing.T, rr registryResult, code int) {
	t.Helper()
	if rr.Exit != code {
		t.Fatalf("registry exit = %d, want %d (stderr=%q)", rr.Exit, code, rr.Stderr)
	}
}

// errorCode returns the envelope's error.code, empty when error is null.
func errorCode(t *testing.T, dec map[string]json.RawMessage) string {
	t.Helper()
	if !nonNull(dec, "error") {
		return ""
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(dec["error"], &e); err != nil {
		t.Fatalf("decode error: %v (%v)", err, dec["error"])
	}
	return e.Code
}

// snapshotIDOf returns the envelope's snapshot.id.
func snapshotIDOf(t *testing.T, dec map[string]json.RawMessage) string {
	t.Helper()
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(dec["snapshot"], &s); err != nil {
		t.Fatalf("decode snapshot: %v (%v)", err, dec["snapshot"])
	}
	if s.ID == "" {
		t.Fatalf("snapshot id is empty (%v)", dec["snapshot"])
	}
	return s.ID
}

// TestManifestControllerReplay drives the controller mode through the
// registry: reconcile publishes, inbox delivers the event, ack receipts it,
// and a second inbox shows nothing pending.
func TestManifestControllerReplay(t *testing.T) {
	regBin, manifest := registrySetup(t)
	stub := newStub(t)
	dir := testState(t)

	rec := runRegistryTool(t, regBin, manifest, stub.srv.URL, "reconcile",
		map[string]any{"url": testURL, "state_dir": dir})
	requireExit(t, rec, 0)
	dec := decodedOf(t, rec)
	if s := statusOf(t, dec); s != "updated" {
		t.Fatalf("reconcile status = %q, want updated", s)
	}

	inbox := runRegistryTool(t, regBin, manifest, stub.srv.URL, "inbox",
		map[string]any{"url": testURL, "consumer": "registry", "state_dir": dir})
	requireExit(t, inbox, 0)
	dec = decodedOf(t, inbox)
	if s := statusOf(t, dec); s != "ok" {
		t.Fatalf("inbox status = %q, want ok", s)
	}
	ids := eventIDs(t, dec)
	if len(ids) == 0 {
		t.Fatalf("inbox delivered no events after reconcile")
	}
	if boolField(t, dec, "has_more") {
		t.Fatalf("inbox reports has_more on a single-event page")
	}

	ack := runRegistryTool(t, regBin, manifest, stub.srv.URL, "ack",
		map[string]any{"url": testURL, "consumer": "registry", "event": ids, "state_dir": dir})
	requireExit(t, ack, 0)
	if s := statusOf(t, decodedOf(t, ack)); s != "ok" {
		t.Fatalf("ack status = %q, want ok", s)
	}

	again := runRegistryTool(t, regBin, manifest, stub.srv.URL, "inbox",
		map[string]any{"url": testURL, "consumer": "registry", "state_dir": dir})
	requireExit(t, again, 0)
	if got := eventIDs(t, decodedOf(t, again)); len(got) != 0 {
		t.Fatalf("second inbox still pending: %v", got)
	}
}

// TestManifestAckByEvent acks by explicit event ID and proves an unknown ID
// is rejected with its error envelope intact, without disturbing pending
// delivery.
func TestManifestAckByEvent(t *testing.T) {
	regBin, manifest := registrySetup(t)
	stub := newStub(t)
	dir := testState(t)

	rec := runRegistryTool(t, regBin, manifest, stub.srv.URL, "reconcile",
		map[string]any{"url": testURL, "state_dir": dir})
	requireExit(t, rec, 0)
	inbox := runRegistryTool(t, regBin, manifest, stub.srv.URL, "inbox",
		map[string]any{"url": testURL, "consumer": "registry", "state_dir": dir})
	requireExit(t, inbox, 0)
	ids := eventIDs(t, decodedOf(t, inbox))
	if len(ids) == 0 {
		t.Fatalf("inbox delivered no events after reconcile")
	}

	unknown := runRegistryTool(t, regBin, manifest, stub.srv.URL, "ack",
		map[string]any{"url": testURL, "consumer": "registry", "event": []string{"evt-unknown"}, "state_dir": dir})
	requireExit(t, unknown, 1)
	dec := decodedOf(t, unknown)
	if s := statusOf(t, dec); s != "error" {
		t.Fatalf("unknown-event ack status = %q, want error", s)
	}
	if code := errorCode(t, dec); code != "unknown_event" {
		t.Fatalf("unknown-event ack error.code = %q, want unknown_event", code)
	}

	still := runRegistryTool(t, regBin, manifest, stub.srv.URL, "inbox",
		map[string]any{"url": testURL, "consumer": "registry", "state_dir": dir})
	requireExit(t, still, 0)
	if got := eventIDs(t, decodedOf(t, still)); len(got) != len(ids) {
		t.Fatalf("rejected ack changed pending events: got %v, want %v", got, ids)
	}

	ack := runRegistryTool(t, regBin, manifest, stub.srv.URL, "ack",
		map[string]any{"url": testURL, "consumer": "registry", "event": ids, "state_dir": dir})
	requireExit(t, ack, 0)
	if s := statusOf(t, decodedOf(t, ack)); s != "ok" {
		t.Fatalf("ack status = %q, want ok", s)
	}
	again := runRegistryTool(t, regBin, manifest, stub.srv.URL, "ack",
		map[string]any{"url": testURL, "consumer": "registry", "event": ids, "state_dir": dir})
	requireExit(t, again, 0)
	if s := statusOf(t, decodedOf(t, again)); s != "ok" {
		t.Fatalf("idempotent ack status = %q, want ok", s)
	}
}

// TestManifestSnapshotExport exports the published snapshot through the
// registry and verifies the reported path, digest, and counts against the
// file on disk.
func TestManifestSnapshotExport(t *testing.T) {
	regBin, manifest := registrySetup(t)
	stub := newStub(t)
	dir := testState(t)

	rec := runRegistryTool(t, regBin, manifest, stub.srv.URL, "reconcile",
		map[string]any{"url": testURL, "state_dir": dir})
	requireExit(t, rec, 0)
	id := snapshotIDOf(t, decodedOf(t, rec))

	dest := filepath.Join(t.TempDir(), "snapshot.json")
	exp := runRegistryTool(t, regBin, manifest, stub.srv.URL, "snapshot",
		map[string]any{"url": testURL, "snapshot": id, "output_file": dest, "state_dir": dir})
	requireExit(t, exp, 0)
	dec := decodedOf(t, exp)
	if s := statusOf(t, dec); s != "ok" {
		t.Fatalf("snapshot status = %q, want ok", s)
	}
	var export struct {
		Path   string         `json:"path"`
		Digest string         `json:"digest"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(dec["export"], &export); err != nil {
		t.Fatalf("decode export: %v (%v)", err, dec["export"])
	}
	if export.Path != dest {
		t.Fatalf("export.path = %q, want %q", export.Path, dest)
	}
	if export.Counts["thread"] != 1 || export.Counts["review"] != 0 || export.Counts["comment"] != 0 {
		t.Fatalf("export counts = %v, want one thread", export.Counts)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read exported snapshot: %v", err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != export.Digest {
		t.Fatalf("export digest = %q, want %q over %d bytes", export.Digest, got, len(b))
	}
}

// TestManifestOperationalErrorDecoded proves the registry decodes an
// operational error envelope (status error, exit 1) instead of suppressing
// it: success_exit_codes [0,1] enables decoding, not suppression.
func TestManifestOperationalErrorDecoded(t *testing.T) {
	regBin, manifest := registrySetup(t)
	stub := newStub(t)
	dir := testState(t)

	rec := runRegistryTool(t, regBin, manifest, stub.srv.URL, "reconcile",
		map[string]any{"url": testURL, "state_dir": dir})
	requireExit(t, rec, 0)

	exp := runRegistryTool(t, regBin, manifest, stub.srv.URL, "snapshot",
		map[string]any{"url": testURL, "snapshot": "snap-unknown", "output_file": filepath.Join(t.TempDir(), "out.json"), "state_dir": dir})
	requireExit(t, exp, 1)
	dec := decodedOf(t, exp)
	if s := statusOf(t, dec); s != "error" {
		t.Fatalf("unknown-snapshot status = %q, want error", s)
	}
	if code := errorCode(t, dec); code != "unknown_snapshot" {
		t.Fatalf("unknown-snapshot error.code = %q, want unknown_snapshot", code)
	}
	if got := stringField(t, dec, "schema"); got != "git-feedback/v1" {
		t.Fatalf("error envelope schema = %q, want git-feedback/v1", got)
	}
}
