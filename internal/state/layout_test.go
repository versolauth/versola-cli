package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// activeDir points the state directory at a fresh temp home and returns it.
func activeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	dir := filepath.Join(home, ".versola", "active")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeState(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// What versola-cli 0.2.6 -- the one on the production VPS -- writes: layout 1,
// with a proxy mode and mounted bundles, and nothing of layout 2 or 3.
const prodLayout1 = `{
  "schemaVersion": 1,
  "target": "vps",
  "version": "0.6.2",
  "configuredAt": "2026-10-01T10:00:00Z",
  "migratedAt": "2026-10-01T10:05:00Z",
  "bundleDir": "bundle-1759312800000000000-123",
  "authUrl": "https://id.example.com",
  "proxyMode": "external",
  "mountedBundleDirs": ["bundle-1759312800000000000-123"]
}
`

func TestLoadOlderLayouts(t *testing.T) {
	dir := activeDir(t)
	for name, content := range map[string]string{
		"layout 1 (versola-cli 0.2.6)": prodLayout1,
		"layout 2": `{"schemaVersion":2,"target":"vps","version":"0.6.3","configuredAt":"2026-10-09T00:00:00Z",
			"bundleDir":"bundle-2","authUrl":"https://id.example.com","proxyMode":"nginx","mountedBundleDirs":["bundle-2"],
			"slots":{"auth":[{"n":1,"version":"0.6.3"},{"n":2,"version":"0.6.3"}]},"secretsRevision":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeState(t, dir, content)
			s, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			// Unknown, not invented: a later step asks for what it needs.
			if s.PostgresHost != "" || s.PreviousVersion != "" || s.Surge != nil {
				t.Errorf("layout 3 fields should be empty: %+v", s)
			}
			if err := s.RequireCurrent(); err != nil {
				t.Errorf("a record with a bundle and a proxy mode is current: %v", err)
			}
			if why := s.MixedVersions(); why != "" {
				t.Errorf("one version throughout, got %q", why)
			}
		})
	}
}

func TestLayout3RoundTrip(t *testing.T) {
	activeDir(t)
	migrated := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	want := &State{
		SchemaVersion:     SchemaVersion,
		Target:            "vps",
		Version:           "0.7.0",
		ConfiguredAt:      time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC),
		MigratedAt:        &migrated,
		BundleDir:         "bundle-3",
		AuthURL:           "https://id.example.com",
		ProxyMode:         "external",
		MountedBundleDirs: []string{"bundle-3"},
		Slots:             map[string][]Slot{"auth": {{N: 1, Version: "0.6.3"}, {N: 2, Version: "0.7.0"}}},
		SecretsRevision:   2,
		PostgresHost:      "127.0.0.1:5432",
		PreviousVersion:   "0.6.3",
		Surge:             map[string]int{"auth": 2},
		PostgresDB:        "versola",
		PostgresCA:        PostgresCAFileName,
	}
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}

	dir, _ := Dir()
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"postgresHost", "previousVersion", "surge", "postgresDb", "postgresCa"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("state.json lacks %q", k)
		}
	}
	if string(keys["schemaVersion"]) != "4" {
		t.Errorf("schemaVersion = %s, want 4", keys["schemaVersion"])
	}
}

// Empty layout 3 fields are left out, so a deployment without an upgrade
// under way reads as it did.
func TestLayout3EmptyFieldsOmitted(t *testing.T) {
	b, err := json.Marshal(&State{SchemaVersion: SchemaVersion, Target: "local", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"postgresHost", "previousVersion", "surge", "postgresDb", "postgresCa"} {
		if strings.Contains(string(b), k) {
			t.Errorf("%s written while empty: %s", k, b)
		}
	}
}

func TestLoadRefusesNewerLayout(t *testing.T) {
	dir := activeDir(t)
	writeState(t, dir, `{"schemaVersion":5,"target":"vps","version":"9.9.9"}`)
	if _, err := Load(); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("got %v, want ErrNewerSchema", err)
	}
}

func TestLoadWithoutStateJSON(t *testing.T) {
	dir := activeDir(t)
	if _, err := Load(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty directory: got %v, want ErrNotConfigured", err)
	}
	// The bare version file of the first builds of this CLI.
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if !errors.Is(err, ErrOldInstall) {
		t.Fatalf("version file: got %v, want ErrOldInstall", err)
	}
	if !strings.Contains(err.Error(), "versola configure") {
		t.Errorf("the error should say what to run: %v", err)
	}
	// ComposeFile (status, down, uninstall) reports it, rather than "nothing
	// deployed", which would hide whatever that deployment still runs.
	if _, _, err := ComposeFile(); !errors.Is(err, ErrOldInstall) {
		t.Errorf("ComposeFile: got %v, want ErrOldInstall", err)
	}
}

func TestRequireCurrent(t *testing.T) {
	for name, c := range map[string]struct {
		st  State
		old bool
	}{
		"current":             {State{BundleDir: "bundle-1", ProxyMode: "external"}, false},
		"no bundle directory": {State{ProxyMode: "external"}, true},
		"no proxy mode":       {State{BundleDir: "bundle-1"}, true},
	} {
		err := c.st.RequireCurrent()
		if got := errors.Is(err, ErrOldInstall); got != c.old {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, _, err := (&State{}).ComposeFilePath(); !errors.Is(err, ErrOldInstall) {
		t.Errorf("ComposeFilePath without a bundle: got %v, want ErrOldInstall", err)
	}
}

func TestMarkStartingOldRecord(t *testing.T) {
	dir := activeDir(t)
	writeState(t, dir, `{"schemaVersion":1,"target":"local","version":"0.3.0","configuredAt":"2026-08-01T00:00:00Z"}`)
	if err := MarkStarting(); !errors.Is(err, ErrOldInstall) {
		t.Errorf("MarkStarting: got %v, want ErrOldInstall", err)
	}
	if err := MarkRunning(); !errors.Is(err, ErrOldInstall) {
		t.Errorf("MarkRunning: got %v, want ErrOldInstall", err)
	}
}

func TestMixedVersions(t *testing.T) {
	for name, c := range map[string]struct {
		st   State
		want []string // substrings; none means ""
	}{
		"implicit slot 1": {State{Version: "0.7.0"}, nil},
		"replicas at the deployment's version": {State{Version: "0.7.0", Slots: map[string][]Slot{
			"auth": {{N: 1, Version: "0.7.0"}, {N: 2, Version: "0.7.0"}}}}, nil},
		"a replica at another version": {State{Version: "0.7.0", Slots: map[string][]Slot{
			"auth": {{N: 1, Version: "0.6.3"}, {N: 2, Version: "0.7.0"}}}},
			[]string{"auth has slot 1 at 0.6.3", "at 0.7.0"}},
		"every replica at the old version (record moved on alone)": {State{Version: "0.7.0", Slots: map[string][]Slot{
			"edge": {{N: 1, Version: "0.6.3"}}}},
			[]string{"edge has slot 1 at 0.6.3"}},
		"both services": {State{Version: "0.7.0", Slots: map[string][]Slot{
			"auth": {{N: 1, Version: "0.6.3"}}, "edge": {{N: 2, Version: "0.6.3"}}}},
			[]string{"auth has slot 1", "edge has slot 2"}},
		"a stand-in slot": {State{Version: "0.7.0", Surge: map[string]int{"auth": 3}},
			[]string{"stand-in", "auth slot 3"}},
	} {
		got := c.st.MixedVersions()
		if len(c.want) == 0 {
			if got != "" {
				t.Errorf("%s: got %q, want none", name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", name, got, w)
			}
		}
	}
}

// configure over the production record: layout 3 is written, with the
// Postgres host it was given, and the mounted bundle is kept.
func TestFinalizeOverLayout1(t *testing.T) {
	dir := activeDir(t)
	writeState(t, dir, prodLayout1)
	oldBundle := filepath.Join(dir, "bundle-1759312800000000000-123")
	newBundle := filepath.Join(dir, "bundle-2")
	for _, b := range []string{oldBundle, newBundle} {
		if err := os.MkdirAll(b, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := Finalize(FinalizeOptions{Target: "vps", Version: "0.7.0", BundleDir: newBundle,
		AuthURL: "https://id.example.com", ProxyMode: "external", PostgresHost: "127.0.0.1:5432"}); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != 4 || s.PostgresHost != "127.0.0.1:5432" || s.Version != "0.7.0" {
		t.Errorf("got %+v", s)
	}
	if s.PreviousVersion != "" || s.Surge != nil || s.Slots != nil {
		t.Errorf("a configure starts with no upgrade under way: %+v", s)
	}
	if !reflect.DeepEqual(s.MountedBundleDirs, []string{filepath.Base(oldBundle)}) {
		t.Errorf("the running containers' bundle should stay recorded as mounted: %v", s.MountedBundleDirs)
	}
	if _, err := os.Stat(oldBundle); err != nil {
		t.Errorf("the bundle the running containers mount was removed: %v", err)
	}
}
