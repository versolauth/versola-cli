//go:build windows

package state

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockRangeBytes is how much of the file LockFileEx locks. The file's
// actual content (see Lock's informational pid/timestamp write) is a few
// bytes and never grows, but Windows locks a byte RANGE, not "the whole
// file" as a concept -- an arbitrarily large range comfortably covers
// anything this file will ever contain.
const lockRangeBytes = 1 << 30

// tryLockFile attempts to acquire an exclusive lock on f without blocking
// (LOCKFILE_FAIL_IMMEDIATELY), returning errLockBusy (rather than blocking,
// per Lock's own bounded-wait loop) if another process already holds it.
//
// LockFileEx is Windows' equivalent of Unix flock for this purpose: the
// lock is released automatically when the handle is closed, including as
// a side effect of the holding process exiting for any reason -- see
// lock_unix.go's tryLockFile and Lock's own comment for why that's the
// property this whole file exists for.
func tryLockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockRangeBytes,
		0,
		ol,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockBusy
	}
	return err
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockRangeBytes, 0, ol)
}
