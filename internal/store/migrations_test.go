package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefuseNewerSchema verifies that a store whose schema version is newer
// than the supported version is refused, never replaced.
func TestRefuseNewerSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE schema_version SET version = ?`, 9999); err != nil {
		t.Fatalf("bump schema version: %v", err)
	}
	st.Close()

	_, err = Open(dir, Options{})
	if err == nil {
		t.Fatal("open succeeded on newer schema, want refusal")
	}
	se, ok := err.(*Error)
	if !ok || se.Code != CodeStoreNewer {
		t.Fatalf("error = %v, want code %s", err, CodeStoreNewer)
	}
	// The store must survive untouched.
	if _, err := os.Stat(filepath.Join(dir, dbFilename)); err != nil {
		t.Fatalf("store file missing after refusal: %v", err)
	}
}

// TestRefuseCorruptStore verifies that a corrupt database file is refused
// with the explicit store_corrupt code, never deleted or recreated.
func TestRefuseCorruptStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, dbFilename)
	if err := os.WriteFile(path, []byte("this is not a SQLite database, just noise"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Open(dir, Options{})
	if err == nil {
		t.Fatal("open succeeded on corrupt store, want refusal")
	}
	se, ok := err.(*Error)
	if !ok || se.Code != CodeStoreCorrupt {
		t.Fatalf("error = %v, want code %s", err, CodeStoreCorrupt)
	}
	// The corrupt file must remain in place untouched.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasPrefix(string(got), "this is not a SQLite database") {
		t.Fatalf("corrupt file was modified: %q", got)
	}
}
