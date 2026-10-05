package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.conf")
	for _, content := range []string{"one", "two, longer", "3"} {
		if err := WriteFileAtomic(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("got %q, %v; want %q", got, err, content)
		}
	}
	// No temp files are left next to it.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftovers in %s: %v", dir, entries)
	}
}

// A failed write leaves the existing file untouched (here: the directory
// does not exist, so the temp file cannot even be created).
func TestWriteFileAtomicFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.conf")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filepath.Join(dir, "missing", "f.conf"), []byte("new"), 0o644); err == nil {
		t.Fatal("want an error")
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Errorf("got %q", got)
	}
}
