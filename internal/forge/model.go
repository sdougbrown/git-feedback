package forge

import "time"

// Kind enumerates the feedback object kinds.
type Kind string

const (
	KindThread  Kind = "thread"
	KindReview  Kind = "review"
	KindComment Kind = "comment"
	KindTarget  Kind = "target"
)

// ThreadComment is one nested reply inside a review thread.
type ThreadComment struct {
	ID                string    `json:"id"`
	DatabaseID        int64     `json:"database_id"`
	URL               string    `json:"url"`
	Author            string    `json:"author"`
	Body              string    `json:"body"`
	CommitOID         string    `json:"commit_oid"`
	OriginalCommitOID string    `json:"original_commit_oid"`
	CreatedAt         time.Time `json:"created_at"`
}

// Thread is a review thread with its nested comments and resolution state.
// RootCommentDatabaseID is the REST id of the root comment, the target of
// pulls/N/comments/{id}/replies. Line fields are nil when GitHub reports no
// position, as for outdated threads.
type Thread struct {
	ID                    string          `json:"id"`
	Author                string          `json:"author"`
	Body                  string          `json:"body"`
	Path                  string          `json:"path"`
	Line                  *int            `json:"line"`
	OriginalLine          *int            `json:"original_line"`
	StartLine             *int            `json:"start_line"`
	OriginalStartLine     *int            `json:"original_start_line"`
	RootCommentID         string          `json:"root_comment_id"`
	RootCommentDatabaseID int64           `json:"root_comment_database_id"`
	URL                   string          `json:"url"`
	CommitOID             string          `json:"commit_oid"`
	OriginalCommitOID     string          `json:"original_commit_oid"`
	IsOutdated            bool            `json:"is_outdated"`
	ResolutionState       string          `json:"resolution_state"`
	Comments              []ThreadComment `json:"comments"`
}

// Review is a submitted review. CommitID is informational only: an edited
// review keeps its original commit association and must never be read as
// proof the body was evaluated against that commit.
type Review struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

// Comment is a top-level pull request comment.
type Comment struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Snapshot is one complete collected inventory of a target's feedback.
type Snapshot struct {
	Head           string    `json:"head"`
	CollectedStart time.Time `json:"collected_start"`
	CollectedEnd   time.Time `json:"collected_end"`
	Threads        []Thread  `json:"threads"`
	Reviews        []Review  `json:"reviews"`
	Comments       []Comment `json:"comments"`
}

// RateInfo reports provider rate-limit accounting for one resource.
type RateInfo struct {
	Resource  string
	Remaining int
	Limit     int
	Reset     time.Time
}
