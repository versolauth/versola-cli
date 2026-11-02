package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// fakeLatest returns a latest-release lookup answering tag (or err) and
// counting its calls.
func fakeLatest(tag string, err error, calls *int) func(time.Duration) (string, error) {
	return func(time.Duration) (string, error) {
		*calls++
		return tag, err
	}
}

func TestResolveVersionExplicitQueriesNothing(t *testing.T) {
	for _, target := range []string{"local", "vps"} {
		calls := 0
		c, err := resolveVersion("bootstrap", []string{target, "0.6.2"}, &state.State{Target: "local", Version: "0.7.2"}, fakeLatest("0.7.5", nil, &calls))
		if err != nil {
			t.Fatal(err)
		}
		if c.Version != "0.6.2" || c.Latest || c.Note != "" || calls != 0 {
			t.Errorf("%s: choice %+v, %d lookups; want 0.6.2 as given, no note, no lookup", target, c, calls)
		}
	}
}

func TestResolveVersionFreshLocalTakesLatest(t *testing.T) {
	for name, prev := range map[string]*state.State{
		"nothing deployed": nil,
		"vps record":       {Target: "vps", Version: "0.7.2"},
		"no version":       {Target: "local"},
	} {
		calls := 0
		c, err := resolveVersion("bootstrap", []string{"local"}, prev, fakeLatest("0.7.5", nil, &calls))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "Deploying Versola 0.7.5 (the latest release; pass a version to pick another)."
		if c.Version != "0.7.5" || !c.Latest || c.Note != want || calls != 1 {
			t.Errorf("%s: choice %+v, %d lookups", name, c, calls)
		}
	}
}

func TestResolveVersionKeepsDeployedLocal(t *testing.T) {
	prev := &state.State{Target: "local", Version: "0.7.2"}
	for _, tc := range []struct {
		name   string
		latest string
		err    error
		note   string
	}{
		{"newer release", "0.8.0", nil, "Keeping Versola 0.7.2 (deployed; 0.8.0 is the latest, pass it to move)."},
		{"already latest", "0.7.2", nil, "Keeping Versola 0.7.2 (deployed, and the latest release)."},
		{"offline", "", errors.New("dial tcp: no route to host"), "Keeping Versola 0.7.2 (deployed; pass a version to move)."},
	} {
		calls := 0
		c, err := resolveVersion("configure", []string{"local"}, prev, fakeLatest(tc.latest, tc.err, &calls))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// Latest stays false even when the deployed one is the latest: the
		// version came from the record, and its image was there to deploy it.
		if c.Version != "0.7.2" || c.Latest || c.Note != tc.note {
			t.Errorf("%s: choice %+v", tc.name, c)
		}
	}
}

func TestResolveVersionVpsNeedsOne(t *testing.T) {
	calls := 0
	_, err := resolveVersion("bootstrap", []string{"vps"}, nil, fakeLatest("0.7.5", nil, &calls))
	if err == nil || !strings.Contains(err.Error(), "versola bootstrap vps needs the Versola version") ||
		!strings.Contains(err.Error(), "versola bootstrap vps <version> --auth-url https://id.example.com") ||
		!strings.Contains(err.Error(), "versola versions") {
		t.Errorf("err = %v", err)
	}
	// A server already deployed: its version is the example.
	_, err = resolveVersion("configure", []string{"vps"}, &state.State{Target: "vps", Version: "0.7.2"}, fakeLatest("0.7.5", nil, &calls))
	if err == nil || !strings.Contains(err.Error(), "  versola configure vps 0.7.2\n") {
		t.Errorf("err = %v", err)
	}
	if calls != 0 {
		t.Errorf("vps looked up the latest release %d times; it must never", calls)
	}
}

func TestResolveVersionUnknownTarget(t *testing.T) {
	calls := 0
	if _, err := resolveVersion("bootstrap", []string{"k8s"}, nil, fakeLatest("0.7.5", nil, &calls)); err == nil || !strings.Contains(err.Error(), `unsupported target "k8s"`) || calls != 0 {
		t.Errorf("err = %v, %d lookups", err, calls)
	}
}

func TestResolveVersionLatestUnavailable(t *testing.T) {
	calls := 0
	_, err := resolveVersion("bootstrap", []string{"local"}, nil, fakeLatest("", errors.New("GitHub API returned 403 Forbidden"), &calls))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"couldn't determine the latest Versola release (GitHub API returned 403 Forbidden)", "versola bootstrap local <version>", "versola versions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
}

func TestExplainMissingImage(t *testing.T) {
	missing := fmt.Errorf("wrapped: %w", &deploy.ToolsImageMissingError{Version: "0.8.0"})
	err := explainMissingImage(missing, versionChoice{Version: "0.8.0", Latest: true}, "bootstrap")
	if !strings.Contains(err.Error(), "the latest Versola release, 0.8.0, has no versola-tools image yet") || !strings.Contains(err.Error(), "versola versions") {
		t.Errorf("latest: err = %v", err)
	}
	// Typed (or kept) version: the ordinary explanation, "v" prefix and all.
	err = explainMissingImage(missing, versionChoice{Version: "v0.8.0"}, "bootstrap")
	if err != missing || !strings.Contains(err.Error(), `WITHOUT a leading "v"`) || !strings.Contains(err.Error(), "versola versions") {
		t.Errorf("typed: err = %v", err)
	}
	other := errors.New("docker is not running")
	if got := explainMissingImage(other, versionChoice{Version: "0.8.0", Latest: true}, "bootstrap"); got != other {
		t.Errorf("other errors must pass through, got %v", got)
	}
}

// withReleaseAPI points the GitHub lookups at handler (nil: a server that is
// already closed, i.e. unreachable) and isolates HOME, so no real state.json
// is read.
func withReleaseAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Where state.Save writes, for the tests that record a deployment.
	if err := os.MkdirAll(filepath.Join(home, ".versola", "active"), 0o700); err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	if handler == nil {
		srv = httptest.NewServer(http.NotFoundHandler())
		srv.Close()
	} else {
		srv = httptest.NewServer(handler)
		t.Cleanup(srv.Close)
	}
	old := releaseAPI
	releaseAPI = srv.URL
	t.Cleanup(func() { releaseAPI = old })
}

func TestChooseVersionPrintsTheLatestLine(t *testing.T) {
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/versolauth/versola/releases/latest" {
			t.Errorf("asked for %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"tag_name":"0.7.5"}`)
	})
	var out bytes.Buffer
	c, err := chooseVersion(&out, "bootstrap", []string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "0.7.5" || out.String() != "Deploying Versola 0.7.5 (the latest release; pass a version to pick another).\n" {
		t.Errorf("choice %+v, printed %q", c, out.String())
	}
}

func TestChooseVersionKeepsRecordedLocal(t *testing.T) {
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":"0.8.0"}`) })
	if err := (&state.State{Target: "local", Version: "0.7.2", BundleDir: "bundle-1"}).Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c, err := chooseVersion(&out, "configure", []string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "0.7.2" || !strings.HasPrefix(out.String(), "Keeping Versola 0.7.2 (deployed; 0.8.0 is the latest") {
		t.Errorf("choice %+v, printed %q", c, out.String())
	}
}

// The wiring in the commands themselves, up to the point where they would
// need Docker: one argument goes through the resolver, under the lock, before
// anything is pulled.
func TestBootstrapAndConfigureResolveWithoutAVersion(t *testing.T) {
	withReleaseAPI(t, nil)
	for name, run := range map[string]func() error{
		"bootstrap": func() error { return runBootstrap(bootstrapCmd, []string{"local"}) },
		"configure": func() error { return runConfigure(configureCmd, []string{"local"}) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "couldn't determine the latest Versola release") {
			t.Errorf("%s local, GitHub unreachable: err = %v", name, err)
		}
	}
	for name, run := range map[string]func() error{
		"bootstrap": func() error { return runBootstrap(bootstrapCmd, []string{"vps"}) },
		"configure": func() error { return runConfigure(configureCmd, []string{"vps"}) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "versola "+name+" vps needs the Versola version") {
			t.Errorf("%s vps: err = %v", name, err)
		}
	}
}

func TestVersionArgIsOptional(t *testing.T) {
	for _, c := range []struct {
		name string
		args func([]string) error
	}{{"bootstrap", func(a []string) error { return bootstrapCmd.Args(bootstrapCmd, a) }}, {"configure", func(a []string) error { return configureCmd.Args(configureCmd, a) }}} {
		if err := c.args([]string{"local"}); err != nil {
			t.Errorf("%s local: %v", c.name, err)
		}
		if err := c.args([]string{"local", "0.7.5"}); err != nil {
			t.Errorf("%s local 0.7.5: %v", c.name, err)
		}
		if err := c.args(nil); err == nil {
			t.Errorf("%s with no arguments must be refused", c.name)
		}
		if err := c.args([]string{"local", "0.7.5", "x"}); err == nil {
			t.Errorf("%s with three arguments must be refused", c.name)
		}
	}
}
