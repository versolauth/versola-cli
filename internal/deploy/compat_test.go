package deploy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

const testManifest = `{
  "schemaVersion": 1,
  "version": "0.7.6",
  "rollingFrom": ["0.7.4", "0.7.5"],
  "freshDatabaseFrom": ["0.6.*"],
  "notes": "https://example.com/notes/0.7.6"
}`

// toolsDir is a versola-tools output directory; body "" leaves release.json out
// (a release from before manifests).
func toolsDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRequireCompatibility(t *testing.T) {
	const to = "0.7.6"
	cases := []struct {
		name     string
		manifest string
		c        compatCheck
		wantErr  []string // substrings; none: must pass
		wantOut  string
		to       string // default 0.7.6, the manifest's own version
	}{
		{name: "rolling declared: no flag needed, says why", manifest: testManifest,
			c: compatCheck{from: "0.7.5", rollout: true}, wantOut: "rolling upgrade supported by the release (release notes: https://example.com/notes/0.7.6)"},
		{name: "rolling declared: a plain configure says nothing", manifest: testManifest,
			c: compatCheck{from: "0.7.5"}},
		{name: "fresh database: a rollout is refused", manifest: testManifest,
			c:       compatCheck{from: "0.6.2", rollout: true},
			wantErr: []string{"needs a fresh database", "rolling upgrade is not possible", "Nothing was changed", "notes/0.7.6"}},
		{name: "fresh database: --assume-compatible does not override it", manifest: testManifest,
			c:       compatCheck{from: "0.6.2", rollout: true, assume: true},
			wantErr: []string{"needs a fresh database"}},
		{name: "fresh database: --allow-fresh-database does not make a rollout possible", manifest: testManifest,
			c:       compatCheck{from: "0.6.2", rollout: true, allowFresh: true},
			wantErr: []string{"needs a fresh database"}},
		{name: "fresh database: configure is refused, with the way out", manifest: testManifest,
			c:       compatCheck{from: "0.6.2"},
			wantErr: []string{"needs a fresh database", "--allow-fresh-database", "versola configure vps 0.7.6", "Nothing was changed"}},
		{name: "fresh database: configure goes on with --allow-fresh-database", manifest: testManifest,
			c: compatCheck{from: "0.6.2", allowFresh: true}, wantOut: "Warning"},
		{name: "undeclared version: a rollout needs the flag, and why", manifest: testManifest,
			c:       compatCheck{from: "0.7.0", rollout: true},
			wantErr: []string{"does not state compatibility with 0.7.0", "--assume-compatible", "Nothing was changed"}},
		{name: "undeclared version: the flag lets it through", manifest: testManifest,
			c: compatCheck{from: "0.7.0", rollout: true, assume: true}},
		{name: "undeclared version: configure goes on as before", manifest: testManifest,
			c: compatCheck{from: "0.7.0"}},
		{name: "no manifest: a rollout needs the flag (never read as compatible)", manifest: "",
			c:       compatCheck{from: "0.7.5", rollout: true},
			wantErr: []string{"publishes no compatibility manifest", "--assume-compatible"}},
		{name: "no manifest: the flag lets it through", manifest: "",
			c: compatCheck{from: "0.7.5", rollout: true, assume: true}},
		{name: "no manifest: configure goes on as before", manifest: "",
			c: compatCheck{from: "0.6.2"}},
		{name: "a manifest for another version is refused (a wrong image)", manifest: testManifest,
			c:       compatCheck{from: "0.7.5", rollout: true, assume: true},
			wantErr: []string{"is for 0.7.6", "wrongly built image", "Nothing was changed"}, to: "0.7.7"},
		{name: "unknown schema is refused, not guessed at", manifest: `{"schemaVersion": 2}`,
			c:       compatCheck{from: "0.7.5", rollout: true, assume: true},
			wantErr: []string{"schemaVersion 2", "versola upgrade"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.dir, tc.c.to = toolsDir(t, tc.manifest), to
			if tc.to != "" {
				tc.c.to = tc.to
			}
			var out bytes.Buffer
			err := requireCompatibility(tc.c, &out)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("want it to pass, got %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("want a refusal with %q, it passed", tc.wantErr)
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q lacks %q", err, w)
					}
				}
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output %q lacks %q", out.String(), tc.wantOut)
			}
			if tc.wantOut == "" && out.Len() != 0 {
				t.Errorf("unexpected output %q", out.String())
			}
		})
	}
}

func TestCompatibilityCheckAsks(t *testing.T) {
	vps := func(version string) *state.State { return &state.State{Target: "vps", Version: version} }
	underway := &state.State{Target: "vps", Version: "0.7.6", PreviousVersion: "0.7.5"}
	cases := []struct {
		name    string
		version string
		stored  *state.State
		staged  *stagedConfigure
		pg      PostgresOptions
		ask     bool
		rollout bool
	}{
		{name: "first install", version: "0.7.6", stored: nil},
		{name: "same version again", version: "0.7.5", stored: vps("0.7.5")},
		{name: "a local record", version: "0.7.6", stored: &state.State{Target: "local", Version: "0.7.5"}},
		{name: "a plain configure over a record", version: "0.7.6", stored: vps("0.7.5"), ask: true},
		{name: "a rollout's prepare step", version: "0.7.6", stored: vps("0.7.5"), staged: &stagedConfigure{from: vps("0.7.5")}, ask: true, rollout: true},
		{name: "an abort goes back without asking", version: "0.7.5", stored: vps("0.7.6"), staged: &stagedConfigure{from: vps("0.7.6"), abort: true}},
		{name: "a rollout under way is not asked again", version: "0.7.6", stored: underway, staged: &stagedConfigure{from: underway}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ask := compatibilityCheck("d", tc.version, tc.stored, tc.staged, tc.pg)
			if ask != tc.ask || (ask && c.rollout != tc.rollout) {
				t.Errorf("ask=%v rollout=%v, want ask=%v rollout=%v", ask, c.rollout, tc.ask, tc.rollout)
			}
		})
	}
	// The flags travel: --allow-fresh-database reaches a configure, --assume-compatible a rollout.
	c, _ := compatibilityCheck("d", "0.7.6", vps("0.6.2"), nil, PostgresOptions{AllowFreshDatabase: true})
	if !c.allowFresh || c.from != "0.6.2" || c.to != "0.7.6" {
		t.Errorf("configure check: %+v", c)
	}
	c, _ = compatibilityCheck("d", "0.7.6", vps("0.7.5"), &stagedConfigure{from: vps("0.7.5"), assumeCompatible: true}, PostgresOptions{})
	if !c.assume || c.allowFresh {
		t.Errorf("rollout check: %+v", c)
	}
}
