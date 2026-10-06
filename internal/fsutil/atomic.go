// Package fsutil holds small file helpers shared by more than one package.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes b to path through a temp file in the same
// directory and renames it into place, instead of os.WriteFile's
// truncate-then-write: whatever interrupts it (a full disk, a kill, a
// crash) leaves path as the old content or the new, never an empty or half
// written file. Used for files that something reads back later and must be
// able to parse -- the generated compose overlay, nginx's upstreams.
//
// On Windows the rename is MoveFileEx, not a single atomic transaction; the
// guarantee is firm on POSIX (see internal/openbao's atomicWriteFile for the
// longer discussion of the same trade-off).
func WriteFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("couldn't create a temp file next to %s: %w", path, err)
	}
	name := tmp.Name()
	// Nothing left to remove once the rename has succeeded.
	defer os.Remove(name)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't set permissions on %s: %w", name, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't write %s: %w", name, err)
	}
	// Data on disk before the rename points path at it.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't flush %s to disk: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("couldn't finish writing %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("couldn't move %s into place at %s: %w", name, path, err)
	}
	return nil
}
