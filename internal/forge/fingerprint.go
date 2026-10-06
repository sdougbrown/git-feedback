package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// FingerprintInput carries the fingerprint-relevant fields of one feedback
// object. Volatile fields (isOutdated, line, position, diff hunk, commit
// OIDs, and all timestamps) are deliberately absent.
type FingerprintInput struct {
	ID              string
	Kind            Kind
	Author          string
	Body            string
	Path            string
	ResolutionState string
	ReviewState     string
	Comments        []ThreadComment
}

// Fingerprint returns the sha256 hex digest of the canonical JSON (sorted
// keys, no whitespace) of the pinned fingerprint fields. Thread comments are
// ordered by (createdAt, id); only their id, author login, and body appear in
// the fingerprint. It is pure: Stages 2 and 3 call it and never reimplement
// it.
func Fingerprint(in FingerprintInput) string {
	doc := map[string]any{
		"id":               in.ID,
		"kind":             string(in.Kind),
		"author":           in.Author,
		"body":             in.Body,
		"path":             in.Path,
		"resolution_state": in.ResolutionState,
		"review_state":     in.ReviewState,
	}
	if in.Kind == KindThread {
		comments := make([]ThreadComment, len(in.Comments))
		copy(comments, in.Comments)
		sort.SliceStable(comments, func(i, j int) bool {
			if !comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
				return comments[i].CreatedAt.Before(comments[j].CreatedAt)
			}
			return comments[i].ID < comments[j].ID
		})
		docs := make([]map[string]any, 0, len(comments))
		for _, c := range comments {
			docs = append(docs, map[string]any{
				"id":     c.ID,
				"author": c.Author,
				"body":   c.Body,
			})
		}
		doc["comments"] = docs
	}
	// encoding/json emits map keys sorted and without insignificant
	// whitespace, which is the pinned canonical form.
	b, err := json.Marshal(doc)
	if err != nil {
		// The document contains only strings; marshalling cannot fail.
		panic("forge.Fingerprint: canonical JSON marshal failed: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FingerprintThread computes the fingerprint of a review thread.
func FingerprintThread(t Thread) string {
	return Fingerprint(FingerprintInput{
		ID:              t.ID,
		Kind:            KindThread,
		Author:          t.Author,
		Body:            t.Body,
		Path:            t.Path,
		ResolutionState: t.ResolutionState,
		Comments:        t.Comments,
	})
}

// FingerprintReview computes the fingerprint of a submitted review.
func FingerprintReview(r Review) string {
	return Fingerprint(FingerprintInput{
		ID:          r.ID,
		Kind:        KindReview,
		Author:      r.Author,
		Body:        r.Body,
		ReviewState: r.State,
	})
}

// FingerprintComment computes the fingerprint of a top-level comment.
func FingerprintComment(c Comment) string {
	return Fingerprint(FingerprintInput{
		ID:     c.ID,
		Kind:   KindComment,
		Author: c.Author,
		Body:   c.Body,
	})
}
