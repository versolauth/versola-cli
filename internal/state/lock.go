package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockFileName is the advisory lock every command that reads then acts on
// ~/.versola/active holds for its ENTIRE run -- see Lock's own comment for
// why "read state, decide, act" needs one at all.
const lockFileName = "active.lock"

// lockWait is how long Lock retries a busy lock before giving up and
// reporting it as such, rather than blocking forever. Long enough that a
// `versola migrate` run from a second terminal doesn't fail on a lock a
// first one is about to release in a second or two; short enough that a
// command doesn't look hung with no explanation for minutes on end.
const lockWait = 30 * time.Second

// errLockBusy is tryLockFile's sentinel for "someone else holds this right
// now" (as opposed to a real I/O error acquiring it) -- see
// lock_unix.go/lock_windows.go.
var errLockBusy = errors.New("lock busy")

// Lock acquires the exclusive advisory lock on this machine's active
// deployment, blocking (with a bounded wait -- see lockWait, not forever)
// until it's free. The returned func releases it; callers `defer` it
// immediately.
//
// Every command that reads state to decide something and then later acts on
// what it read needs this: configure (locally overwrites vs. asks for a vps
// confirmation depending on what's THERE right now), migrate/up (asks once,
// then acts on the same record), bootstrap (all of the above, back to
// back), and down/uninstall (read ~/.versola/active, then stop containers
// or delete it wholesale). Re-reading state right before acting only ever
// narrows the window between the decision and the act, it never closes it
// -- there's always one more read-then-write pair further down the same
// sequence, and the last one is exactly as racy as the first, just harder
// to actually hit (flagged in review, repeatedly, at different points in
// the same sequence). Holding one lock for the WHOLE sequence -- from
// before the first read of state to after the last write to or removal of
// it -- is the only way to actually close it: no other `versola` command
// can observe or change ~/.versola/active while this one is deciding based
// on it.
//
// Backed by a real OS advisory lock (flock on Unix, LockFileEx on Windows
// -- see tryLockFile/unlockFile in lock_unix.go/lock_windows.go), not a
// lock file plus a hand-rolled staleness heuristic. An OS-level lock is
// tied to a live file descriptor inside the holding process, and the
// kernel releases it the instant that process exits for ANY reason -- a
// clean return, a panic, a crash, `kill -9` -- with no Go cleanup code
// that has to run first for that to happen. A staleness-based version of
// this instead needs a heartbeat to tell an idle-but-alive holder (a
// confirmation prompt left open, a slow migration) from a genuinely
// crashed one, plus a way to make "steal the stale lock" and "a stalled
// holder's own heartbeat still touching it" mutually exclusive -- and an
// earlier version of this file went through two separate rounds of real,
// high-severity bugs in exactly that gap (a stolen lock's fresh holder
// getting its file deleted or re-refreshed out from under it by the
// process that used to hold it) before landing here. None of that class of
// bug is possible against an OS lock: at most one process can hold it at
// any instant, enforced by the kernel, not by anything this package has to
// get right about timing.
func Lock() (unlock func(), err error) {
	path, err := lockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("couldn't create %s: %w", filepath.Dir(path), err)
	}

	// O_RDWR, not O_WRONLY: LockFileEx on Windows requires the handle to
	// have been opened for both reading and writing regardless of which
	// way the lock itself goes.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("couldn't open lock file %s: %w", path, err)
	}

	deadline := time.Now().Add(lockWait)
	for {
		lockErr := tryLockFile(f)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, errLockBusy) {
			_ = f.Close()
			return nil, fmt.Errorf("couldn't lock %s: %w", path, lockErr)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another versola command is still running against this deployment (lock file: %s) -- wait for it to finish", path)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Best-effort and purely informational -- nothing in this package ever
	// reads this back, and correctness never depends on it. The OS lock
	// acquired above is what actually enforces exclusivity; this just
	// leaves a note for a human who opens the file while a command is
	// stuck, so "another versola command is still running" isn't the only
	// clue about which one.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "pid %d, locked at %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))

	unlock = func() {
		_ = unlockFile(f)
		_ = f.Close()
	}
	return unlock, nil
}

// lockPath is ~/.versola/active.lock -- a SIBLING of Dir() (~/.versola/
// active), not a file inside it. Deliberately: `versola uninstall` removes
// Dir() wholesale (os.RemoveAll -- see cmd/uninstall.go) as part of what
// this lock has to protect against running concurrently with anything
// else, and uninstall itself needs to hold this lock while it does that. A
// lock file living inside the very directory being deleted would either
// have to be special-cased out of that removal, or be removed as a side
// effect of it while this function's own unlock still thinks it holds it
// -- keeping it outside Dir() entirely avoids both. It doesn't need to be
// removed on unlock either way: unlike the old staleness-based design, an
// empty lock file just sitting on disk with nobody holding its OS lock is
// inert, not a hazard the next Lock() has to reason about.
func lockPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), lockFileName), nil
}
