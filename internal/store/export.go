package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// snapshotFileSchema is the pinned snapshot file format identifier.
const snapshotFileSchema = "git-feedback-snapshot/v1"

// ExportInput describes one snapshot export to one destination path.
type ExportInput struct {
	TargetID   string
	Account    string
	SnapshotID string
	// Destination is the file to publish; it must not already exist.
	Destination string
	// StateDir is the store's state directory; destinations resolving inside
	// it are refused.
	StateDir string
}

// ExportResult reports one successful export.
type ExportResult struct {
	SnapshotID string
	Path       string
	Digest     string
	Counts     map[string]int
}

// snapshotFile is the pinned export document.
type snapshotFile struct {
	Schema         string          `json:"schema"`
	Target         forge.Target    `json:"target"`
	Account        string          `json:"account"`
	Head           string          `json:"head"`
	CollectedStart time.Time       `json:"collected_start"`
	CollectedEnd   time.Time       `json:"collected_end"`
	Threads        []forge.Thread  `json:"threads"`
	Reviews        []forge.Review  `json:"reviews"`
	Comments       []forge.Comment `json:"comments"`
}

// Export writes one stored snapshot, looked up through the selected stream,
// to Destination with atomic no-replace semantics: a fsync'd mode-0600 temp
// file in the destination directory is hard-linked to the destination and the
// temp name removed. Existing destinations, symlinks, and canonical paths
// inside the state directory are refused.
func (s *Store) Export(ctx context.Context, in ExportInput) (ExportResult, error) {
	seq, ok := parseSnapshotID(in.SnapshotID)
	if !ok {
		return ExportResult{}, &Error{Code: CodeUnknownSnapshot, Message: fmt.Sprintf("unknown snapshot %q", in.SnapshotID)}
	}
	streamID, err := s.resolveStream(ctx, in.TargetID, in.Account)
	if err != nil {
		return ExportResult{}, err
	}
	var targetJSON, head, body, startStr, endStr string
	var account string
	err = s.db.QueryRowContext(ctx, `
		SELECT t.target_json, t.account, s.head, s.body, s.collected_start, s.collected_end
		FROM snapshots s JOIN targets t ON t.stream_id = s.stream_id
		WHERE s.id = ? AND s.stream_id = ?`, seq, streamID).
		Scan(&targetJSON, &account, &head, &body, &startStr, &endStr)
	if err == sql.ErrNoRows {
		return ExportResult{}, &Error{Code: CodeUnknownSnapshot, Message: fmt.Sprintf("unknown snapshot %q for this stream", in.SnapshotID)}
	}
	if err != nil {
		return ExportResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("read snapshot: %v", err)}
	}

	var target forge.Target
	if err := json.Unmarshal([]byte(targetJSON), &target); err != nil {
		return ExportResult{}, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("decode target: %v", err)}
	}
	var snap forge.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		return ExportResult{}, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("decode snapshot body: %v", err)}
	}
	start, err := time.Parse(time.RFC3339Nano, startStr)
	if err != nil {
		return ExportResult{}, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("decode collected_start: %v", err)}
	}
	end, err := time.Parse(time.RFC3339Nano, endStr)
	if err != nil {
		return ExportResult{}, &Error{Code: CodeStoreCorrupt, Message: fmt.Sprintf("decode collected_end: %v", err)}
	}

	sort.SliceStable(snap.Threads, func(i, j int) bool { return snap.Threads[i].ID < snap.Threads[j].ID })
	sort.SliceStable(snap.Reviews, func(i, j int) bool { return snap.Reviews[i].ID < snap.Reviews[j].ID })
	sort.SliceStable(snap.Comments, func(i, j int) bool { return snap.Comments[i].ID < snap.Comments[j].ID })
	doc := snapshotFile{
		Schema:         snapshotFileSchema,
		Target:         target,
		Account:        account,
		Head:           head,
		CollectedStart: start.UTC(),
		CollectedEnd:   end.UTC(),
		Threads:        snap.Threads,
		Reviews:        snap.Reviews,
		Comments:       snap.Comments,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return ExportResult{}, &Error{Code: CodeStore, Message: fmt.Sprintf("marshal snapshot file: %v", err)}
	}

	if err := publishNoReplace(in.Destination, in.StateDir, b); err != nil {
		return ExportResult{}, err
	}
	sum := sha256.Sum256(b)
	return ExportResult{
		SnapshotID: in.SnapshotID,
		Path:       in.Destination,
		Digest:     hex.EncodeToString(sum[:]),
		Counts: map[string]int{
			"thread": len(doc.Threads), "review": len(doc.Reviews), "comment": len(doc.Comments),
		},
	}, nil
}

// publishNoReplace publishes bytes to dest atomically and without replacing
// any existing name.
func publishNoReplace(dest, stateDir string, b []byte) error {
	if dest == "" {
		return &Error{Code: CodeInvalidDestination, Message: "empty destination path"}
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return &Error{Code: CodeInvalidDestination, Message: fmt.Sprintf("resolve destination: %v", err)}
	}
	dir := filepath.Dir(destAbs)
	if _, err := os.Stat(dir); err != nil {
		return &Error{Code: CodeInvalidDestination, Message: fmt.Sprintf("destination directory: %v", err)}
	}

	// Refuse canonical paths inside the state directory.
	if stateDir != "" {
		stateAbs, err := filepath.Abs(stateDir)
		if err != nil {
			return &Error{Code: CodeInvalidDestination, Message: fmt.Sprintf("resolve state directory: %v", err)}
		}
		stateEval, err := filepath.EvalSymlinks(stateAbs)
		if err != nil {
			return &Error{Code: CodeInvalidDestination, Message: fmt.Sprintf("resolve state directory: %v", err)}
		}
		dirEval, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return &Error{Code: CodeInvalidDestination, Message: fmt.Sprintf("resolve destination directory: %v", err)}
		}
		if dirEval == stateEval || strings.HasPrefix(dirEval, stateEval+string(filepath.Separator)) {
			return &Error{Code: CodeExportStateDir, Message: "destination resolves inside the state directory"}
		}
	}

	// Refuse existing destinations and symlinks (link would fail anyway, but
	// an explicit check gives a precise error).
	if st, err := os.Lstat(destAbs); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return &Error{Code: CodeDestinationExists, Message: "destination is a symlink"}
		}
		return &Error{Code: CodeDestinationExists, Message: "destination already exists"}
	}

	tmp, err := os.CreateTemp(dir, ".git-feedback-export-*")
	if err != nil {
		return &Error{Code: CodeExportUnsupported, Message: fmt.Sprintf("create temp file: %v", err)}
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return &Error{Code: CodeExportUnsupported, Message: fmt.Sprintf("write temp file: %v", err)}
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return &Error{Code: CodeExportUnsupported, Message: fmt.Sprintf("fsync temp file: %v", err)}
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return &Error{Code: CodeExportUnsupported, Message: fmt.Sprintf("close temp file: %v", err)}
	}
	// Hard-link temp → destination: link(2) never replaces an existing name,
	// so a concurrent publisher is serialized by the kernel itself.
	if err := os.Link(tmpName, destAbs); err != nil {
		cleanup()
		if os.IsExist(err) {
			return &Error{Code: CodeDestinationExists, Message: "destination already exists"}
		}
		return &Error{Code: CodeExportUnsupported, Message: fmt.Sprintf("no-replace publication unsupported: %v", err)}
	}
	if err := os.Remove(tmpName); err != nil {
		return &Error{Code: CodeExportUnsupported, Message: fmt.Sprintf("remove temp file: %v", err)}
	}
	// Make the publication durable in the destination directory.
	if dh, err := os.Open(dir); err == nil {
		dh.Sync()
		dh.Close()
	}
	return nil
}
