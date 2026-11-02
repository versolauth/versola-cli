package cmd

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/release"
	"github.com/versolauth/versola-cli/internal/state"
)

func TestWriteVersions(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
	var out bytes.Buffer
	writeVersions(&out, []release.Release{
		{Tag: "0.8.0-rc1", Published: day("2026-10-25"), Prerelease: true},
		{Tag: "0.7.5", Published: day("2026-10-21")},
		{Tag: "0.7.4", Published: day("2026-10-10")},
		{Tag: "0.7.3"},
	}, "0.7.5", "0.7.5")
	want := "0.8.0-rc1\t2026-10-25\tpre-release\n" +
		"0.7.5\t2026-10-21\tlatest, deployed\n" +
		"0.7.4\t2026-10-10\t\n" +
		"0.7.3\t\t\n"
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
}

const versionsListBody = `[
 {"tag_name":"0.8.0-rc1","prerelease":true,"published_at":"2026-10-25T08:00:00Z"},
 {"tag_name":"0.7.5","published_at":"2026-10-21T08:00:00Z"},
 {"tag_name":"0.7.4","published_at":"2026-10-10T08:00:00Z"},
 {"tag_name":"0.7.3","published_at":"2026-10-01T08:00:00Z"}
]`

func versionsAPI(latestStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/versolauth/versola/releases":
			fmt.Fprint(w, versionsListBody)
		case "/repos/versolauth/versola/releases/latest":
			w.WriteHeader(latestStatus)
			fmt.Fprint(w, `{"tag_name":"0.7.5"}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestListVersions(t *testing.T) {
	withReleaseAPI(t, versionsAPI(200))
	if err := (&state.State{Target: "vps", Version: "0.7.4", BundleDir: "bundle-1"}).Save(); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := listVersions(&out, &errOut, false, 10); err != nil {
		t.Fatal(err)
	}
	want := "0.7.5\t2026-10-21\tlatest\n0.7.4\t2026-10-10\tdeployed\n0.7.3\t2026-10-01\t\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}

	out.Reset()
	if err := listVersions(&out, &errOut, true, 2); err != nil {
		t.Fatal(err)
	}
	if want := "0.8.0-rc1\t2026-10-25\tpre-release\n0.7.5\t2026-10-21\tlatest\n"; out.String() != want {
		t.Errorf("--all --limit 2: got %q, want %q", out.String(), want)
	}
}

func TestListVersionsLatestLookupFailsSoftly(t *testing.T) {
	withReleaseAPI(t, versionsAPI(500))
	var out, errOut bytes.Buffer
	if err := listVersions(&out, &errOut, false, 1); err != nil {
		t.Fatal(err)
	}
	if out.String() != "0.7.5\t2026-10-21\t\n" {
		t.Errorf("got %q", out.String())
	}
}

func TestListVersionsErrors(t *testing.T) {
	withReleaseAPI(t, nil)
	var out, errOut bytes.Buffer
	for _, limit := range []int{0, 101} {
		if err := listVersions(&out, &errOut, false, limit); err == nil || !strings.Contains(err.Error(), "--limit must be between 1 and 100") {
			t.Errorf("limit %d: err = %v", limit, err)
		}
	}
	if err := listVersions(&out, &errOut, false, 10); err == nil || !strings.Contains(err.Error(), "couldn't list the Versola releases") {
		t.Errorf("unreachable: err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing belongs on stdout on an error, got %q", out.String())
	}
}

func TestListVersionsOnlyPrereleases(t *testing.T) {
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"0.1.0","prerelease":true,"published_at":"2026-07-15T08:00:00Z"}]`)
	})
	var out, errOut bytes.Buffer
	if err := listVersions(&out, &errOut, false, 10); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "--all includes pre-releases") {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}
}

func TestVersionsHelpSaysWhatAllMeans(t *testing.T) {
	if u := versionsCmd.Flags().Lookup("all").Usage; u != "include pre-releases" {
		t.Errorf("--all usage = %q", u)
	}
	if d := versionsCmd.Flags().Lookup("limit").DefValue; d != "10" {
		t.Errorf("--limit default = %s", d)
	}
}
