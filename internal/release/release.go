// Package release asks GitHub which releases a repository has: the latest
// one (what "versola upgrade" installs for the CLI itself, and what
// "versola bootstrap local" deploys when no Versola version is given) and a
// short list of recent ones ("versola versions").
//
// Deliberately small and dependency-free, like the rest of the CLI's GitHub
// use (install.sh, install.ps1, upgrade): two public, unauthenticated REST
// endpoints and a JSON decoder. No token is read from the environment: the
// anonymous rate limit (60 requests an hour per IP) is far above what one
// person typing these commands needs, and a token picked up silently from
// GITHUB_TOKEN would be one more thing that behaves differently on a CI box
// than on the customer's laptop.
package release

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// GitHubAPI is the base URL of GitHub's REST API. Tests point a Client at an
// httptest server instead.
const GitHubAPI = "https://api.github.com"

// VersolaRepo is the Versola product repository: its release tags are the
// versions "bootstrap"/"configure" deploy (and the tags of the versola-tools
// image). Not to be confused with versola-cli's own repository, whose
// releases "upgrade" installs.
const VersolaRepo = "versolauth/versola"

// MaxList is the most releases List returns: one page of GitHub's API. More
// would need pagination, and nobody picks a version from further back than a
// hundred releases by reading a terminal.
const MaxList = 100

// Client talks to GitHub's releases API.
type Client struct {
	// HTTP does the requests; its Timeout bounds each one.
	HTTP *http.Client
	// BaseURL is GitHubAPI in production, without a trailing slash.
	BaseURL string
	// UserAgent identifies the caller ("versola-cli/<version>"). Go's own
	// default would satisfy GitHub's bare requirement that one is present,
	// but a client-identifying one is more useful if GitHub ever needs to
	// flag or rate-limit a specific caller.
	UserAgent string
}

// Release is one entry of List.
type Release struct {
	Tag        string
	Published  time.Time
	Prerelease bool
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Prerelease  bool      `json:"prerelease"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Latest returns the tag of repo's latest release. GitHub's "latest" never
// is a pre-release or a draft, by its own definition, so callers can rely on
// that without filtering.
func (c *Client) Latest(repo string) (string, error) {
	var r githubRelease
	if err := c.getJSON(fmt.Sprintf("/repos/%s/releases/latest", repo), &r); err != nil {
		return "", err
	}
	if r.TagName == "" {
		return "", fmt.Errorf("GitHub's response didn't include a release tag")
	}
	return r.TagName, nil
}

// List returns up to limit of repo's releases, newest first, leaving out
// pre-releases unless includePre. limit is clamped to 1..MaxList. Drafts
// (only visible to a token with push access, so never here in practice) are
// always left out: they are not releases anyone can deploy.
//
// The filter is applied to one page of MaxList releases rather than asking
// GitHub for exactly limit: GitHub cannot filter out pre-releases itself, so
// asking for 10 could return 10 pre-releases and leave nothing to show.
func (c *Client) List(repo string, includePre bool, limit int) ([]Release, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > MaxList {
		limit = MaxList
	}
	var raw []githubRelease
	if err := c.getJSON(fmt.Sprintf("/repos/%s/releases?per_page=%d", repo, MaxList), &raw); err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(raw))
	for _, r := range raw {
		if r.Draft || r.TagName == "" || (r.Prerelease && !includePre) {
			continue
		}
		published := r.PublishedAt
		if published.IsZero() {
			published = r.CreatedAt
		}
		out = append(out, Release{Tag: r.TagName, Published: published, Prerelease: r.Prerelease})
	}
	// GitHub returns them newest first in practice, but does not document
	// the order; sorting makes "newest first" true whatever it does. Stable,
	// so releases published in the same second keep GitHub's order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Published.After(out[j].Published) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c *Client) getJSON(path string, into any) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("couldn't parse GitHub's response: %w", err)
	}
	return nil
}
