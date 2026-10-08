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
	ID                string
	DatabaseID        int64
	URL               string
	Author            string
	Body              string
	CommitOID         string
	OriginalCommitOID string
	CreatedAt         time.Time
}

// Thread is a review thread with its nested comments and resolution state.
// RootCommentDatabaseID is the REST id of the root comment, the target of
// pulls/N/comments/{id}/replies. Line fields are nil when GitHub reports no
// position, as for outdated threads.
type Thread struct {
	ID                    string
	Author                string
	Body                  string
	Path                  string
	Line                  *int
	OriginalLine          *int
	StartLine             *int
	OriginalStartLine     *int
	RootCommentID         string
	RootCommentDatabaseID int64
	URL                   string
	CommitOID             string
	OriginalCommitOID     string
	IsOutdated            bool
	ResolutionState       string
	Comments              []ThreadComment
}

// Review is a submitted review. CommitID is informational only: an edited
// review keeps its original commit association and must never be read as
// proof the body was evaluated against that commit.
type Review struct {
	ID       string
	Author   string
	Body     string
	State    string
	CommitID string
}

// Comment is a top-level pull request comment.
type Comment struct {
	ID        string
	Author    string
	Body      string
	CreatedAt time.Time
}

// Snapshot is one complete collected inventory of a target's feedback.
type Snapshot struct {
	Head           string
	CollectedStart time.Time
	CollectedEnd   time.Time
	Threads        []Thread
	Reviews        []Review
	Comments       []Comment
}

// RateInfo reports provider rate-limit accounting for one resource.
type RateInfo struct {
	Resource  string
	Remaining int
	Limit     int
	Reset     time.Time
}
