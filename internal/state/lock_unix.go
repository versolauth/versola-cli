//go:build !windows

package state

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile attempts to acquire an exclusive flock on f without
// blocking, returning errLockBusy (rather than blocking, per Lock's own
// bounded-wait loop) if another process already holds it.
//
// flock, not fcntl byte-range locking: it's associated with the open file
// description, so it's released automatically when every fd referring to
// it is closed -- including as a side effect of the holding process
// exiting for any reason -- with nothing in this package that has to run
// first for that to happen. See Lock's own comment for why that property
// is the entire reason this file exists.
func tryLockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLockBusy
	}
	return err
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
