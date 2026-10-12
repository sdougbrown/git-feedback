package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// resetTables lists the per-stream tables in child-first order so foreign
// keys stay satisfied when a target's streams are deleted. targets itself is
// deleted last and directly by target ID.
var resetTables = []string{
	"acks", "events", "observations", "revisions",
	"objects", "attempts", "schedule", "snapshots",
}

// Exists reports whether any of the store's database files (the main
// database or its WAL sidecars) is present in dir. A stat error that is
// not IsNotExist counts as existing: the open path surfaces the real
// error. This is the shared file-set check for the reset paths, so a
// partial reset that leaves sidecars behind is not mistaken for an
// absent store.
func Exists(dir string) bool {
	for _, name := range []string{DBFilename, DBFilename + "-wal", DBFilename + "-shm"} {
		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil || !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

// Reset deletes the store's database files inside stateDir and reports
// whether any store file existed. The state directory itself is left in
// place; the next Open recreates the database from scratch. This is the
// remedy for a refused (legacy, older-data, or corrupt) store: reset never
// opens or inspects the store, so it works even when Open refuses.
func Reset(stateDir string) (bool, error) {
	dir := filepath.Clean(stateDir)
	removed := false
	for _, name := range []string{DBFilename, DBFilename + "-wal", DBFilename + "-shm"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, &Error{Code: CodeStore, Message: fmt.Sprintf("stat %s: %v", path, err)}
		}
		if err := os.Remove(path); err != nil {
			return removed, &Error{Code: CodeStore, Message: fmt.Sprintf("remove %s: %v", path, err)}
		}
		removed = true
	}
	return removed, nil
}

// ResetTarget deletes every stored stream for one target ID across all
// accounts, with its snapshots, events, revisions, attempts, schedule, and
// acknowledgements. It reports whether any stream was removed. Host-scoped
// state (leases, rate gates, the HTTP cache) is shared across targets and
// survives, as do other targets' streams.
func (s *Store) ResetTarget(ctx context.Context, targetID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, &Error{Code: CodeStore, Message: fmt.Sprintf("begin reset: %v", err)}
	}
	defer tx.Rollback()
	for _, table := range resetTables {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE stream_id IN (SELECT stream_id FROM targets WHERE target_id = ?)`, table),
			targetID); err != nil {
			return false, &Error{Code: CodeStore, Message: fmt.Sprintf("reset %s: %v", table, err)}
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM targets WHERE target_id = ?`, targetID)
	if err != nil {
		return false, &Error{Code: CodeStore, Message: fmt.Sprintf("reset targets: %v", err)}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, &Error{Code: CodeStore, Message: fmt.Sprintf("reset targets: %v", err)}
	}
	if err := tx.Commit(); err != nil {
		return false, &Error{Code: CodeStore, Message: fmt.Sprintf("commit reset: %v", err)}
	}
	return n > 0, nil
}
