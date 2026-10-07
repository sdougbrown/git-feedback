package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/git-feedback/internal/clock"
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

// TestOpenLockedStoreRealBusyIsRetriable exercises a REAL driver-generated
// SQLITE_BUSY error, not a hand-transcribed string. It opens two
// connections to the same store: the first holds the WAL write lock via a
// BEGIN IMMEDIATE transaction, and the second (with a zero busy timeout)
// attempts a write that must fail with the driver's own busy error. That
// error is fed to isLockError and corruptOrStore, so a modernc.org/sqlite
// upgrade that changes the busy wording fails here instead of silently
// reclassifying transient lock contention as fatal store_corrupt.
func TestOpenLockedStoreRealBusyIsRetriable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := filepath.Join(dir, dbFilename)

	// Connection A holds the WAL write lock. A single serialized connection
	// keeps BEGIN IMMEDIATE and the write on the same underlying handle.
	dbA, err := sql.Open("sqlite", dsn(path, 5000))
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer dbA.Close()
	dbA.SetMaxOpenConns(1)
	if _, err := dbA.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin A: %v", err)
	}

	// Connection B bypasses the dsn guard (which maps <= 0 to the 5000ms
	// default) so the contended write fails immediately with the driver's
	// own SQLITE_BUSY error instead of blocking 5s.
	dsnB := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)&_pragma=foreign_keys(1)", path)
	dbB, err := sql.Open("sqlite", dsnB)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer dbB.Close()
	dbB.SetMaxOpenConns(1)
	_, busyErr := dbB.Exec("CREATE TABLE lock_probe (x INTEGER)")
	if busyErr == nil {
		t.Fatal("expected a busy error from the second writer, got nil")
	}

	// The real driver busy error must classify as a transient lock error.
	if !isLockError(busyErr) {
		t.Fatalf("real driver busy error not classified as lock: %v", busyErr)
	}

	// Feeding the real busy error to corruptOrStore must yield the
	// retriable store_error code, not store_corrupt.
	cerr := corruptOrStore(path, busyErr)
	var se *Error
	if !errors.As(cerr, &se) {
		t.Fatalf("corruptOrStore: expected *Error, got %v", cerr)
	}
	if se.Code != CodeStore {
		t.Fatalf("code = %s, want %s (message: %s)", se.Code, CodeStore, se.Message)
	}
}

// TestMigrationSelfIdentityPreservesRows builds a populated pre-0003 store by
// applying 0001 and 0002 directly, then opens it with the store so 0003
// applies, and asserts the seeded objects and events survive with an empty
// author (legacy rows are always deliverable).
func TestMigrationSelfIdentityPreservesRows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, dbFilename)
	db, err := sql.Open("sqlite", dsn(path, 5000))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = 1"); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	ms, _, err := discoverMigrations()
	if err != nil {
		t.Fatalf("discover migrations: %v", err)
	}
	for _, m := range ms {
		if m.version != 1 && m.version != 2 {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (2)`); err != nil {
		t.Fatalf("record version: %v", err)
	}
	seed := []string{
		`INSERT INTO targets (stream_id, target_id, account, host, target_json)
		 VALUES (1, 'github:github.com:owner/name:7', 'alice', 'github.com', '{}')`,
		`INSERT INTO snapshots (id, stream_id, head, fingerprint, body, collected_start, collected_end)
		 VALUES (1, 1, 'h1', 'fp', '{}', '2026-07-01T11:59:00Z', '2026-07-01T12:00:00Z')`,
		`INSERT INTO objects (stream_id, kind, provider_id, counter, present, last_fingerprint)
		 VALUES (1, 'thread', 't1', 1, 1, 'fp')`,
		`INSERT INTO events (stream_id, kind, object_kind, object_id, revision, url, snapshot_id, observed_at)
		 VALUES (1, 'initial_observation', 'thread', 't1', 1, 'u', 1, '2026-07-01T12:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	st, err := Open(dir, Options{Clock: clock.NewFake(baseTime)})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer st.Close()

	// The object row survives with an empty author (legacy, always deliverable).
	var author string
	if err := st.db.QueryRow(`SELECT author FROM objects WHERE stream_id = 1 AND provider_id = 't1'`).Scan(&author); err != nil {
		t.Fatalf("read object author: %v", err)
	}
	if author != "" {
		t.Fatalf("object author = %q, want '' (legacy rows keep empty author)", author)
	}
	// The event row survives with an empty author.
	if err := st.db.QueryRow(`SELECT author FROM events WHERE stream_id = 1`).Scan(&author); err != nil {
		t.Fatalf("read event author: %v", err)
	}
	if author != "" {
		t.Fatalf("event author = %q, want '' (legacy rows keep empty author)", author)
	}
	// The legacy event is still delivered (pending) for a new consumer.
	pending, err := st.Inbox(context.Background(), InboxInput{TargetID: "github:github.com:owner/name:7", Account: "alice", Consumer: "c2", Limit: MaxInboxLimit})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(pending.Events) != 1 || pending.Events[0].ID != "e1" {
		t.Fatalf("pending events = %+v, want e1", pending.Events)
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
