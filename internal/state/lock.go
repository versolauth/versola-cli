package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockFileName is the advisory lock every state-mutating command (configure,
// migrate, up, bootstrap) holds for its ENTIRE run -- see Lock's own comment
// for why "read state, decide, act" needs one at all.
const lockFileName = "active.lock"

// lockStaleAfter is how old an unattended lock file has to be before Lock
// treats it as abandoned rather than genuinely held -- see lockIsStale's own
// comment on why age, not the recorded PID, is what decides that.
const lockStaleAfter = 10 * time.Minute

// lockWait is how long Lock retries before giving up and reporting the
// lock as busy, rather than blocking forever. Long enough that a `versola
// migrate` run from a second terminal doesn't fail on a lock a first one is
// about to release in a second or two; short enough that a command doesn't
// look hung with no explanation for minutes on end.
const lockWait = 30 * time.Second

// Lock acquires the exclusive advisory lock on this machine's active
// deployment, blocking (with a bounded wait -- see lockWait, not forever)
// until it's free. The returned func releases it; callers `defer` it
// immediately.
//
// Every command that reads state to decide something and then later acts on
// what it read needs this, and re-reading state right before acting (see
// cmd/confirm.go's own comment, deploy.Migrate's recordMigrated) only ever
// narrows that window, it doesn't close it: there's always one more
// read-then-write pair further down the same sequence, and the last one is
// exactly as racy as the first, just harder to actually hit (flagged in
// review three times over, at three different points in the same
// configure/migrate/up sequence). Holding one lock for the WHOLE sequence
// -- from before the first read of state to after the last write to it --
// is the only way to actually close it: no other `versola` command can
// observe or change state.json while this one is deciding based on it.
//
// A plain exclusive-create lock file, not flock/LockFileEx: this only ever
// needs to keep separate `versola` PROCESSES on the same machine from
// interleaving against the SAME ~/.versola/active (this CLI never runs two
// configure calls concurrently against the same deployment BY DESIGN -- see
// state.Prepare's own comment -- the problem here is a caller that does
// that anyway, not a supported use case this needs to make fast or
// graceful about), and O_EXCL's atomicity already gives the same guarantee
// flock would, without a platform-specific implementation or a new
// dependency.
func Lock() (unlock func(), err error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("couldn't create %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockFileName)

	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			// Best-effort diagnostic for a human reading the lock file by
			// hand ("who's PID 12345, is it still running?") -- not read
			// back by lockIsStale (see its own comment on why age, not
			// this, is what actually decides staleness), so a failure to
			// write it isn't worth failing the lock acquisition over.
			fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("couldn't create lock file %s: %w", path, err)
		}

		if stale, staleErr := lockIsStale(path); staleErr == nil && stale {
			// The command that held this lock is gone (crashed, killed,
			// a lost SSH session mid-run) without ever reaching its own
			// deferred cleanup -- safe to steal: nothing is actually
			// racing against state.json right now. Removing and
			// immediately retrying the O_EXCL create (rather than just
			// falling through and assuming this iteration now owns it) is
			// what still keeps this exclusive if two commands both hit a
			// stale lock at the same moment -- only one of their retries
			// wins the create that follows.
			_ = os.Remove(path)
			continue
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another versola command is still running against this deployment (lock file: %s) -- wait for it to finish, or remove the lock file by hand if you're sure nothing is actually running", path)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// lockIsStale reports whether the lock file is old enough that whatever
// held it is presumed gone.
//
// Age, not the PID recorded inside it, is what decides this: there's no
// portable, dependency-free way from here to ask "is PID N still running
// AND still the same process that wrote this lock" that behaves the same
// on Windows and Linux/macOS (PID reuse aside, Go's own os.Process.Signal
// only supports os.Kill on Windows, not the harmless liveness probe POSIX
// allows via signal 0). A real configure/migrate/up finishes in well under
// lockStaleAfter even against a slow VPS -- something genuinely wedged for
// that long (a hung Docker daemon, most plausibly) is itself worth
// noticing on its own terms, not silently unblocked here.
func lockIsStale(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Raced with the holder's own cleanup -- it finished and
			// removed the lock between the caller seeing os.ErrExist and
			// this Stat. Not stale, just already gone; the caller's next
			// O_EXCL attempt succeeds on its own.
			return false, nil
		}
		return false, err
	}
	return time.Since(info.ModTime()) > lockStaleAfter, nil
}
