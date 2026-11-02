package release

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, status int, body string, gotPath *string, gotUA *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.URL.RequestURI()
		}
		if gotUA != nil {
			*gotUA = r.Header.Get("User-Agent")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), BaseURL: srv.URL, UserAgent: "versola-cli/test"}
}

func TestLatest(t *testing.T) {
	var path, ua string
	c := serve(t, 200, `{"tag_name":"0.7.5","prerelease":false}`, &path, &ua)
	tag, err := c.Latest(VersolaRepo)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "0.7.5" {
		t.Errorf("tag = %q", tag)
	}
	if path != "/repos/versolauth/versola/releases/latest" {
		t.Errorf("path = %q", path)
	}
	if ua != "versola-cli/test" {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestLatestErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"non-200", `{"message":"Not Found"}`, "GitHub API returned 404", 404},
		{"rate limited", `{}`, "GitHub API returned 403", 403},
		{"empty tag", `{"tag_name":""}`, "didn't include a release tag", 200},
		{"empty body", ``, "couldn't parse", 200},
		{"bad json", `{"tag_name":`, "couldn't parse", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := serve(t, tc.status, tc.body, nil, nil).Latest(VersolaRepo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLatestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{HTTP: http.DefaultClient, BaseURL: url, UserAgent: "x"}
	if _, err := c.Latest(VersolaRepo); err == nil {
		t.Fatal("expected an error from a closed server")
	}
}

// Out of order on purpose (List sorts), with a draft, an empty tag and two
// pre-releases.
const listBody = `[
 {"tag_name":"0.7.4","published_at":"2026-10-10T08:00:00Z"},
 {"tag_name":"0.7.5","published_at":"2026-10-21T08:00:00Z"},
 {"tag_name":"0.8.0-rc1","prerelease":true,"published_at":"2026-10-25T08:00:00Z"},
 {"tag_name":"0.9.0","draft":true,"published_at":"2026-10-30T08:00:00Z"},
 {"tag_name":"","published_at":"2026-10-29T08:00:00Z"},
 {"tag_name":"0.7.3","created_at":"2026-10-01T08:00:00Z"},
 {"tag_name":"0.2.1","prerelease":true,"published_at":"2026-01-01T08:00:00Z"}
]`

func tags(rs []Release) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Tag)
	}
	return strings.Join(s, ",")
}

func TestList(t *testing.T) {
	var path string
	c := serve(t, 200, listBody, &path, nil)
	rs, err := c.List(VersolaRepo, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := tags(rs); got != "0.7.5,0.7.4,0.7.3" {
		t.Errorf("tags = %s", got)
	}
	if path != "/repos/versolauth/versola/releases?per_page=100" {
		t.Errorf("path = %q", path)
	}
	// published_at missing: created_at stands in.
	if d := rs[2].Published.Format("2006-01-02"); d != "2026-10-01" {
		t.Errorf("0.7.3 date = %s", d)
	}
}

func TestListIncludePre(t *testing.T) {
	rs, err := serve(t, 200, listBody, nil, nil).List(VersolaRepo, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := tags(rs); got != "0.8.0-rc1,0.7.5,0.7.4,0.7.3,0.2.1" {
		t.Errorf("tags = %s", got)
	}
	if !rs[0].Prerelease || rs[1].Prerelease {
		t.Errorf("prerelease flags wrong: %+v", rs[:2])
	}
}

func TestListLimit(t *testing.T) {
	c := serve(t, 200, listBody, nil, nil)
	for _, tc := range []struct {
		limit int
		want  string
	}{
		{2, "0.7.5,0.7.4"},
		{0, "0.7.5"}, // clamped up to 1
		{1000, "0.7.5,0.7.4,0.7.3"},
	} {
		rs, err := c.List(VersolaRepo, false, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if got := tags(rs); got != tc.want {
			t.Errorf("limit %d: tags = %s, want %s", tc.limit, got, tc.want)
		}
	}
}

func TestListErrors(t *testing.T) {
	if _, err := serve(t, 500, `oops`, nil, nil).List(VersolaRepo, false, 10); err == nil || !strings.Contains(err.Error(), "GitHub API returned 500") {
		t.Errorf("non-200: err = %v", err)
	}
	if _, err := serve(t, 200, `{"not":"a list"}`, nil, nil).List(VersolaRepo, false, 10); err == nil || !strings.Contains(err.Error(), "couldn't parse") {
		t.Errorf("bad json: err = %v", err)
	}
}
