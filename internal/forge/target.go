package forge

import (
	"fmt"
	"strings"
)

// TargetKindIssue marks a target that is an issue rather than a pull
// request. Pull request targets carry no kind: issue targets arrived later,
// and their serialized form predates the field.
const TargetKindIssue = "issue"

// Target identifies one pull request or issue on one forge, namespaced by
// forge, host, repository identity, and number. Pull requests and issues
// share one number sequence per repository, so the number alone identifies
// the target. Host and Repo retain the display spelling as collected; ID
// uses the canonical (lowercase) forms.
type Target struct {
	Forge  string `json:"forge"`
	Host   string `json:"host"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	ID     string `json:"id"`
	URL    string `json:"url"`
	// Kind is empty for a pull request target and TargetKindIssue for an
	// issue target. Use IsIssue rather than comparing Kind directly.
	Kind string `json:"kind,omitempty"`
}

// IsIssue reports whether the target is an issue rather than a pull request.
func (t Target) IsIssue() bool { return t.Kind == TargetKindIssue }

// NewTarget builds a Target whose ID is <forge>:<host>:<repo>:<number> with
// the host and repository normalized to lowercase.
func NewTarget(forge, host, repo string, number int, url string) Target {
	return Target{
		Forge:  forge,
		Host:   host,
		Repo:   repo,
		Number: number,
		ID:     TargetID(forge, host, repo, number),
		URL:    url,
	}
}

// TargetID computes the canonical target identifier.
func TargetID(forge, host, repo string, number int) string {
	return fmt.Sprintf("%s:%s:%s:%d", forge, CanonicalHost(host), CanonicalRepo(repo), number)
}

// CanonicalHost normalizes a forge host for comparison.
func CanonicalHost(host string) string {
	return strings.ToLower(host)
}

// CanonicalRepo normalizes an owner/repository path for comparison.
func CanonicalRepo(repo string) string {
	return strings.ToLower(repo)
}

// CanonicalAccount normalizes an account login for comparison.
func CanonicalAccount(account string) string {
	return strings.ToLower(account)
}
