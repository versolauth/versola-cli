package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{
  "schemaVersion": 1,
  "releases": [
    {"version": "0.7.6", "rollingFrom": ["0.7.4", "0.7.5", "0.7.*"], "freshDatabaseFrom": ["0.6.*", "0.5.2"], "notes": "https://github.com/versolauth/versola/releases/tag/0.7.6"},
    {"version": "0.7.5", "rollingFrom": [], "freshDatabaseFrom": ["0.6.*"], "notes": "https://github.com/versolauth/versola/releases/tag/0.7.5"},
    {"version": "0.8.0", "rollingFrom": ["0.7.*"], "freshDatabaseFrom": [], "notes": "http://not-https.example/0.8.0"}
  ]
}`

func mustParse(t *testing.T, s string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheck(t *testing.T) {
	m := mustParse(t, sample)
	for _, c := range []struct {
		name, from, to string
		want           Verdict
		reason         string // substring of Reason when Unknown
	}{
		{"listed exactly", "0.7.5", "0.7.6", Rolling, ""},
		{"listed by glob", "0.7.1", "0.7.6", Rolling, ""},
		{"an earlier line is a fresh database", "0.6.2", "0.7.6", FreshDatabase, ""},
		{"a fresh database is listed exactly", "0.5.2", "0.7.6", FreshDatabase, ""},
		{"nothing says anything", "0.5.3", "0.7.6", Unknown, "does not state compatibility with 0.5.3"},
		{"no entry for the target", "0.7.5", "0.9.0", Unknown, "no entry for 0.9.0"},
		{"an empty rollingFrom is not a promise", "0.7.4", "0.7.5", Unknown, "does not state compatibility"},
		{"a downgrade is never rolling", "0.7.6", "0.7.5", Unknown, "only speaks about upgrades"},
		{"the same version", "0.7.6", "0.7.6", Unknown, "only speaks about upgrades"},
		{"a glob does not roll from a pre-release", "0.7.9-rc1", "0.8.0", Unknown, "does not state compatibility"},
		{"a fresh-database glob covers a pre-release", "0.6.3-rc1", "0.7.6", FreshDatabase, ""},
		{"not a release version", "dev", "0.7.6", Unknown, "not both release versions"},
	} {
		got := m.Check(c.from, c.to)
		if got.Verdict != c.want {
			t.Errorf("%s: %s -> %s = %s (%s), want %s", c.name, c.from, c.to, got.Verdict, got.Reason, c.want)
		}
		if c.want == Unknown && !strings.Contains(got.Reason, c.reason) {
			t.Errorf("%s: reason %q lacks %q", c.name, got.Reason, c.reason)
		}
		if c.want != Unknown && got.Reason != "" {
			t.Errorf("%s: a declared verdict carries the reason %q", c.name, got.Reason)
		}
	}
}

func TestCheckWithoutManifest(t *testing.T) {
	var m *Manifest
	got := m.Check("0.7.5", "0.7.6")
	if got.Verdict != Unknown || !strings.Contains(got.Reason, "does not publish a compatibility manifest") {
		t.Fatalf("got %+v", got)
	}
}

func TestFreshDatabaseWinsOverRolling(t *testing.T) {
	m := mustParse(t, `{"schemaVersion":1,"releases":[{"version":"0.7.6","rollingFrom":["0.6.*"],"freshDatabaseFrom":["0.6.*"],"notes":""}]}`)
	if got := m.Check("0.6.1", "0.7.6"); got.Verdict != FreshDatabase {
		t.Fatalf("got %s", got.Verdict)
	}
}

func TestNotesAreOnlyPlainHTTPSLinks(t *testing.T) {
	m := mustParse(t, sample)
	if got := m.Check("0.7.5", "0.7.6").Notes; got != "https://github.com/versolauth/versola/releases/tag/0.7.6" {
		t.Errorf("notes = %q", got)
	}
	if got := m.Check("0.7.5", "0.8.0").Notes; got != "" {
		t.Errorf("an http link was kept: %q", got)
	}
	for _, bad := range []string{"https://x/\x1b[31m", "https://x/ y", "https://x/é", "javascript:alert(1)", "https://" + strings.Repeat("a", 400)} {
		if safeLink(bad) != "" {
			t.Errorf("safeLink kept %q", bad)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"not json":       `{`,
		"no schema":      `{"releases":[]}`,
		"future schema":  `{"schemaVersion":2,"releases":[]}`,
		"schema is text": `{"schemaVersion":"1","releases":[]}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := Parse([]byte(`{"schemaVersion":2,"releases":[]}`))
	if err == nil || !strings.Contains(err.Error(), "schemaVersion 2") {
		t.Errorf("the error does not name the schema: %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if m, err := Load(dir); m != nil || err != nil {
		t.Fatalf("a missing file is (nil, nil): got %v, %v", m, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir)
	if err != nil || len(m.Releases) != 3 {
		t.Fatalf("got %v, %v", m, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"schemaVersion":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("an unknown schema was accepted")
	}
}

func TestReport(t *testing.T) {
	m := mustParse(t, sample)
	enc := func(r Result) string { b, _ := json.Marshal(r.Report()); return string(b) }
	if got := enc(m.Check("0.7.5", "0.7.6")); got != `{"from":"0.7.5","rolling":true,"freshDatabase":false,"notes":"https://github.com/versolauth/versola/releases/tag/0.7.6"}` {
		t.Errorf("rolling: %s", got)
	}
	if got := enc(m.Check("0.6.2", "0.7.6")); !strings.Contains(got, `"rolling":false,"freshDatabase":true`) {
		t.Errorf("fresh: %s", got)
	}
	if got := enc(m.Check("0.5.3", "0.7.6")); !strings.Contains(got, `"rolling":null,"freshDatabase":false`) {
		t.Errorf("unknown: %s", got)
	}
}

func TestLoadRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := `{"schemaVersion":1,"releases":[],"pad":"` + strings.Repeat("x", maxSize) + `"}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRefusesWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	// A directory in the manifest's place needs no privileges to make.
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a directory: got %v", err)
	}

	// A symlink to a valid manifest is refused too (skipped where symlinks
	// need a privilege the test does not have).
	link := t.TempDir()
	target := filepath.Join(link, "real.json")
	if err := os.WriteFile(target, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(link, FileName)); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a symlink: got %v", err)
	}
}
