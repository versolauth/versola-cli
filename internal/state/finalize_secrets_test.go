package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func finalizeRev(t *testing.T, target string, rev int) {
	t.Helper()
	b, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := FinalizeWithRevision(target, "1.0.0", b, "https://id.example.com", "nginx", rev); err != nil {
		t.Fatal(err)
	}
}

func loadRev(t *testing.T) int {
	t.Helper()
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return s.SecretsRevision
}

func TestFinalizeRecordsTheSecretsRevision(t *testing.T) {
	isolate(t)
	finalizeRev(t, "vps", 2)
	if got := loadRev(t); got != 2 {
		t.Fatalf("recorded %d", got)
	}

	t.Run("an older release does not lower it", func(t *testing.T) {
		finalizeRev(t, "vps", 1)
		if got := loadRev(t); got != 2 {
			t.Errorf("recorded %d, want it kept at 2", got)
		}
	})
	t.Run("a newer one raises it", func(t *testing.T) {
		finalizeRev(t, "vps", 3)
		if got := loadRev(t); got != 3 {
			t.Errorf("recorded %d", got)
		}
	})
	t.Run("a release without a schema keeps what the record had", func(t *testing.T) {
		finalizeRev(t, "vps", 0)
		if got := loadRev(t); got != 3 {
			t.Errorf("recorded %d, want 3 kept", got)
		}
	})
	t.Run("the revision of another target is not carried over", func(t *testing.T) {
		finalizeRev(t, "local", 1)
		if got := loadRev(t); got != 1 {
			t.Errorf("recorded %d", got)
		}
	})
	t.Run("plain Finalize leaves a first record without a revision, which reads as 1", func(t *testing.T) {
		isolate(t)
		b, err := Prepare()
		if err != nil {
			t.Fatal(err)
		}
		if err := Finalize("vps", "1.0.0", b, "https://id.example.com", "nginx"); err != nil {
			t.Fatal(err)
		}
		s, err := Load()
		if err != nil || s.SecretsRevision != 0 || s.EffectiveSecretsRevision() != 1 {
			t.Fatalf("%+v %v", s, err)
		}
	})
}

func TestMarkStartingAndRunningKeepTheRevision(t *testing.T) {
	isolate(t)
	finalizeRev(t, "vps", 4)
	if err := MarkStarting(); err != nil {
		t.Fatal(err)
	}
	if got := loadRev(t); got != 4 {
		t.Errorf("after MarkStarting: %d", got)
	}
	if err := MarkRunning(); err != nil {
		t.Fatal(err)
	}
	if got := loadRev(t); got != 4 {
		t.Errorf("after MarkRunning: %d", got)
	}
}

func TestPendingNote(t *testing.T) {
	dir := isolate(t)

	t.Run("none is nil and not an error", func(t *testing.T) {
		p, err := LoadPending()
		if p != nil || err != nil {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("a saved note reads back, 0600, and its directory is created", func(t *testing.T) {
		if err := SavePending(Pending{Target: "vps", SecretsRevision: 2}); err != nil {
			t.Fatal(err)
		}
		p, err := LoadPending()
		if err != nil || p == nil || p.Target != "vps" || p.SecretsRevision != 2 {
			t.Fatalf("%+v %v", p, err)
		}
		if info, err := os.Stat(filepath.Join(dir, pendingFileName)); err != nil || (info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
			t.Errorf("%v %v", info, err)
		}
		if _, err := os.Stat(filepath.Join(dir, pendingFileName+".tmp")); !os.IsNotExist(err) {
			t.Error("the temp file was left behind")
		}
	})
	t.Run("a broken note is an error, never read as none", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, pendingFileName), []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPending(); err == nil || !strings.Contains(err.Error(), "delete it") {
			t.Fatalf("%v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, pendingFileName), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPending(); err == nil {
			t.Fatal("a note with no target is not a note")
		}
	})
	t.Run("clearing removes it, and clearing nothing is fine", func(t *testing.T) {
		if err := ClearPending(); err != nil {
			t.Fatal(err)
		}
		if p, err := LoadPending(); p != nil || err != nil {
			t.Fatalf("%+v %v", p, err)
		}
		if err := ClearPending(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFinalizeClearsOnlyItsOwnTargetsNote(t *testing.T) {
	isolate(t)
	if err := SavePending(Pending{Target: "vps"}); err != nil {
		t.Fatal(err)
	}
	finalizeRev(t, "local", 1)
	if p, _ := LoadPending(); p == nil {
		t.Fatal("configuring local removed the note of an unfinished vps install")
	}
	finalizeRev(t, "vps", 1)
	if p, _ := LoadPending(); p != nil {
		t.Fatal("configuring vps left its own note")
	}
}

func TestFinalizeClearsTheUnfinishedInstallNote(t *testing.T) {
	isolate(t)
	if err := SavePending(Pending{Target: "vps", SecretsRevision: 1}); err != nil {
		t.Fatal(err)
	}
	finalizeRev(t, "vps", 1)
	if p, err := LoadPending(); p != nil || err != nil {
		t.Fatalf("the note outlived the deployment record: %+v %v", p, err)
	}
	// And a failed configure, which never gets to Finalize, leaves it.
	if err := SavePending(Pending{Target: "vps", SecretsRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(); err != nil {
		t.Fatal(err)
	}
	if p, _ := LoadPending(); p == nil {
		t.Fatal("Prepare must not touch the note")
	}
}

// uninstall removes Dir() whole; the note has to be inside it so that it goes too.
func TestPendingNoteLivesInTheActiveDirectory(t *testing.T) {
	dir := isolate(t)
	if err := SavePending(Pending{Target: "vps"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if p, err := LoadPending(); p != nil || err != nil {
		t.Fatalf("%+v %v", p, err)
	}
}
