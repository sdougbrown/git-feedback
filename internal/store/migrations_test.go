package store

import (
	"errors"
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

// TestOpenLockedStoreIsRetriable verifies that a transient lock failure on
// an existing store (a concurrent process holding the WAL lock longer than
// the busy timeout) is classified as retriable store_error, not
// store_corrupt. A real lock failure surfaces as
// "database is locked (5) (SQLITE_BUSY)".
func TestOpenLockedStoreIsRetriable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, dbFilename)
	if err := os.WriteFile(path, []byte("existing store data"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The busy error string as modernc.org/sqlite surfaces it via database/sql.
	locked := errors.New("database is locked (5) (SQLITE_BUSY)")
	err := corruptOrStore(path, locked)
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("corruptOrStore: expected *Error, got %v", err)
	}
	if se.Code != CodeStore {
		t.Fatalf("code = %s, want %s (message: %s)", se.Code, CodeStore, se.Message)
	}

	// A genuine open failure on the same file stays store_corrupt.
	err = corruptOrStore(path, errors.New("file is not a database"))
	if !errors.As(err, &se) {
		t.Fatalf("corruptOrStore: expected *Error, got %v", err)
	}
	if se.Code != CodeStoreCorrupt {
		t.Fatalf("code = %s, want %s (message: %s)", se.Code, CodeStoreCorrupt, se.Message)
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
