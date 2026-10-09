package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveSecretsRevision(t *testing.T) {
	for _, c := range []struct{ recorded, want int }{{0, 1}, {1, 1}, {2, 2}, {7, 7}, {-3, 1}} {
		if got := (&State{SecretsRevision: c.recorded}).EffectiveSecretsRevision(); got != c.want {
			t.Errorf("recorded %d: got %d, want %d", c.recorded, got, c.want)
		}
	}
}

func TestSecretsRevisionIsReadFromStateJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	dir := filepath.Join(home, ".versola", "active")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a recorded revision", func(t *testing.T) {
		write(`{"schemaVersion":1,"target":"vps","version":"0.6.3","configuredAt":"2026-10-09T00:00:00Z","secretsRevision":3}`)
		s, err := Load()
		if err != nil || s.SecretsRevision != 3 || s.EffectiveSecretsRevision() != 3 {
			t.Fatalf("%+v %v", s, err)
		}
	})
	t.Run("a state written before the field existed is revision 1", func(t *testing.T) {
		write(`{"schemaVersion":1,"target":"vps","version":"0.6.2","configuredAt":"2026-09-27T00:00:00Z"}`)
		s, err := Load()
		if err != nil || s.SecretsRevision != 0 || s.EffectiveSecretsRevision() != 1 {
			t.Fatalf("%+v %v", s, err)
		}
	})
	t.Run("a legacy version-file deployment is revision 1 too", func(t *testing.T) {
		if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "version"), []byte("0.1.0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Load()
		if err != nil || s.EffectiveSecretsRevision() != 1 {
			t.Fatalf("%+v %v", s, err)
		}
	})
}

// omitempty keeps the file as it was for a deployment that has no revision
// recorded: nothing about state.json changes until something writes the field.
func TestSecretsRevisionIsOmittedWhenNotRecorded(t *testing.T) {
	b, err := json.Marshal(&State{SchemaVersion: SchemaVersion, Target: "vps", Version: "0.6.3"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secretsRevision") {
		t.Errorf("an unset revision must not be written: %s", b)
	}
	b, _ = json.Marshal(&State{SecretsRevision: 2})
	if !strings.Contains(string(b), `"secretsRevision":2`) {
		t.Errorf("a set revision is written: %s", b)
	}
}
