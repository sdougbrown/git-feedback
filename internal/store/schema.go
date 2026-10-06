package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// dbFilename is the SQLite database file inside the state directory.
const dbFilename = "feedback.db"

// dsn builds the modernc.org/sqlite DSN with the pinned pragmas applied on
// every connection: WAL journaling, a 5s busy timeout, and enforced foreign
// keys (foreign keys are per-connection in SQLite).
func dsn(path string) string {
	return fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
}

// openDatabase opens (creating if needed) the store's database file with the
// pinned permissions and pragmas, and refuses corrupt files.
func openDatabase(stateDir string) (*sql.DB, string, error) {
	dir := filepath.Clean(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", &Error{Code: CodeStore, Message: fmt.Sprintf("create state directory: %v", err)}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, "", &Error{Code: CodeStore, Message: fmt.Sprintf("restrict state directory: %v", err)}
	}
	path := filepath.Join(dir, dbFilename)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, "", corruptOrStore(path, err)
	}
	// One serialized connection keeps write transactions simple and avoids
	// SQLITE_BUSY churn inside this process; cross-process contention is
	// handled by busy_timeout and WAL.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, "", corruptOrStore(path, err)
	}
	if err := checkIntegrity(db); err != nil {
		db.Close()
		return nil, "", err
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if _, statErr := os.Stat(name); statErr != nil {
			// The file does not exist yet; nothing to restrict.
			continue
		}
		if err := os.Chmod(name, 0o600); err != nil {
			db.Close()
			return nil, "", &Error{Code: CodeStore, Message: fmt.Sprintf("restrict %s: %v", name, err)}
		}
	}
	return db, path, nil
}

// corruptOrStore maps an open/ping failure on an existing non-empty file to
// the explicit store_corrupt error; the tool never deletes or recreates it.
func corruptOrStore(path string, err error) error {
	if st, statErr := os.Stat(path); statErr == nil && st.Size() > 0 {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("open %s: %v", path, err)}
	}
	return &Error{Code: CodeStore, Message: fmt.Sprintf("open %s: %v", path, err)}
}

// checkIntegrity runs PRAGMA integrity_check and reports store_corrupt on any
// failure other than a clean "ok".
func checkIntegrity(db *sql.DB) error {
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("integrity_check: %v", err)}
	}
	defer rows.Close()
	var ok bool
	for rows.Next() {
		var res string
		if err := rows.Scan(&res); err != nil {
			return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("integrity_check: %v", err)}
		}
		if res == "ok" {
			ok = true
			continue
		}
		return &Error{Code: CodeStoreCorrupt, Message: "integrity_check: " + res}
	}
	if err := rows.Err(); err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("integrity_check: %v", err)}
	}
	if !ok {
		return &Error{Code: CodeStoreCorrupt, Message: "integrity_check: no result"}
	}
	return nil
}
