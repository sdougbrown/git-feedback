// Package github implements the forge.Adapter for GitHub.com: target URL
// parsing, bounded gh-auth lookup, a read-only transport with rate
// gating and pacing, and complete collection of pull request feedback
// (threads, reviews, and issue comments) or of an issue's conversation
// comments.
package github

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

const forgeName = "github"

// targetURLPattern matches a GitHub.com pull request or issue URL with an
// optional trailing path, query, or fragment. The match is case-insensitive
// so that display spellings of the host, owner, and repository are accepted.
// Pull requests and issues share one number sequence per repository, so a
// number names exactly one target.
var targetURLPattern = regexp.MustCompile(`(?i)^https://github\.com/([^/#?]+)/([^/#?]+)/(pull|issues)/([0-9]+)(?:[/?#].*)?$`)

// ParseTarget parses a GitHub.com pull request or issue URL into a
// forge.Target. The target ID uses the normalized (lowercase) host and
// repository; the URL retains the input spelling. A `/pull/N` URL yields a
// pull request target and an `/issues/N` URL yields an issue target; a URL
// that names a pull request through `/issues/N` stays an issue target, since
// the spelling is authoritative and no extra request is made to classify it.
// Every other host — including github Enterprise and gitlab.com — returns an
// error wrapping forge.ErrUnsupportedHost; a github.com URL that is neither a
// pull request nor an issue returns forge.ErrNotFound.
func ParseTarget(raw string) (forge.Target, error) {
	m := targetURLPattern.FindStringSubmatch(raw)
	if m != nil {
		number, err := strconv.Atoi(m[4])
		if err != nil {
			return forge.Target{}, fmt.Errorf("%w: %q is not a pull request or issue URL", forge.ErrNotFound, raw)
		}
		t := forge.NewTarget(forgeName, "github.com", m[1]+"/"+m[2], number, raw)
		if strings.EqualFold(m[3], "issues") {
			t.Kind = forge.TargetKindIssue
		}
		return t, nil
	}

	host := ""
	if u, uerr := url.Parse(raw); uerr == nil {
		host = u.Hostname()
	}
	if host != "" && forge.CanonicalHost(host) != "github.com" {
		return forge.Target{}, fmt.Errorf("%w: %q", forge.ErrUnsupportedHost, host)
	}
	return forge.Target{}, fmt.Errorf("%w: %q is not a GitHub.com pull request or issue URL", forge.ErrNotFound, raw)
}
