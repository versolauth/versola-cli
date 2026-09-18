package openbao

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// atomicWriteFile writes b to path via a temp file in path's own
// directory, then renames it into place, instead of os.WriteFile's own
// open(O_TRUNC)-then-write.
//
// That distinction matters for both of this package's credential files
// (SaveLocalAdmin's root token/unseal key, SaveCredentials' AppRole
// SecretID): os.WriteFile truncates the file the moment it opens it, and
// only writes the new bytes after that -- so a process that's killed
// partway through (Ctrl+C, an OOM kill, a crash, a lost VM) can leave
// path empty or partially written, having destroyed whatever valid
// credentials were there before without ever finishing the new ones
// either. None of those interruptions is a write error os.WriteFile's
// own caller ever sees, so a check-the-returned-error fallback (see
// SaveLocalAdmin's console fallback, from an earlier review round) can't
// catch this case at all -- it only helps when WriteFile itself reports
// failure.
//
// Writing the new content to a temp file first and renaming over path
// closes that gap: os.Rename is atomic on the same filesystem (which the
// temp file's directory, matching path's own, guarantees), so every
// point this can be interrupted still leaves path as either the old
// content or the fully-written new content -- never something in
// between. True on Windows too, not just POSIX: Go's os.Rename there
// uses MoveFileEx with MOVEFILE_REPLACE_EXISTING, which gives the same
// guarantee.
func atomicWriteFile(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("couldn't create a temp file next to %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	// Only ever cleans up a leftover on a failure path below -- once
	// os.Rename succeeds there's nothing left at tmpPath to remove, so
	// this Remove failing at that point (file already gone) is the
	// expected, harmless case, not something worth surfacing.
	defer os.Remove(tmpPath)

	// os.CreateTemp always creates the file at 0600 regardless of what's
	// asked for -- explicit here anyway rather than relying on that,
	// since it's this function's own contract (perm is a parameter,
	// callers shouldn't have to know CreateTemp's default happens to
	// already match) and cheap to make certain of.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't set permissions on %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't write %s: %w", tmpPath, err)
	}
	// Flush the temp file's data to disk before it's ever renamed into
	// place. Close alone only hands the bytes to the OS's page cache --
	// without this, a host crash or lost VM right after Rename below
	// returns can still lose the new content the rename just pointed
	// path at, even though nothing here ever saw an error (flagged in
	// PR review: the atomicity fix above protects against a partial
	// write, not against this, a separate durability gap).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't flush %s to disk: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("couldn't finish writing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("couldn't move %s into place at %s: %w", tmpPath, path, err)
	}
	// Best-effort: also flush dir's own metadata, so the rename itself
	// -- not just the data inside the file it now points at -- survives
	// a crash right after this returns. A rename only becomes durable
	// once the *directory's* metadata update reaches disk, not merely
	// once Rename returns; a crash in that narrower gap can, on some
	// filesystems, leave the directory entry still pointing at the old
	// (or no) file even though Rename above already succeeded from this
	// process's own point of view.
	//
	// POSIX-only, and not fatal: directory fsync has no well-established
	// equivalent through Go's os package on Windows (same reasoning as
	// ensureCredentialsDir's own Chmod-is-best-effort comment), and
	// docker-local -- the only target Windows ever runs -- is a
	// throwaway dev stack this narrower gap matters least for; vps, the
	// target this protects in earnest, is always Linux. Skipped rather
	// than attempted-and-warned-every-call on a platform where it isn't
	// meaningful to begin with.
	if runtime.GOOS != "windows" {
		if err := syncDir(dir); err != nil {
			fmt.Printf("  (couldn't flush %s's directory entry to disk: %v)\n", path, err)
		}
	}
	return nil
}

// syncDir flushes dir's own metadata to disk, so a rename into dir
// (os.Rename in atomicWriteFile above) is durable across a crash, not
// just visible to this process from the moment Rename returns. See
// atomicWriteFile's own comment for why this matters and why it's only
// attempted, best-effort, on POSIX.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
