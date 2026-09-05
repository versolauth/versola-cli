package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lockFileName is the advisory lock every command that reads then acts on
// ~/.versola/active holds for its ENTIRE run -- see Lock's own comment for
// why "read state, decide, act" needs one at all.
const lockFileName = "active.lock"

// lockStaleAfter is how long a lock can go untouched before Lock treats it
// as abandoned rather than genuinely held. The holder refreshes it every
// lockHeartbeat for as long as it's alive (see Lock's own comment), so
// this only ever has to outlast a couple of missed heartbeats -- not the
// longest real operation this CLI might run, the mistake an earlier
// version of this made (flagged in review: a flat 10-minute age check
// treated a confirmation prompt left open, or a slow migration against a
// large database, the same as an abandoned lock).
const lockStaleAfter = 30 * time.Second

// lockHeartbeat is how often a held lock's mtime is refreshed.
const lockHeartbeat = 10 * time.Second

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
// what it read needs this: configure (locally overwrites vs. asks for a vps
// confirmation depending on what's THERE right now), migrate/up (asks once,
// then acts on the same record), bootstrap (all of the above, back to
// back), and down/uninstall (read ~/.versola/active, then stop containers
// or delete it wholesale). Re-reading state right before acting only ever
// narrows the window between the decision and the act, it never closes it
// -- there's always one more read-then-write pair further down the same
// sequence, and the last one is exactly as racy as the first, just harder
// to actually hit (flagged in review, repeatedly, at different points in
// the same sequence, and again for down/uninstall not holding this at
// all). Holding one lock for the WHOLE sequence -- from before the first
// read of state to after the last write to or removal of it -- is the only
// way to actually close it: no other `versola` command can observe or
// change ~/.versola/active while this one is deciding based on it.
//
// A plain exclusive-create lock file, not flock/LockFileEx: this only ever
// needs to keep separate `versola` PROCESSES on the same machine from
// interleaving against the same ~/.versola/active (this CLI never runs two
// configure calls concurrently against the same deployment BY DESIGN -- see
// state.Prepare's own comment -- the problem here is a caller that does
// that anyway, not a supported use case this needs to make fast or
// graceful about), and O_EXCL's atomicity already gives the same guarantee
// flock would, without a platform-specific implementation or a new
// dependency.
func Lock() (unlock func(), err error) {
	path, err := lockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("couldn't create %s: %w", filepath.Dir(path), err)
	}

	// Unique to this specific acquisition (PID alone isn't: a PID can be
	// reused, and more to the point here, this same process could steal a
	// stale lock and re-acquire within its own lifetime) -- written into
	// the file and checked again on release, so this only ever removes the
	// lock IT created, never a different one that's since taken its place
	// (see unlock below).
	token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	deadline := time.Now().Add(lockWait)
	for {
		f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if createErr == nil {
			fmt.Fprintln(f, token)
			_ = f.Close()
			break
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("couldn't create lock file %s: %w", path, createErr)
		}

		if stale, staleErr := lockIsStale(path); staleErr == nil && stale {
			// The command that held this lock is gone (crashed, killed, a
			// lost SSH session mid-run) without ever reaching its own
			// deferred cleanup, and hasn't refreshed it in over
			// lockStaleAfter -- safe to steal: nothing is actually racing
			// against ~/.versola/active right now.
			//
			// os.Rename to claim it, not os.Remove: if two waiters both
			// observe the same stale lock at the same moment, a rename of
			// that exact path only ever succeeds for one of them -- the
			// loser's Rename fails outright because the source is already
			// gone. Two unconditional os.Remove calls don't have that
			// property (both just report success): the loser could remove
			// the file again after the winner's very next loop iteration
			// has already recreated it, deleting a brand-new, legitimately
			// held lock instead of the stale one it thought it was cleaning
			// up (flagged in review -- exactly this interleaving is what
			// let two commands both believe they held the lock at once).
			// The claimed file itself is discarded immediately after --
			// nothing in it is needed once ownership of the removal is
			// settled, only the exclusivity of the rename mattered.
			claimed := path + ".stale-" + token
			if err := os.Rename(path, claimed); err != nil {
				// Someone else claimed it first, or the original holder's
				// own unlock ran and removed it first -- either way, not
				// this iteration's turn; fall through to the normal
				// wait-and-retry below instead of assuming ownership.
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("another versola command is still running against this deployment (lock file: %s) -- wait for it to finish, or remove the lock file by hand if you're sure nothing is actually running", path)
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			_ = os.Remove(claimed)
			continue
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another versola command is still running against this deployment (lock file: %s) -- wait for it to finish, or remove the lock file by hand if you're sure nothing is actually running", path)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Refreshes the lock's mtime for as long as this process holds it, so
	// lockIsStale tracks "is the holder still alive", not "has it been
	// running a while" -- a confirmation prompt left open at a y/N, or a
	// migration against a large production database, can both legitimately
	// run past any fixed timeout short enough to also reclaim a genuinely
	// crashed holder promptly (flagged in review).
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(lockHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				now := time.Now()
				_ = os.Chtimes(path, now, now)
			}
		}
	}()

	unlock = func() {
		close(done)
		// Only removed if it's still the exact lock this call created --
		// an earlier version removed unconditionally by path, which meant
		// a lock this process's own heartbeat failed to keep alive for
		// some reason (system sleep, a long GC pause, a clock jump) could
		// already have been stolen by a different `versola` invocation by
		// the time this runs, and this would then delete THAT invocation's
		// own live lock out from under it -- two commands interleaving
		// against the same state with neither aware of the other (flagged
		// in review).
		b, readErr := os.ReadFile(path)
		if readErr == nil && strings.TrimSpace(string(b)) == token {
			_ = os.Remove(path)
		}
	}
	return unlock, nil
}

// lockPath is ~/.versola/active.lock -- a SIBLING of Dir() (~/.versola/
// active), not a file inside it. Deliberately: `versola uninstall` removes
// Dir() wholesale (os.RemoveAll -- see cmd/uninstall.go) as part of what
// this lock has to protect against running concurrently with anything
// else, and uninstall itself needs to hold this lock while it does that.
// A lock file living inside the very directory being deleted would either
// have to be special-cased out of that removal, or be removed as a side
// effect of it while still logically "held" until this function's own
// unlock runs -- keeping it outside Dir() entirely avoids both.
func lockPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), lockFileName), nil
}

// lockIsStale reports whether the lock file has gone unrefreshed for
// longer than lockStaleAfter -- see the comment on that constant, and on
// Lock's own heartbeat, for why age since the last refresh (not age since
// creation, and not the PID recorded inside it) is what decides this.
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
