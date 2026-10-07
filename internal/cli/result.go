package cli

import (
	"encoding/json"
	"io"
)

// Schema is the result envelope schema identifier.
const Schema = "git-feedback/v1"

// Status values used by subcommands.
const (
	StatusUpdated    = "updated"
	StatusUnchanged  = "unchanged"
	StatusDeferred   = "deferred"
	StatusBusy       = "busy"
	StatusHeadChange = "head_changed"
	StatusOK         = "ok"
	StatusEvents     = "events"
	StatusTimeout    = "timeout"
	StatusError      = "error"
)

// ReviewerCompletionUnknown is the only reviewer_completion value in v1.
const ReviewerCompletionUnknown = "unknown"

// Target mirrors forge.Target in the envelope.
type Target struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Forge  string `json:"forge"`
	Host   string `json:"host"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// Snapshot summarizes one stored immutable snapshot.
type Snapshot struct {
	ID             string         `json:"id"`
	CollectedStart string         `json:"collected_start"`
	CollectedEnd   string         `json:"collected_end"`
	Complete       bool           `json:"complete"`
	ObjectCounts   map[string]int `json:"object_counts"`
}

// Attempt reports the latest collection attempt separately from
// snapshot.complete.
type Attempt struct {
	At        string  `json:"at"`
	OK        bool    `json:"ok"`
	Complete  bool    `json:"complete"`
	ErrorCode *string `json:"error_code"`
	NextDue   string  `json:"next_due"`
}

// Freshness reports how current the returned snapshot is.
type Freshness struct {
	SnapshotObservedAt string `json:"snapshot_observed_at"`
	Stale              bool   `json:"stale"`
}

// Export reports the artifact written by the snapshot command.
type Export struct {
	Path   string         `json:"path"`
	Digest string         `json:"digest"`
	Counts map[string]int `json:"counts"`
}

// Error is the envelope error object.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Result is the single JSON value every subcommand writes to stdout.
type Result struct {
	Schema             string    `json:"schema"`
	Command            string    `json:"command"`
	Status             string    `json:"status"`
	Target             *Target   `json:"target"`
	Account            *string   `json:"account"`
	ObservedHead       *string   `json:"observed_head"`
	ExpectedHead       *string   `json:"expected_head"`
	Snapshot           *Snapshot `json:"snapshot"`
	Attempt            *Attempt  `json:"attempt"`
	Freshness          Freshness `json:"freshness"`
	ReviewerCompletion string    `json:"reviewer_completion"`
	Events             []any     `json:"events"`
	HasMore            bool      `json:"has_more"`
	NextCursor         *string   `json:"next_cursor"`
	Export             *Export   `json:"export"`
	Error              *Error    `json:"error"`
}

// Write emits exactly one JSON value for r to w, applying the envelope
// defaults (schema, reviewer_completion, and a non-null events array).
// It is the single envelope writer; subcommands must not format output
// themselves.
func Write(w io.Writer, r Result) error {
	if r.Schema == "" {
		r.Schema = Schema
	}
	if r.ReviewerCompletion == "" {
		r.ReviewerCompletion = ReviewerCompletionUnknown
	}
	if r.Events == nil {
		r.Events = []any{}
	}
	return json.NewEncoder(w).Encode(r)
}
