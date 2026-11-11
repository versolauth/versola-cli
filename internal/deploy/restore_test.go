package deploy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/state"
)

// fakeRestore stands in for Docker and the machine around a restore.
type fakeRestore struct {
	st        *state.State
	stErr     error
	baoDir    string
	existing  map[string]bool // "volume:name", "container:name"
	running   []string
	tables    int
	daemonErr error

	extractErr error
	restoreErr error
	filled     map[string]string // volume -> what was put into it
	removed    []string
	dump       string // what pg_restore was fed
	pg         *fakeBundledPg
	sigs       chan os.Signal
	calls      []string
}

func newFakeRestore(baoDir string) *fakeRestore {
	return &fakeRestore{baoDir: baoDir, existing: map[string]bool{}, filled: map[string]string{}, pg: newFakeBundledPg(false), sigs: make(chan os.Signal, 3)}
}

func (f *fakeRestore) ops() restoreOps {
	return restoreOps{
		loadState:    func() (*state.State, error) { return f.st, f.stErr },
		openbaoDir:   func() (string, error) { return f.baoDir, nil },
		dockerDaemon: func() error { return f.daemonErr },
		exists:       func(kind, name string) (bool, error) { return f.existing[kind+":"+name], nil },
		prepareImage: func() error { f.calls = append(f.calls, "image"); return nil },
		createVolume: func(name string) error {
			f.calls = append(f.calls, "create "+name)
			f.filled[name] = ""
			return nil
		},
		removeVolume: func(name string) error {
			f.calls = append(f.calls, "remove "+name)
			f.removed = append(f.removed, name)
			delete(f.filled, name)
			return nil
		},
		extractVolume: func(volume string, r io.Reader) error {
			b, _ := io.ReadAll(r)
			f.filled[volume] = string(b)
			return f.extractErr
		},
		runningServices: func(string) ([]string, error) { return f.running, nil },
		bundledPg:       f.pg.ctl(),
		tableCount:      func(string, pgTarget) (int, error) { f.calls = append(f.calls, "count"); return f.tables, nil },
		pgRestore: func(_ string, _ pgTarget, r io.Reader) error {
			b, _ := io.ReadAll(r)
			f.dump = string(b)
			f.calls = append(f.calls, "pg_restore")
			return f.restoreErr
		},
		signals: func() (<-chan os.Signal, func()) { return f.sigs, func() {} },
	}
}

// restoreArchive makes an archive with the members of a bundled vps
// deployment, and a clean home to restore it into.
func restoreArchive(t *testing.T) (archive, baoDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	archive = filepath.Join(home, "a.tar.gz")
	buildArchive(t, archive, BackupManifest{PostgresDB: "auth", PostgresUser: "versola_app"}, vpsFiles())
	return archive, filepath.Join(home, ".versola", "openbao")
}

func TestRestoreIdentityPutsOpenBaoBack(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	var out strings.Builder
	if err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, &out, f.ops()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := f.filled["versola-openbao-file-vps"]; got != "tar-of-openbao" {
		t.Errorf("OpenBao's volume holds %q", got)
	}
	if _, ok := f.filled["versola-postgres-data-vps"]; !ok {
		t.Errorf("no empty Postgres volume was created: %v", f.calls)
	}
	for _, name := range []string{"vps.json", "vps-admin.json"} {
		fi, err := os.Stat(filepath.Join(baoDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want 0600", name, fi.Mode().Perm())
		}
	}
	for _, want := range []string{
		"versola configure vps 0.7.6 --auth-url https://id.example.com --proxy nginx",
		"versola backup restore " + quotePath(archive),
		"versola up",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the next steps lack %q:\n%s", want, out.String())
		}
	}
	// What it prints is names and sizes, never a value of the credentials.
	for _, secret := range []string{"SECRET-ID", "ROOT", "UNSEAL"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("the output carries a secret value (%s)", secret)
		}
	}
	// The image is made present before anything is created.
	if len(f.calls) == 0 || f.calls[0] != "image" {
		t.Errorf("order: %v", f.calls)
	}
	if f.pg.calls != nil {
		t.Errorf("Postgres was touched: %v", f.pg.calls)
	}
}

func TestRestoreIdentityReplacesNothing(t *testing.T) {
	for name, set := range map[string]func(f *fakeRestore, baoDir string){
		"a credentials file": func(f *fakeRestore, baoDir string) {
			os.MkdirAll(baoDir, 0o700)
			os.WriteFile(filepath.Join(baoDir, "vps.json"), []byte("{}"), 0o600)
		},
		"OpenBao's volume":    func(f *fakeRestore, _ string) { f.existing["volume:versola-openbao-file-vps"] = true },
		"the Postgres volume": func(f *fakeRestore, _ string) { f.existing["volume:versola-postgres-data-vps"] = true },
		"a container":         func(f *fakeRestore, _ string) { f.existing["container:versola-postgres-vps"] = true },
	} {
		archive, baoDir := restoreArchive(t)
		f := newFakeRestore(baoDir)
		f.stErr = state.ErrNotConfigured
		set(f, baoDir)
		err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
		if err == nil || !strings.Contains(err.Error(), "already has parts of a Versola deployment") || !strings.Contains(err.Error(), "`versola uninstall --purge`") {
			t.Errorf("%s: %v", name, err)
		}
		if len(f.filled) != 0 || len(f.calls) != 0 {
			t.Errorf("%s: something was done: %v %v", name, f.calls, f.filled)
		}
	}
}

func TestRestoreIdentityUndoesWhatItMadeOnFailure(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	f.extractErr = errors.New("tar failed")
	var out strings.Builder
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, &out, f.ops())
	if err == nil || !strings.Contains(err.Error(), "tar failed") {
		t.Fatalf("%v", err)
	}
	if len(f.filled) != 0 {
		t.Errorf("a volume is left: %v", f.filled)
	}
	for _, name := range []string{"vps.json", "vps-admin.json"} {
		if _, err := os.Stat(filepath.Join(baoDir, name)); err == nil {
			t.Errorf("%s is left behind", name)
		}
	}
	if !strings.Contains(out.String(), "removed again") {
		t.Errorf("it does not say what it undid:\n%s", out.String())
	}
}

func TestRestoreIdentityInterruptedLeavesNothing(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	f.sigs <- os.Interrupt
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
	if code := exitcode.Code(err); code != exitcode.Interrupted {
		t.Fatalf("exit code %d (%v), want %d", code, err, exitcode.Interrupted)
	}
	if len(f.filled) != 0 {
		t.Errorf("a volume is left: %v", f.filled)
	}
	if _, err := os.Stat(filepath.Join(baoDir, "vps.json")); err == nil {
		t.Errorf("a credentials file is left behind")
	}
}

func TestRestoreIdentityAsksFirstAndAbortsOnNo(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	err := restoreBackup(RestoreOptions{Archive: archive, In: strings.NewReader("n\n")}, io.Discard, f.ops())
	if code := exitcode.Code(err); code != exitcode.Aborted {
		t.Fatalf("exit code %d (%v), want %d", code, err, exitcode.Aborted)
	}
	if len(f.filled) != 0 {
		t.Errorf("a volume was created after a no: %v", f.filled)
	}
}

func TestRestoreRefusesWhatItDoesNotRestore(t *testing.T) {
	archive, baoDir := restoreArchive(t)
	dir := filepath.Dir(archive)

	external := filepath.Join(dir, "ext.tar.gz")
	buildArchive(t, external, BackupManifest{PostgresMode: "external"}, vpsFiles())
	f := newFakeRestore(baoDir)
	f.stErr = state.ErrNotConfigured
	if err := restoreBackup(RestoreOptions{Archive: external}, io.Discard, f.ops()); err == nil || !strings.Contains(err.Error(), "Postgres of your own") || !strings.Contains(err.Error(), "pg_restore") {
		t.Errorf("an external database: %v", err)
	}

	nodump := filepath.Join(dir, "nodump.tar.gz")
	buildArchive(t, nodump, BackupManifest{Skipped: []BackupSkipped{{Name: backupDumpMember, Reason: "--skip-database"}}}, vpsFiles()[1:])
	if err := restoreBackup(RestoreOptions{Archive: nodump}, io.Discard, f.ops()); err == nil || !strings.Contains(err.Error(), "no database dump (--skip-database)") {
		t.Errorf("no dump: %v", err)
	}

	nobao := filepath.Join(dir, "nobao.tar.gz")
	buildArchive(t, nobao, BackupManifest{Skipped: []BackupSkipped{{Name: backupOpenbaoMember, Reason: "set up by hand"}}}, append(vpsFiles()[:1], vpsFiles()[2:]...))
	if err := restoreBackup(RestoreOptions{Archive: nobao, AssumeYes: true}, io.Discard, f.ops()); err == nil || !strings.Contains(err.Error(), "no openbao-volume.tar (set up by hand)") {
		t.Errorf("no OpenBao data: %v", err)
	}

	f.stErr = nil
	f.st = &state.State{Target: "local", Version: "0.7.6"}
	if err := restoreBackup(RestoreOptions{Archive: archive}, io.Discard, f.ops()); err == nil || !strings.Contains(err.Error(), "tracks a local deployment") {
		t.Errorf("a local record: %v", err)
	}
	f.st, f.stErr = nil, state.ErrOldInstall
	if err := restoreBackup(RestoreOptions{Archive: archive}, io.Discard, f.ops()); err == nil || !strings.Contains(err.Error(), "older versola-cli") {
		t.Errorf("an old install: %v", err)
	}
	f.stErr, f.daemonErr = state.ErrNotConfigured, errors.New("Docker daemon: not reachable")
	if err := restoreBackup(RestoreOptions{Archive: archive}, io.Discard, f.ops()); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("no Docker: %v", err)
	}
}

// ---- the database -------------------------------------------------------

// databaseFixture is a deployment that `configure` has set up, on Versola
// 0.7.6 with its own Postgres, in a home that also holds the OpenBao admin
// file the archive was taken with.
func databaseFixture(t *testing.T) (archive string, f *fakeRestore) {
	t.Helper()
	st, _, _ := backupFixture(t, "vps", PostgresModeBundled)
	st.Version = "0.7.6"
	home, _ := os.UserHomeDir()
	baoDir := filepath.Join(home, ".versola", "openbao")
	archive = filepath.Join(home, "a.tar.gz")
	buildArchive(t, archive, BackupManifest{PostgresDB: "auth", PostgresUser: "versola_app"}, vpsFiles())
	f = newFakeRestore(baoDir)
	f.st = st
	return archive, f
}

func TestRestoreDatabaseIntoAnEmptyOne(t *testing.T) {
	archive, f := databaseFixture(t)
	var out strings.Builder
	if err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, &out, f.ops()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.dump != "PGDMP-fake-dump" {
		t.Errorf("pg_restore was fed %q", f.dump)
	}
	// Postgres was down, so it was started for this alone and stopped again.
	if got := strings.Join(f.pg.calls, ","); got != "start,stop" {
		t.Errorf("Postgres calls: %s", got)
	}
	if !strings.Contains(out.String(), "`versola up`") {
		t.Errorf("no next step:\n%s", out.String())
	}
}

func TestRestoreDatabaseWithDataNeedsTheFlagAndTheWord(t *testing.T) {
	archive, f := databaseFixture(t)
	f.tables = 14

	// Without the flag: refused, and nothing was fed to pg_restore.
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
	if err == nil || !strings.Contains(err.Error(), "--replace-database") || !strings.Contains(err.Error(), "14 tables") {
		t.Fatalf("no flag: %v", err)
	}
	if f.dump != "" {
		t.Errorf("the database was touched without the flag")
	}

	// With the flag, --yes does not answer the typed question.
	var out strings.Builder
	err = restoreBackup(RestoreOptions{Archive: archive, ReplaceDatabase: true, AssumeYes: true, In: strings.NewReader("")}, &out, f.ops())
	if code := exitcode.Code(err); code != exitcode.Aborted {
		t.Fatalf("exit code %d (%v), want %d", code, err, exitcode.Aborted)
	}
	if !strings.Contains(out.String(), "--yes does not answer this question") || f.dump != "" {
		t.Errorf("--yes answered it, or the database was touched: %q", out.String())
	}
	// A wrong word.
	err = restoreBackup(RestoreOptions{Archive: archive, ReplaceDatabase: true, In: strings.NewReader("yes\n")}, io.Discard, f.ops())
	if code := exitcode.Code(err); code != exitcode.Aborted || f.dump != "" {
		t.Fatalf("\"yes\" was taken for the word: %v", err)
	}
	// The word.
	err = restoreBackup(RestoreOptions{Archive: archive, ReplaceDatabase: true, In: strings.NewReader("vps\n")}, io.Discard, f.ops())
	if err != nil || f.dump != "PGDMP-fake-dump" {
		t.Fatalf("with the word: %v, fed %q", err, f.dump)
	}
}

func TestRestoreDatabaseFailureLeavesPostgresStopped(t *testing.T) {
	archive, f := databaseFixture(t)
	f.restoreErr = errors.New("pg_restore failed: boom -- it ran in one transaction, so the database is as it was")
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
	if err == nil || !strings.Contains(err.Error(), "one transaction") {
		t.Fatalf("%v", err)
	}
	if got := strings.Join(f.pg.calls, ","); got != "start,stop" {
		t.Errorf("Postgres calls after a failure: %s", got)
	}
}

func TestRestoreDatabaseRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(f *fakeRestore)
		want string
	}{
		{"another version", func(f *fakeRestore) { f.st.Version = "0.7.5" }, "taken on Versola 0.7.6, and this deployment is on 0.7.5"},
		{"a Postgres of the customer's own", func(f *fakeRestore) { f.st.PostgresMode = "" }, "Postgres of your own"},
		{"services running", func(f *fakeRestore) { f.running = []string{"postgres", "auth", "central"} }, "services are running (auth, central)"},
		{"an unfinished rolling upgrade", func(f *fakeRestore) { f.st.PreviousVersion = "0.7.5" }, "rolling upgrade"},
		{"the bundle is gone", func(f *fakeRestore) { f.st.BundleDir = "nowhere" }, "files are missing"},
	}
	for _, c := range cases {
		archive, f := databaseFixture(t)
		c.edit(f)
		err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if f.dump != "" || len(f.pg.calls) != 0 {
			t.Errorf("%s: something was touched (%v)", c.name, f.pg.calls)
		}
	}
}

func TestRestoreDatabaseRefusesAnotherOpenBao(t *testing.T) {
	archive, f := databaseFixture(t)
	// The deployment on this machine has its own OpenBao keys, not the archive's.
	if err := os.WriteFile(filepath.Join(f.baoDir, "vps-admin.json"), []byte(`{"rootToken":"OTHER","unsealKey":"OTHER-KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
	if err == nil || !strings.Contains(err.Error(), "not the one the archive was taken with") {
		t.Fatalf("%v", err)
	}
	for _, v := range []string{"OTHER-KEY", "UNSEAL", "ROOT"} {
		if strings.Contains(err.Error(), v) {
			t.Errorf("the refusal shows a key (%s)", v)
		}
	}
	if f.dump != "" {
		t.Errorf("the database was touched")
	}
	// The same keys (the usual case, after the first restore): fine.
	if err := os.WriteFile(filepath.Join(f.baoDir, "vps-admin.json"), []byte(`{"rootToken":"R2","unsealKey":"UNSEAL"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops()); err != nil {
		t.Errorf("the same unseal key was refused: %v", err)
	}
}

func TestRestoreDatabaseAnotherDatabaseName(t *testing.T) {
	archive, f := databaseFixture(t)
	buildArchive(t, archive, BackupManifest{PostgresDB: "other", PostgresUser: "versola_app"}, vpsFiles())
	err := restoreBackup(RestoreOptions{Archive: archive, AssumeYes: true}, io.Discard, f.ops())
	if err == nil || !strings.Contains(err.Error(), `"other"`) || !strings.Contains(err.Error(), "--postgres-db other") {
		t.Fatalf("%v", err)
	}
}

// ---- what is printed and what is run --------------------------------------

func TestRestoredConfigureCommand(t *testing.T) {
	days := 0
	rec := &state.State{AuthURL: "https://id.example.com", ProxyMode: "external", CentralURL: "https://c.example.com:8093",
		MTLSDomain: "mtls.example.com", CentralMTLSDomain: "cm.example.com", CentralDomain: "central.example.com",
		CertLifetimeDays: &days, EstEnabled: true, ImageRegistry: "cr.example.com/versola", PostgresDB: "versola"}
	got := RestoredConfigureCommand("0.7.6", rec, true, false)
	want := "versola configure vps 0.7.6 --auth-url https://id.example.com --proxy external --central-url https://c.example.com:8093" +
		" --mtls-domain mtls.example.com --central-mtls-domain cm.example.com --central-domain central.example.com" +
		" --cert-lifetime-days 0 --est --registry cr.example.com/versola --postgres-db versola --setup-openbao"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	// --acme-staging only when the archive says the deployment used it, and
	// it comes last, after --setup-openbao.
	if strings.Contains(got, "--acme-staging") {
		t.Errorf("staging flag without staging: %s", got)
	}
	if staged := RestoredConfigureCommand("0.7.6", rec, true, true); staged != got+" --acme-staging" {
		t.Errorf("staging: %s", staged)
	}
	// Values come from a file: whatever would end the command or escape the terminal is neutralised.
	evil := RestoredConfigureCommand("0.7.6", &state.State{AuthURL: "https://x.example.com'; rm -rf ~; echo '\x1b[2J"}, false, false)
	if strings.ContainsAny(evil, "\x1b\n") || strings.Count(evil, "'") != 2 {
		t.Errorf("the value is not one quoted word: %q", evil)
	}
}

func TestQuoting(t *testing.T) {
	long := "https://a-very-long-name-for-an-identity-server.subdomain.example.com/and/a/path"
	if got := quoteArg(long); got != long {
		t.Errorf("a long URL was cut or quoted: %s", got)
	}
	for in, want := range map[string]string{
		"/home/me/versola-backups/a.tar.gz": "/home/me/versola-backups/a.tar.gz",
		"/home/me/my backups/a.tar.gz":      "'/home/me/my backups/a.tar.gz'",
		"/home/me/it's.tar.gz":              `'/home/me/it'\''s.tar.gz'`,
		"/home/мария/a.tar.gz":              "'/home/мария/a.tar.gz'",
	} {
		if got := quotePath(in); got != want {
			t.Errorf("quotePath(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRestoreArgsKeepThePasswordOffTheCommandLine(t *testing.T) {
	pg := pgTarget{db: "auth", user: "versola_app"}
	for name, args := range map[string][]string{
		"restore": pgRestoreArgs("pgc", pg),
		"count":   pgCountArgs("pgc", pg),
	} {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "PGPASSWORD=") && !strings.Contains(joined, `PGPASSWORD="$POSTGRES_PASSWORD"`) {
			t.Errorf("%s: a password value in the arguments: %s", name, joined)
		}
		if !strings.Contains(joined, "PGUSER=versola_app") || !strings.Contains(joined, "PGDATABASE=auth") || !strings.HasPrefix(joined, "exec -i ") {
			t.Errorf("%s: %s", name, joined)
		}
	}
	r := strings.Join(pgRestoreArgs("pgc", pg), " ")
	// Emptied first, in the same transaction as the dump, committed only when pg_restore finished.
	for _, want := range []string{"echo 'BEGIN;'", "DROP SCHEMA %I CASCADE", "CREATE SCHEMA public", "pg_restore --no-password --no-owner -f -", "echo 'COMMIT;'", "ON_ERROR_STOP=on", `--dbname "$PGDATABASE"`, `PGPASSWORD="$POSTGRES_PASSWORD"`} {
		if !strings.Contains(r, want) {
			t.Errorf("the restore script lacks %s: %s", want, r)
		}
	}
	if strings.Contains(r, "--single-transaction") || strings.Contains(r, "--clean") {
		t.Errorf("the restore script must not rely on pg_restore --clean (it leaves what the dump lacks): %s", r)
	}
	// COMMIT is behind pg_restore's success, never unconditional.
	if i, j := strings.Index(r, "pg_restore --no-password"), strings.Index(r, "echo 'COMMIT;'"); i < 0 || j < i || !strings.Contains(r[i:j], "; then echo 0") {
		t.Errorf("COMMIT is not conditional on pg_restore: %s", r)
	}
	v := strings.Join(extractVolumeArgs("vol"), " ")
	if !strings.Contains(v, "--network none") || strings.Count(v, " -v ") != 1 || !strings.Contains(v, "vol:/dst") || !strings.Contains(v, "--pull never") {
		t.Errorf("the volume is filled with more than the volume: %s", v)
	}
}
