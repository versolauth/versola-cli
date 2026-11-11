package deploy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/state"
)

// A Ctrl-C that comes while Postgres is started for the restore, before the
// database is touched, ends the command with the database as it was. (The
// signals are held while Postgres is started: without this the command went on
// to replace the database after the customer had pressed it.)
func TestRestoreDatabaseInterruptedBeforeTheRestoreTouchesNothing(t *testing.T) {
	archive, f := databaseFixture(t)
	ops := f.ops()
	ops.tableCount = func(string, pgTarget) (int, error) {
		f.pg.sigs <- os.Interrupt
		time.Sleep(300 * time.Millisecond) // the goroutine that holds the signals notes it
		return 0, nil
	}
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, ops)
	if code := exitcode.Code(err); code != exitcode.Interrupted || !strings.Contains(err.Error(), "not touched") {
		t.Fatalf("exit code %d: %v", code, err)
	}
	if f.dump != "" {
		t.Errorf("the database was restored after an interrupt")
	}
	if got := strings.Join(f.pg.calls, ","); got != "start,stop" {
		t.Errorf("Postgres calls: %s", got)
	}
}

// A Ctrl-C that comes during a restore that then commits is reported as what it
// is: the database WAS replaced. It used to say the database was as it was.
func TestRestoreDatabaseInterruptedAfterTheCommitSaysSo(t *testing.T) {
	archive, f := databaseFixture(t)
	ops := f.ops()
	ops.pgRestore = func(_ string, _ pgTarget, r io.Reader) error {
		b, _ := io.ReadAll(r)
		f.dump = string(b)
		f.pg.sigs <- os.Interrupt
		time.Sleep(300 * time.Millisecond)
		return nil
	}
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, ops)
	if code := exitcode.Code(err); code != exitcode.Interrupted {
		t.Fatalf("exit code %d: %v", code, err)
	}
	if !strings.Contains(err.Error(), "WAS replaced") || strings.Contains(err.Error(), "as it was") || strings.Contains(err.Error(), "not touched") {
		t.Errorf("the message misstates what happened: %v", err)
	}
	if f.dump == "" {
		t.Errorf("the restore did not run")
	}
}

// A question that is waiting when the signal comes is given up on: the
// customer who pressed Ctrl-C does not have to answer it.
func TestAskInterruptiblyGivesUpOnASignal(t *testing.T) {
	heard := make(chan struct{})
	block := make(chan struct{})
	defer close(block)
	go func() { time.Sleep(50 * time.Millisecond); close(heard) }()
	err := askInterruptibly(pgSession{Heard: func() bool { return true }, HeardCh: heard}, func() error { <-block; return nil })
	if exitcode.Code(err) != exitcode.Interrupted {
		t.Errorf("%v", err)
	}
	// Without held signals the question is just asked.
	called := false
	if err := askInterruptibly(pgSession{Heard: func() bool { return false }}, func() error { called = true; return nil }); err != nil || !called {
		t.Errorf("%v (called %v)", err, called)
	}
}

// The reason shown for a failed restore is the error, not a NOTICE that came
// before it.
func TestFirstStderrLinePrefersTheError(t *testing.T) {
	got := firstStderrLine("NOTICE:  drop cascades to 2 other objects\npsql:<stdin>:57: ERROR:  role \"ghost\" does not exist\n")
	if !strings.Contains(got, "ERROR:  role") || strings.Contains(got, "NOTICE") {
		t.Errorf("= %q", got)
	}
	if got := firstStderrLine("pg_restore: error: could not read from input file\n"); !strings.Contains(got, "could not read") {
		t.Errorf("= %q", got)
	}
	if got := firstStderrLine("only a note\n"); got != ": only a note" {
		t.Errorf("= %q", got)
	}
	if firstStderrLine("") != "" || strings.ContainsAny(firstStderrLine("ERROR \x1b[2J"), "\x1b") {
		t.Errorf("empty or escapes")
	}
}

// When the machine has no admin file to compare, the restore says it could not
// tell, instead of passing silently.
func TestRestoreDatabaseSaysWhenItCouldNotCompareKeys(t *testing.T) {
	archive, f := databaseFixture(t)
	if err := os.Remove(filepath.Join(f.baoDir, "vps-admin.json")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, &out, f.ops()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "couldn't compare this machine's OpenBao keys") {
		t.Errorf("no note:\n%s", out.String())
	}
}

// A volume that appears between the check and its creation is not taken for one
// this run made, so a later failure never removes it.
func TestRestoreIdentityLeavesAVolumeThatAppeared(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	ops := f.ops()
	calls := 0
	ops.exists = func(kind, name string) (bool, error) {
		if kind == "volume" && name == "versola-openbao-file-vps" {
			calls++
			return calls > 1, nil // absent at the check, there at the creation
		}
		return false, nil
	}
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, ops)
	if err == nil || !strings.Contains(err.Error(), "appeared while this was asking") {
		t.Fatalf("%v", err)
	}
	if len(f.removed) != 0 {
		t.Errorf("a volume this run did not make was removed: %v", f.removed)
	}
	if _, err := os.Stat(filepath.Join(baoDir, "vps.json")); err == nil {
		t.Errorf("the credentials this run wrote are left behind")
	}
}

// A record in the archive from a newer layout is refused: the command printed
// from it would silently lack what it holds.
func TestRestoreIdentityRefusesANewerRecord(t *testing.T) {
	_, baoDir := restoreArchive(t)
	home := filepath.Dir(filepath.Dir(baoDir))
	p := filepath.Join(home, "newer.tar.gz")
	files := vpsFiles()
	files[4].data = `{"schemaVersion":99,"target":"vps","version":"0.7.6","authUrl":"https://id.example.com"}`
	buildArchive(t, p, BackupManifest{}, files)
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	err := restoreBackup(RestoreOptions{Archive: p, AssumeYes: true}, io.Discard, f.ops())
	if err == nil || !strings.Contains(err.Error(), "newer versola") {
		t.Fatalf("%v", err)
	}
	if len(f.filled) != 0 {
		t.Errorf("something was created before the refusal: %v", f.filled)
	}
}

// An unfinished first install is a reason to stop, named as such.
func TestRestoreIdentityStopsOnAPendingNote(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	if err := state.SavePending(state.Pending{}); err != nil {
		t.Skipf("can't write a pending note here: %v", err)
	}
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
	if err == nil || !strings.Contains(err.Error(), "unfinished first install") {
		t.Fatalf("%v", err)
	}
	if len(f.filled) != 0 {
		t.Errorf("something was created: %v", f.filled)
	}
}

// Postgres that already runs is left running, with no start and no stop.
func TestRestoreDatabaseLeavesARunningPostgresRunning(t *testing.T) {
	archive, f := databaseFixture(t)
	f.pg = newFakeBundledPg(true)
	if err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops()); err != nil {
		t.Fatal(err)
	}
	if len(f.pg.calls) != 0 || f.dump == "" {
		t.Errorf("Postgres calls %v, dump %q", f.pg.calls, f.dump)
	}
}
