package store

import (
	"database/sql"
	"fmt"
)

// SupportedDataVersion is the data version this binary writes and accepts.
// It tracks the meaning of stored VALUES — fingerprint inputs, author
// normalization, and attribution rules — not the SQL table layout (that is
// schema_version, migrated by the SQL files). Bump it whenever one of those
// semantics changes. Stores carrying an older data version are refused at
// Open, never silently re-baselined: reinterpreting old rows under new
// rules would fabricate events. The store is a disposable cache; the
// refusal message names the remedy.
//
// Version 1 is implicit: stores written before data versioning carried no
// data_version row and are treated as version 1.
const SupportedDataVersion = 2

// legacyDataVersion is the data version assigned to stores that predate
// data versioning (no data_version row).
const legacyDataVersion = 1

// versionDB is satisfied by both *sql.DB and *sql.Tx, so the data version
// checks can run inside the open migration transaction or standalone.
type versionDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// seedDataVersion ensures the data_version table exists and, for a store
// created fresh by this Open, stamps it with the supported data version.
// A pre-existing store is left untouched so the data version check can
// refuse it rather than re-baseline it.
func seedDataVersion(tx versionDB, fresh bool) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS data_version (version INTEGER NOT NULL)`); err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("prepare data_version: %v", err)}
	}
	if !fresh {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO data_version (version) VALUES (?)`, SupportedDataVersion); err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("record data version: %v", err)}
	}
	return nil
}

// checkDataVersion refuses stores whose stored values mean something other
// than what this binary computes: a legacy store (implicit version 1)
// predates the changed fingerprint, author, and attribution rules, and a
// newer version was written by a newer binary. Neither case is migrated or
// re-baselined; the message names the store path and the remedy.
func checkDataVersion(q versionDB, path string) error {
	var rows int
	if err := q.QueryRow(`SELECT count(*) FROM data_version`).Scan(&rows); err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("read data_version: %v", err)}
	}
	version := legacyDataVersion
	if rows > 0 {
		if err := q.QueryRow(`SELECT COALESCE(MAX(version), ?) FROM data_version`, legacyDataVersion).Scan(&version); err != nil {
			return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("read data_version: %v", err)}
		}
	}
	switch {
	case version > SupportedDataVersion:
		return &Error{
			Code: CodeStoreNewer,
			Message: fmt.Sprintf(
				"store data version %d is newer than supported version %d; the store at %s was written by a newer git-feedback; delete it (or run `git-feedback store reset`) and collect again",
				version, SupportedDataVersion, path),
		}
	case version < SupportedDataVersion:
		return &Error{
			Code: CodeStoreDataOlder,
			Message: fmt.Sprintf(
				"store data version %d is older than supported version %d; the stored values in %s were written under different fingerprint, author, or attribution rules; delete the store (or run `git-feedback store reset`) and let the next collection re-baseline",
				version, SupportedDataVersion, path),
		}
	}
	return nil
}
