package forge

import (
	"errors"
	"testing"
	"time"
)

func TestFingerprintExcludesVolatileFields(t *testing.T) {
	thread := Thread{
		ID:              "t1",
		Author:          "alice",
		Body:            "please fix",
		Path:            "cmd/main.go",
		IsOutdated:      false,
		ResolutionState: "unresolved",
		Comments: []ThreadComment{
			{ID: "c1", Author: "bob", Body: "on it", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: "c2", Author: "alice", Body: "done", CreatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		},
	}
	base := FingerprintThread(thread)

	// isOutdated flips: fingerprint unchanged.
	outdated := thread
	outdated.IsOutdated = true
	if FingerprintThread(outdated) != base {
		t.Error("fingerprint changed when IsOutdated changed")
	}

	// Comment timestamps shift without changing (createdAt, id) order:
	// fingerprint unchanged.
	shifted := thread
	shifted.Comments = []ThreadComment{
		{ID: "c1", Author: "bob", Body: "on it", CreatedAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)},
		{ID: "c2", Author: "alice", Body: "done", CreatedAt: time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)},
	}
	if FingerprintThread(shifted) != base {
		t.Error("fingerprint changed when comment timestamps changed")
	}

	// Content changes: fingerprint changes.
	edited := thread
	edited.Body = "please fix this"
	if FingerprintThread(edited) == base {
		t.Error("fingerprint did not change when the body changed")
	}

	// Comment body changes: fingerprint changes.
	replied := thread
	replied.Comments = make([]ThreadComment, len(thread.Comments))
	copy(replied.Comments, thread.Comments)
	replied.Comments[0].Body = "still on it"
	if FingerprintThread(replied) == base {
		t.Error("fingerprint did not change when a comment body changed")
	}

	// Comments supplied in reversed order: fingerprint unchanged.
	reversed := thread
	reversed.Comments = []ThreadComment{
		{ID: "c2", Author: "alice", Body: "done", CreatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{ID: "c1", Author: "bob", Body: "on it", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	if FingerprintThread(reversed) != base {
		t.Error("fingerprint changed when comments were supplied in reversed order")
	}

	// Equal-timestamp comments: the ID tiebreak must be visible.
	// Swapping IDs between equal-timestamp comments changes the fingerprint.
	tieA := thread
	tieA.Comments = []ThreadComment{
		{ID: "a", Author: "x", Body: "hello", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "b", Author: "y", Body: "world", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	tieB := thread
	tieB.Comments = []ThreadComment{
		{ID: "b", Author: "x", Body: "hello", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "a", Author: "y", Body: "world", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	if FingerprintThread(tieA) == FingerprintThread(tieB) {
		t.Error("fingerprint did not change when equal-timestamp comment IDs were swapped")
	}

	// Resolution state changes: fingerprint changes.
	resolved := thread
	resolved.ResolutionState = "resolved"
	if FingerprintThread(resolved) == base {
		t.Error("fingerprint did not change when resolution state changed")
	}

	// Reviews: commit OID and review state handling.
	review := Review{ID: "r1", Author: "alice", Body: "lgtm", State: "approved", CommitID: "aaaaaaaa"}
	reviewFP := FingerprintReview(review)
	reviewCommitOIDChanged := review
	reviewCommitOIDChanged.CommitID = "bbbbbbbb"
	if FingerprintReview(reviewCommitOIDChanged) != reviewFP {
		t.Error("fingerprint changed when the review commit OID changed")
	}
	reviewStateChanged := review
	reviewStateChanged.State = "changes_requested"
	if FingerprintReview(reviewStateChanged) == reviewFP {
		t.Error("fingerprint did not change when the review state changed")
	}

	// Same canonical fields across kinds produce different fingerprints.
	sameFields := FingerprintInput{ID: "x", Kind: KindComment, Body: "b"}
	otherKind := sameFields
	otherKind.Kind = KindReview
	if Fingerprint(sameFields) == Fingerprint(otherKind) {
		t.Error("fingerprint ignored the object kind")
	}

	// Pin the digest algorithm: 64 lowercase hex chars.
	fp := Fingerprint(sameFields)
	if len(fp) != 64 {
		t.Fatalf("fingerprint length = %d, want 64", len(fp))
	}
	for i, c := range fp {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("fingerprint[%d] = %c, want lowercase hex", i, c)
		}
	}

	// Known vector: sha256 of the canonical JSON for a minimal comment.
	const wantFP = "60c26e4e20fee353dd277a8ef42fdec148e440e9b3c2f770e3ee4384871c797c"
	if fp != wantFP {
		t.Errorf("Fingerprint(sameFields) = %s, want %s", fp, wantFP)
	}
}

func TestTargetIDNormalization(t *testing.T) {
	target := NewTarget("github", "GitHub.com", "Owner/Repo", 123, "https://github.com/Owner/Repo/pull/123")
	want := "github:github.com:owner/repo:123"
	if target.ID != want {
		t.Fatalf("Target.ID = %q, want %q", target.ID, want)
	}
	if target.Host != "GitHub.com" || target.Repo != "Owner/Repo" {
		t.Fatalf("display spelling not retained: host=%q repo=%q", target.Host, target.Repo)
	}
	if CanonicalHost("GitHub.com") != "github.com" {
		t.Error("CanonicalHost did not lowercase the host")
	}
	if CanonicalRepo("Owner/Repo") != "owner/repo" {
		t.Error("CanonicalRepo did not lowercase the repository path")
	}
	if CanonicalAccount("Alice") != "alice" {
		t.Error("CanonicalAccount did not lowercase the login")
	}
}

func TestRateLimitError(t *testing.T) {
	until := time.Date(2024, 1, 1, 1, 2, 3, 0, time.UTC)
	err := &ErrRateLimited{Resource: "graphql", Until: until}
	var target *ErrRateLimited
	if !errors.As(err, &target) || target.Resource != "graphql" || !target.Until.Equal(until) {
		t.Fatalf("errors.As failed or fields lost: %v", err)
	}
}
