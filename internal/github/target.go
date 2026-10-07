// Package github implements the forge.Adapter for GitHub.com: target URL
// parsing, bounded gh-auth lookup, a read-only transport with rate
// gating and pacing, and complete pull request collection (threads,
// reviews, and issue comments) normalized into forge objects.
package github

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

const forgeName = "github"

// prURLPattern matches a GitHub.com pull request URL with an optional
// trailing path, query, or fragment. The match is case-insensitive so that
// display spellings of the host, owner, and repository are accepted.
var prURLPattern = regexp.MustCompile(`(?i)^https://github\.com/([^/#?]+)/([^/#?]+)/pull/([0-9]+)(?:[/?#].*)?$`)

// ParseTarget parses a GitHub.com pull request URL into a forge.Target. The
// target ID uses the normalized (lowercase) host and repository; the URL
// retains the input spelling. Every other host — including github
// Enterprise and gitlab.com — returns an error wrapping
// forge.ErrUnsupportedHost; a github.com URL that is not a pull request
// returns forge.ErrNotFound.
func ParseTarget(raw string) (forge.Target, error) {
	m := prURLPattern.FindStringSubmatch(raw)
	if m != nil {
		number, err := strconv.Atoi(m[3])
		if err != nil {
			return forge.Target{}, fmt.Errorf("%w: %q is not a pull request URL", forge.ErrNotFound, raw)
		}
		return forge.NewTarget(forgeName, "github.com", m[1]+"/"+m[2], number, raw), nil
	}

	host := ""
	if u, uerr := url.Parse(raw); uerr == nil {
		host = u.Hostname()
	}
	if host != "" && forge.CanonicalHost(host) != "github.com" {
		return forge.Target{}, fmt.Errorf("%w: %q", forge.ErrUnsupportedHost, host)
	}
	return forge.Target{}, fmt.Errorf("%w: %q is not a GitHub.com pull request URL", forge.ErrNotFound, raw)
}
