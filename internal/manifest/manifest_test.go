package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const valid = `{
  "schemaVersion": 1,
  "version": "0.7.6",
  "rollingFrom": ["0.7.0", "0.7.5"],
  "freshDatabaseFrom": ["0.6.*"],
  "notes": "https://example.com/notes"
}`

func TestLoad(t *testing.T) {
	m, err := Load(write(t, valid))
	if err != nil || m == nil {
		t.Fatalf("valid: %v %v", m, err)
	}
	if m.Version != "0.7.6" || m.Notes != "https://example.com/notes" || len(m.RollingFrom) != 2 {
		t.Errorf("parsed wrong: %+v", m)
	}
}

func TestLoadAbsentIsNotAnError(t *testing.T) {
	m, err := Load(t.TempDir())
	if m != nil || err != nil {
		t.Errorf("absent: %v %v", m, err)
	}
}

func TestLoadRefusesWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"not json":       {`{`, "not valid JSON"},
		"other schema":   {`{"schemaVersion": 2}`, "schemaVersion 2"},
		"no schema":      {`{"rollingFrom": ["0.7.0"]}`, "schemaVersion 0"},
		"string version": {`{"schemaVersion": "1"}`, "not valid JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := Load(write(t, tc.body))
			if m != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, %v; want an error with %q", m, err, tc.want)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	m, _ := Load(write(t, valid))
	for from, want := range map[string]Verdict{
		"0.7.0":   Rolling,
		"0.7.5":   Rolling,
		"0.7.3":   Undeclared, // not listed: nothing is assumed
		"0.6.2":   FreshDatabase,
		"0.6.":    FreshDatabase,
		"0.60.1":  Undeclared, // "0.6.*" is a prefix of "0.6.", not of "0.60."
		"0.8.0":   Undeclared,
		"":        Undeclared,
		"0.7.0-x": Undeclared, // exact entries match exactly
	} {
		if got := m.Check(from); got != want {
			t.Errorf("Check(%q) = %v, want %v", from, got, want)
		}
	}
}

func TestCheckWithoutManifest(t *testing.T) {
	var m *Manifest
	if got := m.Check("0.6.2"); got != Undeclared {
		t.Errorf("a missing manifest declares nothing, got %v", got)
	}
}

func TestCheckOverlapRefuses(t *testing.T) {
	m := &Manifest{RollingFrom: []string{"0.7.*"}, FreshDatabaseFrom: []string{"0.7.1"}}
	if got := m.Check("0.7.1"); got != FreshDatabase {
		t.Errorf("a version in both lists must be refused, got %v", got)
	}
	if got := m.Check("0.7.2"); got != Rolling {
		t.Errorf("0.7.2: %v", got)
	}
}
