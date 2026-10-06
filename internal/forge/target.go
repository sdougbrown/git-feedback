package forge

import (
	"fmt"
	"strings"
)

// Target identifies one change request on one forge, namespaced by forge,
// host, repository identity, and change-request number. Host and Repo retain
// the display spelling as collected; ID uses the canonical (lowercase) forms.
type Target struct {
	Forge  string
	Host   string
	Repo   string
	Number int
	ID     string
	URL    string
}

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
