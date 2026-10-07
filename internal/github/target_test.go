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

func TestGitHubNonPullURL(t *testing.T) {
	_, err := ParseTarget("https://github.com/owner/repo/issues/5")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
