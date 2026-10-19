package github

import (
	"errors"
	"testing"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

func TestParseTargetGitHub(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		num  int
		host string
		repo string
		kind string
	}{
		{
			name: "plain",
			raw:  "https://github.com/owner/repo/pull/123",
			want: "github:github.com:owner/repo:123",
			num:  123,
			host: "github.com",
			repo: "owner/repo",
		},
		{
			name: "trailing path",
			raw:  "https://github.com/owner/repo/pull/123/files",
			want: "github:github.com:owner/repo:123",
			num:  123,
		},
		{
			name: "query and fragment",
			raw:  "https://github.com/owner/repo/pull/123?diff=split#discussion_r99",
			want: "github:github.com:owner/repo:123",
			num:  123,
		},
		{
			name: "trailing slash",
			raw:  "https://github.com/owner/repo/pull/123/",
			want: "github:github.com:owner/repo:123",
			num:  123,
		},
		{
			name: "mixed case display spelling",
			raw:  "https://GitHub.com/Owner/Repo/pull/9",
			want: "github:github.com:owner/repo:9",
			num:  9,
			host: "github.com",
			repo: "Owner/Repo",
		},
		{
			name: "issue",
			raw:  "https://github.com/owner/repo/issues/1993",
			want: "github:github.com:owner/repo:1993",
			num:  1993,
			host: "github.com",
			repo: "owner/repo",
			kind: forge.TargetKindIssue,
		},
		{
			name: "issue with comment anchor",
			raw:  "https://github.com/owner/repo/issues/1993#issuecomment-1234",
			want: "github:github.com:owner/repo:1993",
			num:  1993,
			kind: forge.TargetKindIssue,
		},
		{
			name: "issue display spelling",
			raw:  "https://GitHub.com/Owner/Repo/Issues/7/",
			want: "github:github.com:owner/repo:7",
			num:  7,
			host: "github.com",
			repo: "Owner/Repo",
			kind: forge.TargetKindIssue,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTarget(tc.raw)
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tc.raw, err)
			}
			if got.ID != tc.want {
				t.Errorf("ID = %q, want %q", got.ID, tc.want)
			}
			if got.Number != tc.num {
				t.Errorf("Number = %d, want %d", got.Number, tc.num)
			}
			if got.Forge != "github" {
				t.Errorf("forge = %q", got.Forge)
			}
			if tc.host != "" && got.Host != tc.host {
				t.Errorf("Host = %q, want %q", got.Host, tc.host)
			}
			if tc.repo != "" && got.Repo != tc.repo {
				t.Errorf("Repo = %q, want %q", got.Repo, tc.repo)
			}
			if got.URL != tc.raw {
				t.Errorf("URL = %q, want %q", got.URL, tc.raw)
			}
			if got.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.IsIssue() != (tc.kind == forge.TargetKindIssue) {
				t.Errorf("IsIssue() = %t for kind %q", got.IsIssue(), tc.kind)
			}
		})
	}
}

func TestUnsupportedHost(t *testing.T) {
	for _, raw := range []string{
		"https://gitlab.com/owner/repo/-/merge_requests/1",
		"https://example.com/owner/repo/pull/1",
		"https://github.enterprise.example/owner/repo/pull/1",
	} {
		_, err := ParseTarget(raw)
		if !errors.Is(err, forge.ErrUnsupportedHost) {
			t.Errorf("ParseTarget(%q) error = %v, want ErrUnsupportedHost", raw, err)
		}
	}
}

func TestGitHubNonTargetURL(t *testing.T) {
	// A GitHub.com URL that is neither a pull request nor an issue is not a
	// target. An /issues/N URL is a target even when N names a pull request:
	// the spelling decides the target kind (see docs/contract.md).
	for _, raw := range []string{
		"https://github.com/owner/repo",
		"https://github.com/owner/repo/discussions/5",
		"https://github.com/owner/repo/issues",
		"https://github.com/owner/repo/pulls/5",
		// A non-numeric number is not a target: the pattern's ([0-9]+)
		// tail rejects it, so both spellings fall through to ErrNotFound.
		"https://github.com/owner/repo/pull/abc",
		"https://github.com/owner/repo/issues/abc",
	} {
		if _, err := ParseTarget(raw); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("ParseTarget(%q) error = %v, want ErrNotFound", raw, err)
		}
	}
}
