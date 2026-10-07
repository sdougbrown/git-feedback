package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// restReview is one entry of GET pulls/N/reviews.
type restReview struct {
	ID       int    `json:"id"`
	Body     string `json:"body"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	User     *struct {
		Login string `json:"login"`
	} `json:"user"`
}

// restComment is one entry of GET issues/N/comments.
type restComment struct {
	ID        int       `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      *struct {
		Login string `json:"login"`
	} `json:"user"`
}

// restHead reads the pull request head SHA from GET pulls/N.
func (a *Adapter) restHead(ctx context.Context, tp *Transport, t forge.Target) (string, error) {
	status, _, body, _, _, err := tp.Get(ctx, headPath(t), nil)
	if err != nil {
		return "", fmt.Errorf("%w: head read failed: %v", forge.ErrIncomplete, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: head read returned HTTP %d", forge.ErrIncomplete, status)
	}
	var pull struct {
		Head *struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pull); err != nil {
		return "", fmt.Errorf("%w: malformed head response: %v", forge.ErrIncomplete, err)
	}
	if pull.Head == nil || pull.Head.SHA == "" {
		return "", fmt.Errorf("%w: head response missing head.sha", forge.ErrIncomplete)
	}
	return pull.Head.SHA, nil
}

func headPath(t forge.Target) string {
	return fmt.Sprintf("/repos/%s/pulls/%d", t.Repo, t.Number)
}

func reviewsPath(t forge.Target) string {
	return fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d", t.Repo, t.Number, graphqlPageSize)
}

func commentsPath(t forge.Target) string {
	return fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d", t.Repo, t.Number, graphqlPageSize)
}

// collectThreads walks the outer reviewThreads pagination and then the
// nested comments pagination of every thread that continues past its first
// batched page.
func (a *Adapter) collectThreads(ctx context.Context, tp *Transport, t forge.Target, st *collectState) ([]gqlThreadNode, error) {
	var threads []gqlThreadNode
	cursor := ""
	seen := map[string]bool{}
	for {
		vars := map[string]any{
			"owner":  ownerOf(t),
			"name":   nameOf(t),
			"number": t.Number,
		}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		data, gqlErrs, info, hasInfo, err := tp.GraphQL(ctx, threadsQuery, vars)
		if err != nil {
			return nil, fmt.Errorf("%w: threads query failed: %v", forge.ErrIncomplete, err)
		}
		if hasInfo {
			st.rates = append(st.rates, info)
		}
		if len(gqlErrs) > 0 {
			return nil, fmt.Errorf("%w: GraphQL errors: %s", forge.ErrIncomplete, joinErrs(gqlErrs))
		}
		var payload gqlThreadsData
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, fmt.Errorf("%w: malformed threads response: %v", forge.ErrIncomplete, err)
		}
		if err := a.recordGQLRateLimit(payload.RateLimit); err != nil {
			return nil, err
		}
		rt := payload.Repository.PullRequest.ReviewThreads
		for _, node := range rt.Nodes {
			filled, err := a.fillThreadComments(ctx, tp, node, st)
			if err != nil {
				return nil, err
			}
			threads = append(threads, filled)
		}
		if !rt.PageInfo.HasNextPage {
			return threads, nil
		}
		next := ""
		if rt.PageInfo.EndCursor != nil {
			next = *rt.PageInfo.EndCursor
		}
		if next == "" || seen[next] {
			return nil, fmt.Errorf("%w: thread pagination did not terminate", forge.ErrIncomplete)
		}
		seen[next] = true
		cursor = next
	}
}

// fillThreadComments fetches remaining nested comment pages for one thread.
func (a *Adapter) fillThreadComments(ctx context.Context, tp *Transport, node gqlThreadNode, st *collectState) (gqlThreadNode, error) {
	seen := map[string]bool{}
	for node.Comments.PageInfo.HasNextPage {
		cursor := ""
		if node.Comments.PageInfo.EndCursor != nil {
			cursor = *node.Comments.PageInfo.EndCursor
		}
		if cursor == "" {
			return node, fmt.Errorf("%w: nested comment pagination missing cursor on thread %s", forge.ErrIncomplete, node.ID)
		}
		if seen[cursor] {
			return node, fmt.Errorf("%w: nested comment pagination repeated cursor %q on thread %s", forge.ErrIncomplete, cursor, node.ID)
		}
		seen[cursor] = true
		vars := map[string]any{"id": node.ID, "cursor": cursor}
		data, gqlErrs, info, hasInfo, err := tp.GraphQL(ctx, threadCommentsQuery, vars)
		if err != nil {
			return node, fmt.Errorf("%w: nested comments query failed: %v", forge.ErrIncomplete, err)
		}
		if hasInfo {
			st.rates = append(st.rates, info)
		}
		if len(gqlErrs) > 0 {
			return node, fmt.Errorf("%w: GraphQL errors: %s", forge.ErrIncomplete, joinErrs(gqlErrs))
		}
		var payload gqlThreadCommentsData
		if err := json.Unmarshal(data, &payload); err != nil {
			return node, fmt.Errorf("%w: malformed nested comments response: %v", forge.ErrIncomplete, err)
		}
		if err := a.recordGQLRateLimit(payload.RateLimit); err != nil {
			return node, err
		}
		node.Comments.Nodes = append(node.Comments.Nodes, payload.Node.Comments.Nodes...)
		node.Comments.PageInfo = payload.Node.Comments.PageInfo
	}
	return node, nil
}

func (a *Adapter) collectReviews(ctx context.Context, tp *Transport, t forge.Target, st *collectState) ([]restReview, error) {
	raws, rates, err := a.restPaginate(ctx, tp, reviewsPath(t))
	if err != nil {
		return nil, err
	}
	st.rates = append(st.rates, rates...)
	var reviews []restReview
	for _, raw := range raws {
		var rv restReview
		if err := json.Unmarshal(raw, &rv); err != nil {
			return nil, fmt.Errorf("%w: malformed review record: %v", forge.ErrIncomplete, err)
		}
		reviews = append(reviews, rv)
	}
	return reviews, nil
}

func (a *Adapter) collectComments(ctx context.Context, tp *Transport, t forge.Target, st *collectState) ([]restComment, error) {
	raws, rates, err := a.restPaginate(ctx, tp, commentsPath(t))
	if err != nil {
		return nil, err
	}
	st.rates = append(st.rates, rates...)
	var comments []restComment
	for _, raw := range raws {
		var cm restComment
		if err := json.Unmarshal(raw, &cm); err != nil {
			return nil, fmt.Errorf("%w: malformed comment record: %v", forge.ErrIncomplete, err)
		}
		comments = append(comments, cm)
	}
	return comments, nil
}

func joinErrs(msgs []string) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += "; "
		}
		out += m
	}
	return out
}

func ownerOf(t forge.Target) string {
	for i := 0; i < len(t.Repo); i++ {
		if t.Repo[i] == '/' {
			return t.Repo[:i]
		}
	}
	return t.Repo
}

func nameOf(t forge.Target) string {
	for i := 0; i < len(t.Repo); i++ {
		if t.Repo[i] == '/' {
			return t.Repo[i+1:]
		}
	}
	return t.Repo
}

// normalizeThread converts a GraphQL thread node into a forge.Thread. The
// root comment supplies the thread author and body; the remaining comments
// are the thread's replies. Resolution state is carried from isResolved.
func normalizeThread(node gqlThreadNode) (*forge.Thread, error) {
	if len(node.Comments.Nodes) == 0 {
		return nil, fmt.Errorf("%w: thread %s has no root comment", forge.ErrIncomplete, node.ID)
	}
	root := node.Comments.Nodes[0]
	rootAuthor := ""
	if root.Author != nil {
		rootAuthor = root.Author.Login
	}
	ft := &forge.Thread{
		ID:              node.ID,
		Author:          rootAuthor,
		Body:            root.Body,
		Path:            node.Path,
		IsOutdated:      node.IsOutdated,
		ResolutionState: "unresolved",
	}
	if node.IsResolved {
		ft.ResolutionState = "resolved"
	}
	for _, c := range node.Comments.Nodes[1:] {
		author := ""
		if c.Author != nil {
			author = c.Author.Login
		}
		ft.Comments = append(ft.Comments, forge.ThreadComment{
			ID:        c.ID,
			Author:    author,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return ft, nil
}

// normalizeReview converts a REST review. CommitID is informational only:
// it is never proof the body was evaluated against that head.
func normalizeReview(rv restReview) (*forge.Review, error) {
	author := ""
	if rv.User != nil {
		author = rv.User.Login
	}
	return &forge.Review{
		ID:       strconv.Itoa(rv.ID),
		Author:   author,
		Body:     rv.Body,
		State:    rv.State,
		CommitID: rv.CommitID,
	}, nil
}

// normalizeComment converts a REST issue comment.
func normalizeComment(cm restComment) (*forge.Comment, error) {
	author := ""
	if cm.User != nil {
		author = cm.User.Login
	}
	return &forge.Comment{
		ID:        strconv.Itoa(cm.ID),
		Author:    author,
		Body:      cm.Body,
		CreatedAt: cm.CreatedAt,
	}, nil
}
