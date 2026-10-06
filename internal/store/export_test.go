package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// TestExportSnapshot verifies the snapshot file format, digest, and that the
// destination is looked up through the selected stream.
func TestExportSnapshot(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA",
		[]forge.Thread{thread("t2", "second"), thread("t1", "first")},
		[]forge.Review{{ID: "r1", Author: "rev", Body: "body", State: "COMMENTED"}},
		[]forge.Comment{{ID: "c1", Author: "rev", Body: "note"}}))

	dir := t.TempDir()
	dest := filepath.Join(dir, "snap.json")
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	res, err := st.Export(context.Background(), ExportInput{
		TargetID: testTarget().ID, Account: "alice", SnapshotID: "s1",
		Destination: dest, StateDir: stateDir,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	sum := sha256.Sum256(b)
	if res.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want sha256 of file bytes", res.Digest)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("export mode = %v, want 0600", info.Mode().Perm())
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".git-feedback-export-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	var doc struct {
		Schema   string          `json:"schema"`
		Target   forge.Target    `json:"target"`
		Account  string          `json:"account"`
		Head     string          `json:"head"`
		Threads  []forge.Thread  `json:"threads"`
		Reviews  []forge.Review  `json:"reviews"`
		Comments []forge.Comment `json:"comments"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Schema != "git-feedback-snapshot/v1" {
		t.Errorf("schema = %q", doc.Schema)
	}
	if doc.Target.ID != testTarget().ID || doc.Account != "alice" || doc.Head != "headA" {
		t.Errorf("header = %s/%s/%s", doc.Target.ID, doc.Account, doc.Head)
	}
	// Arrays are sorted by object ID.
	if len(doc.Threads) != 2 || doc.Threads[0].ID != "t1" || doc.Threads[1].ID != "t2" {
		t.Errorf("threads not sorted: %+v", doc.Threads)
	}
	if len(res.Counts) != 3 || res.Counts["thread"] != 2 || res.Counts["review"] != 1 || res.Counts["comment"] != 1 {
		t.Errorf("counts = %v", res.Counts)
	}
}

// TestExportRefusesExistingAndSymlink verifies no-replace refusals.
func TestExportRefusesExistingAndSymlink(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	dir := t.TempDir()

	existing := filepath.Join(dir, "exists.json")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := st.Export(context.Background(), ExportInput{
		TargetID: testTarget().ID, Account: "alice", SnapshotID: "s1",
		Destination: existing, StateDir: dir,
	}); err == nil {
		t.Fatal("export over existing destination succeeded")
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep me" {
		t.Fatal("existing destination was modified")
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(existing, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := st.Export(context.Background(), ExportInput{
		TargetID: testTarget().ID, Account: "alice", SnapshotID: "s1",
		Destination: link, StateDir: dir,
	}); err == nil {
		t.Fatal("export to symlink destination succeeded")
	}
}

// TestConcurrentExportNoReplace verifies that exactly one of two concurrent
// exports to the same destination succeeds.
func TestConcurrentExportNoReplace(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))
	dir := t.TempDir()
	dest := filepath.Join(dir, "snap.json")
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.Export(context.Background(), ExportInput{
				TargetID: testTarget().ID, Account: "alice", SnapshotID: "s1",
				Destination: dest, StateDir: stateDir,
			})
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent exports succeeded, want exactly 1", succeeded, n)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("destination missing: %v", err)
	}
}

// TestExportRefusesStateDir verifies refusal of destinations resolving inside
// the state directory.
func TestExportRefusesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	publish(t, st, "alice", mkSnapshot("headA", nil, nil, nil))

	if _, err := st.Export(context.Background(), ExportInput{
		TargetID: testTarget().ID, Account: "alice", SnapshotID: "s1",
		Destination: filepath.Join(dir, "snap.json"), StateDir: dir,
	}); err == nil {
		t.Fatal("export into state directory succeeded")
	} else if se, ok := err.(*Error); !ok || se.Code != CodeExportStateDir {
		t.Fatalf("error = %v, want code %s", err, CodeExportStateDir)
	}
}

// TestCrossAccountExportRejected verifies that a snapshot ID is resolved
// through the selected stream, not by ID alone.
func TestCrossAccountExportRejected(t *testing.T) {
	st := openTestStore(t)
	publish(t, st, "alice", mkSnapshot("headA", []forge.Thread{thread("t1", "alice only")}, nil, nil))
	publish(t, st, "bob", mkSnapshot("headA", nil, nil, nil))

	if _, err := st.Export(context.Background(), ExportInput{
		TargetID: testTarget().ID, Account: "bob", SnapshotID: "s1",
		Destination: filepath.Join(t.TempDir(), "snap.json"), StateDir: t.TempDir(),
	}); err == nil {
		t.Fatal("cross-account export by ID succeeded, want rejection")
	}
	// Bob's own snapshot exports fine.
	if _, err := st.Export(context.Background(), ExportInput{
		TargetID: testTarget().ID, Account: "bob", SnapshotID: "s2",
		Destination: filepath.Join(t.TempDir(), "snap.json"), StateDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("own-stream export: %v", err)
	}
}
