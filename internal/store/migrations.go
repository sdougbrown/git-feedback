package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one ordered numbered SQL file.
type migration struct {
	version int
	name    string
	sql     string
}

var migrationNameRe = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// discoverMigrations lists the embedded numbered SQL files in ascending
// version order. The highest version present is the supported schema version.
func discoverMigrations() ([]migration, int, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, 0, &Error{Code: CodeStore, Message: fmt.Sprintf("read embedded migrations: %v", err)}
	}
	var ms []migration
	for _, e := range entries {
		m := migrationNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, 0, &Error{Code: CodeStore, Message: "unexpected migration file name: " + e.Name()}
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, 0, &Error{Code: CodeStore, Message: "bad migration version: " + e.Name()}
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, 0, &Error{Code: CodeStore, Message: fmt.Sprintf("read migration %s: %v", e.Name(), err)}
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(sqlBytes)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	supported := 0
	if len(ms) > 0 {
		supported = ms[len(ms)-1].version
	}
	return ms, supported, nil
}

// knownTables lists the tables owned by the migrations, used to detect a
// damaged store that has tables but no readable schema_version.
var knownTables = []string{
	"targets", "snapshots", "objects", "revisions",
	"events", "acks", "observations", "leases",
	"attempts", "schedule", "rate_gates", "http_cache",
}

// migrate applies pending embedded migrations transactionally. It refuses a
// newer-than-supported schema and never deletes or recreates an existing
// store.
func migrate(db *sql.DB) error {
	ms, supported, err := discoverMigrations()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("begin migration: %v", err)}
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("prepare schema_version: %v", err)}
	}

	var hasVersion, hasTables int
	if err := tx.QueryRow(
		`SELECT (SELECT count(*) FROM schema_version), (SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN (?,?,?,?,?,?,?,?,?,?,?,?))`,
		knownTables[0], knownTables[1], knownTables[2], knownTables[3],
		knownTables[4], knownTables[5], knownTables[6], knownTables[7],
		knownTables[8], knownTables[9], knownTables[10], knownTables[11],
	).Scan(&hasVersion, &hasTables); err != nil {
		return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("inspect schema: %v", err)}
	}
	if hasVersion == 0 && hasTables > 0 {
		return &Error{Code: CodeStoreCorrupt, Message: "store tables without a readable schema_version"}
	}
	if hasVersion > 0 && hasTables == 0 {
		return &Error{Code: CodeStoreCorrupt, Message: "schema_version without store tables"}
	}

	current := 0
	if hasVersion > 0 {
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
			return &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("read schema_version: %v", err)}
		}
	}
	if current > supported {
		return &Error{
			Code:    CodeStoreNewer,
			Message: fmt.Sprintf("store schema version %d is newer than supported version %d", current, supported),
		}
	}
	for _, m := range ms {
		if m.version <= current {
			continue
		}
		if _, err := tx.Exec(m.sql); err != nil {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("apply migration %s: %v", m.name, err)}
		}
		if _, err := tx.Exec(`DELETE FROM schema_version`); err != nil {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("record migration %s: %v", m.name, err)}
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, m.version); err != nil {
			return &Error{Code: CodeStore, Message: fmt.Sprintf("record migration %s: %v", m.name, err)}
		}
	}
	if err := tx.Commit(); err != nil {
		return &Error{Code: CodeStore, Message: fmt.Sprintf("commit migrations: %v", err)}
	}
	return nil
}
