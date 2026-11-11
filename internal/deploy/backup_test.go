package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

const backupAuthConf = `postgres {
  url = "jdbc:postgresql://db.example.com:5432/auth?currentSchema=auth&sslmode=verify-full"
  user = "versola_app"
  password = ${POSTGRES_PASSWORD}
}
`

// backupFixture makes a temp home with a deployment bundle, a state.json
// and the OpenBao files, and returns a state pointing at it and the ops of
// a backup that stands in for Docker.
func backupFixture(t *testing.T, target, pgMode string) (*state.State, backupOps, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	stDir := filepath.Join(home, ".versola", "active")
	baoDir := filepath.Join(home, ".versola", "openbao")
	bundle := filepath.Join(stDir, "bundle-1")
	for _, d := range []string{bundle, baoDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bundle, "compose.yml"), "services: {}\n")
	write(filepath.Join(bundle, "auth.conf"), backupAuthConf)
	write(filepath.Join(stDir, "state.json"), `{"schemaVersion":4}`+"\n")
	write(filepath.Join(baoDir, target+".json"), `{"roleId":"r","secretId":"SECRET-ID"}`)
	write(filepath.Join(baoDir, target+"-admin.json"), `{"rootToken":"ROOT","unsealKey":"UNSEAL"}`)

	st := &state.State{Target: target, Version: "0.9.0", BundleDir: "bundle-1", ProxyMode: "x", PostgresMode: pgMode}
	ops := backupOps{
		now: func() time.Time { return time.Date(2026, 11, 2, 10, 4, 5, 0, time.UTC) },
		dumpDatabase: func(_ *state.State, _ string, _ pgTarget, w io.Writer) error {
			_, err := io.WriteString(w, "PGDMP-fake")
			return err
		},
		copyOpenbao: func(_ string, w io.Writer, _ io.Writer) (string, error) {
			_, err := io.WriteString(w, "tar-of-openbao")
			return "", err
		},
		openbaoDir:      func() (string, error) { return baoDir, nil },
		stateDir:        func() (string, error) { return stDir, nil },
		proxyGeneration: func() string { return "gen-000007" },
	}
	return st, ops, filepath.Join(home, "out")
}

// readArchive returns the members of a tar.gz in order, with their content.
func readArchive(t *testing.T, path string) ([]string, map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	content := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		content[h.Name] = string(b)
	}
	return names, content
}

// A deployment on Let's Encrypt staging says so in the manifest (read from the
// bundle's proxy configuration, as StoredVps reads it); any other does not
// carry the field at all, so that the format and old archives are unchanged,
// and neither does a local one.
func TestCreateBackupRecordsACMEStaging(t *testing.T) {
	for _, c := range []struct {
		name    string
		target  string
		staging bool
		want    bool
	}{
		{"vps staging", "vps", true, true},
		{"vps production", "vps", false, false},
		{"local", "local", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, ops, out := backupFixture(t, c.target, PostgresModeBundled)
			if c.staging {
				home, _ := os.UserHomeDir()
				conf := filepath.Join(home, ".versola", "active", "bundle-1", "proxy", "conf.d")
				if err := os.MkdirAll(conf, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(conf, "versola.conf"), []byte("acme_directory "+proxy.ACMEStaging+";\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path, err := createBackup(st, BackupOptions{OutDir: out, CLIVersion: "1.2.3"}, io.Discard, ops)
			if err != nil {
				t.Fatal(err)
			}
			m, err := ReadBackupManifest(path)
			if err != nil {
				t.Fatal(err)
			}
			if m.ACMEStaging != c.want {
				t.Errorf("ACMEStaging = %v, want %v", m.ACMEStaging, c.want)
			}
			_, content := readArchive(t, path)
			if has := strings.Contains(content["manifest.json"], "acmeStaging"); has != c.want {
				t.Errorf("manifest has the field: %v, want %v", has, c.want)
			}
		})
	}
}

func TestCreateBackupLayoutAndManifest(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
	var log strings.Builder
	path, err := createBackup(st, BackupOptions{OutDir: out, CLIVersion: "1.2.3"}, &log, ops)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "versola-backup-vps-0.9.0-20261102T100405Z.tar.gz"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("archive mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	names, content := readArchive(t, path)
	want := []string{"manifest.json", "database.dump", "openbao-volume.tar", "openbao/vps.json", "openbao/vps-admin.json", "state/state.json"}
	if !slices.Equal(names, want) {
		t.Fatalf("members = %v, want %v", names, want)
	}

	m, err := ReadBackupManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != 1 || m.CLIVersion != "1.2.3" || m.VersolaVersion != "0.9.0" || m.Target != "vps" || m.PostgresMode != "bundled" ||
		m.PostgresDB != "auth" || m.PostgresUser != "versola_app" || m.ProxyGeneration != "gen-000007" {
		t.Errorf("manifest = %+v", m)
	}
	if len(m.Members) != len(want)-1 {
		t.Fatalf("manifest members = %v", m.Members)
	}
	for _, bm := range m.Members {
		sum := sha256.Sum256([]byte(content[bm.Name]))
		if bm.SHA256 != hex.EncodeToString(sum[:]) || bm.Size != int64(len(content[bm.Name])) {
			t.Errorf("%s: manifest says %d bytes %s, archive has %d bytes", bm.Name, bm.Size, bm.SHA256, len(content[bm.Name]))
		}
	}
	if !slices.ContainsFunc(m.Skipped, func(s BackupSkipped) bool { return s.Name == "openbao/vps-postgres-role.sql" }) {
		t.Errorf("the missing role script is not listed as left out: %v", m.Skipped)
	}

	// Names and sizes only: no secret value reaches the output.
	for _, secret := range []string{"ROOT", "UNSEAL", "SECRET-ID", "PGDMP-fake"} {
		if strings.Contains(log.String(), secret) || strings.Contains(content["manifest.json"], secret) {
			t.Errorf("%q printed or in the manifest", secret)
		}
	}
	// Nothing but the archive is left in the directory.
	ents, _ := os.ReadDir(out)
	if len(ents) != 1 {
		t.Errorf("out dir holds %d entries, want only the archive", len(ents))
	}
}

func TestCreateBackupExternalModeAndSkipDatabase(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", "")
	ops.dumpDatabase = func(*state.State, string, pgTarget, io.Writer) error {
		t.Fatal("dumped with --skip-database")
		return nil
	}
	path, err := createBackup(st, BackupOptions{OutDir: out, SkipDatabase: true}, io.Discard, ops)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadBackupManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.PostgresMode != "external" {
		t.Errorf("postgresMode = %q, want external", m.PostgresMode)
	}
	if !slices.Contains(m.Skipped, BackupSkipped{"database.dump", "--skip-database"}) {
		t.Errorf("skipped = %v", m.Skipped)
	}
}

func TestCreateBackupLocalIsBundled(t *testing.T) {
	st, ops, out := backupFixture(t, "local", "")
	path, err := createBackup(st, BackupOptions{OutDir: out}, io.Discard, ops)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ReadBackupManifest(path)
	if m.PostgresMode != "bundled" || m.ProxyGeneration != "" {
		t.Errorf("local manifest = %+v", m)
	}
}

func TestCreateBackupOpenbaoSkipped(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
	ops.copyOpenbao = func(string, io.Writer, io.Writer) (string, error) { return "set up by hand", nil }
	var log strings.Builder
	path, err := createBackup(st, BackupOptions{OutDir: out}, &log, ops)
	// Written, but not a success: a dump without OpenBao's data can't be read.
	if err == nil || !strings.Contains(err.Error(), "WITHOUT OpenBao's data") {
		t.Fatalf("err = %v, want the incomplete-backup error", err)
	}
	names, _ := readArchive(t, path)
	if slices.Contains(names, "openbao-volume.tar") {
		t.Error("an empty OpenBao copy is in the archive")
	}
	if strings.Contains(log.String(), "openbao-volume.tar                       0 B") {
		t.Error("the skipped copy is printed as a member")
	}
	if !strings.Contains(log.String(), "openbao-volume.tar: set up by hand") {
		t.Errorf("the summary doesn't say the copy was left out:\n%s", log.String())
	}
}

// A failure halfway leaves no archive and no temp file behind.
func TestCreateBackupFailureLeavesNothing(t *testing.T) {
	for name, mutate := range map[string]func(*backupOps){
		"dump fails": func(o *backupOps) {
			o.dumpDatabase = func(_ *state.State, _ string, _ pgTarget, w io.Writer) error {
				io.WriteString(w, "half a dump")
				return errors.New("pg_dump failed")
			}
		},
		"openbao copy fails": func(o *backupOps) {
			o.copyOpenbao = func(string, io.Writer, io.Writer) (string, error) { return "", errors.New("tar failed") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
			mutate(&ops)
			if _, err := createBackup(st, BackupOptions{OutDir: out}, io.Discard, ops); err == nil {
				t.Fatal("no error")
			}
			ents, err := os.ReadDir(out)
			if err != nil {
				t.Fatal(err)
			}
			if len(ents) != 0 {
				var left []string
				for _, e := range ents {
					left = append(left, e.Name())
				}
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

func TestPackBackupRemovesTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	staging := t.TempDir()
	m := BackupManifest{Members: []BackupMember{{Name: "missing", Size: 3}}}
	if err := packBackup(filepath.Join(dir, "x.tar.gz"), staging, m); err == nil {
		t.Fatal("no error for a missing staged member")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("left %d entries behind", len(ents))
	}
}

func TestCreateBackupRefusesOtherTargets(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", "")
	st.Target = "edge"
	if _, err := createBackup(st, BackupOptions{OutDir: out}, io.Discard, ops); err == nil {
		t.Fatal("no error")
	}
}

func TestPgDumpArgs(t *testing.T) {
	tg := pgTarget{host: "db.example.com", port: "5432", db: "auth", user: "versola_app", sslmode: "verify-full", caFile: "/home/u/.versola/active/b/postgres-ca/ca.crt"}

	ext := pgDumpExternalArgs(tg, "/b/auth.secrets.env")
	joined := strings.Join(ext, " ")
	for _, want := range []string{"--env-file /b/auth.secrets.env", "PGHOST=db.example.com", "PGUSER=versola_app", "PGDATABASE=auth",
		"PGSSLMODE=verify-full", "/home/u/.versola/active/b/postgres-ca/ca.crt:" + pgCAContainerDir + "/ca.crt:ro", pgCheckImage} {
		if !strings.Contains(joined, want) {
			t.Errorf("external args lack %q: %v", want, ext)
		}
	}
	in := pgDumpInContainerArgs("abc123", tg)
	if !slices.Equal(in[:6], []string{"exec", "-e", "PGUSER=versola_app", "-e", "PGDATABASE=auth", "abc123"}) {
		t.Errorf("in-container args = %v", in)
	}
	for _, args := range [][]string{ext, in} {
		last := args[len(args)-1]
		if last != pgDumpScript || !strings.Contains(last, `"$POSTGRES_PASSWORD"`) || !strings.Contains(last, "--format=custom") {
			t.Errorf("the dump is not pg_dump -Fc with the password from the environment: %q", last)
		}
		for _, a := range args {
			// The script names the variable ("$POSTGRES_PASSWORD"); a value
			// would be anything else after the "=".
			if (strings.HasPrefix(a, "PGPASSWORD=") && !strings.HasPrefix(a, `PGPASSWORD="$POSTGRES_PASSWORD"`)) || strings.HasPrefix(a, "POSTGRES_PASSWORD=") {
				t.Errorf("a password on the command line: %q", a)
			}
		}
	}
}

func TestOpenbaoTarArgsReadOnly(t *testing.T) {
	args := strings.Join(openbaoTarArgs("versola-openbao-file-vps"), " ")
	if !strings.Contains(args, "-v versola-openbao-file-vps:/src:ro") || !strings.Contains(args, "--network none") {
		t.Errorf("args = %s", args)
	}
}

func TestBackupNotIncluded(t *testing.T) {
	vps := strings.Join(backupNotIncluded("vps"), "\n")
	for _, want := range []string{"ACME certificate volume", "versola-acme-vps", "mutual-TLS volume"} {
		if !strings.Contains(vps, want) {
			t.Errorf("vps text lacks %q:\n%s", want, vps)
		}
	}
	if local := strings.Join(backupNotIncluded("local"), "\n"); strings.Contains(local, "ACME") || !strings.Contains(local, "mutual-TLS") {
		t.Errorf("local text:\n%s", local)
	}
}

func TestListBackups(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
	first, err := createBackup(st, BackupOptions{OutDir: out}, io.Discard, ops)
	if err != nil {
		t.Fatal(err)
	}
	ops.now = func() time.Time { return time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC) }
	second, err := createBackup(st, BackupOptions{OutDir: out}, io.Discard, ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "versola-backup-broken.tar.gz"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, _, err := ListBackups(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Path != second || list[1].Path != first || list[2].Err == nil {
		t.Errorf("list = %+v", list)
	}

	empty, dir, err := ListBackups(filepath.Join(out, "none"))
	if err != nil || len(empty) != 0 || dir == "" {
		t.Errorf("missing dir: %v %v %q", empty, err, dir)
	}
}

// fakeOpenbao records what copyOpenbaoWith does with OpenBao.
type fakeOpenbao struct {
	volume, running, configMissing, canUnseal bool
	stopErr, startErr, unsealErr, tarErr      error
	prepareErr                                error
	interrupt                                 bool
	calls                                     []string
}

func (f *fakeOpenbao) ctl() openbaoCtl {
	return openbaoCtl{
		volumeExists: func(string) bool { return f.volume },
		inspect: func(string) (bool, bool, bool, error) {
			return true, f.running, f.configMissing, nil
		},
		canUnseal: func(string, string) (bool, string, error) {
			f.calls = append(f.calls, "check-unseal")
			return f.canUnseal, "no saved unseal key", nil
		},
		stop:   func(string) error { f.calls = append(f.calls, "stop"); return f.stopErr },
		start:  func(string) error { f.calls = append(f.calls, "start"); return f.startErr },
		unseal: func(string, string, io.Writer) error { f.calls = append(f.calls, "unseal"); return f.unsealErr },
		tar: func(_ string, w io.Writer) error {
			f.calls = append(f.calls, "tar")
			io.WriteString(w, "data")
			return f.tarErr
		},
		prepare: func() error { f.calls = append(f.calls, "prepare"); return f.prepareErr },
		signals: func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 1)
			if f.interrupt {
				ch <- os.Interrupt
			}
			return ch, func() {}
		},
	}
}

func TestCopyOpenbao(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		f         fakeOpenbao
		wantCalls []string
		wantSkip  bool
		wantErr   bool
	}{
		{"no volume", fakeOpenbao{}, nil, true, false},
		{"stopped: copied as it is", fakeOpenbao{volume: true}, []string{"prepare", "tar"}, false, false},
		{"running: stop, copy, start, unseal", fakeOpenbao{volume: true, running: true, canUnseal: true},
			[]string{"prepare", "unseal", "check-unseal", "stop", "tar", "start", "unseal"}, false, false},
		{"running on a gone config: never stopped", fakeOpenbao{volume: true, running: true, configMissing: true, canUnseal: true}, []string{"prepare"}, true, false},
		{"can't unseal again: never stopped", fakeOpenbao{volume: true, running: true}, []string{"prepare", "unseal", "check-unseal"}, true, false},
		// After a reboot it runs sealed: unsealed first, then checked and copied.
		{"sealed and can't be unsealed: never stopped", fakeOpenbao{volume: true, running: true, canUnseal: true, unsealErr: boom}, []string{"prepare", "unseal"}, true, false},
		{"image missing and the pull fails: never stopped", fakeOpenbao{volume: true, running: true, canUnseal: true, prepareErr: boom}, []string{"prepare"}, false, true},
		{"copy fails: still started and unsealed", fakeOpenbao{volume: true, running: true, canUnseal: true, tarErr: boom},
			[]string{"prepare", "unseal", "check-unseal", "stop", "tar", "start", "unseal"}, false, true},
		{"stop fails: no copy, still started", fakeOpenbao{volume: true, running: true, canUnseal: true, stopErr: boom},
			[]string{"prepare", "unseal", "check-unseal", "stop", "start", "unseal"}, false, true},
		{"interrupted: started and unsealed, then the error", fakeOpenbao{volume: true, running: true, canUnseal: true, interrupt: true},
			[]string{"prepare", "unseal", "check-unseal", "stop", "tar", "start", "unseal"}, false, true},
		{"start fails", fakeOpenbao{volume: true, running: true, canUnseal: true, startErr: boom},
			[]string{"prepare", "unseal", "check-unseal", "stop", "tar", "start"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.f
			skip, err := copyOpenbaoWith(f.ctl(), "vps", io.Discard, io.Discard)
			if (err != nil) != tt.wantErr || (skip != "") != tt.wantSkip {
				t.Errorf("skip=%q err=%v", skip, err)
			}
			if !slices.Equal(f.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", f.calls, tt.wantCalls)
			}
		})
	}
}

func TestCreateBackupSweepsLeftovers(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
	other := filepath.Join(out, ".staging-versola-backup-otherhost.invalid-1")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{backupTempPrefix(".staging-") + "123", backupTempPrefix(".tmp-") + "456"} {
		if err := os.MkdirAll(filepath.Join(out, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(out, "versola-backup-old.tar.gz")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createBackup(st, BackupOptions{OutDir: out}, io.Discard, ops); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(out)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") && filepath.Join(out, e.Name()) != other {
			t.Errorf("leftover not removed: %s", e.Name())
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("an older archive was removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another host's staging directory was removed")
	}
}

func TestCreateBackupRecordsUnfinishedRollout(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
	st.PreviousVersion = "0.8.0"
	var log strings.Builder
	path, err := createBackup(st, BackupOptions{OutDir: out}, &log, ops)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ReadBackupManifest(path)
	if !strings.Contains(m.UnfinishedRollout, "0.8.0") || !strings.Contains(log.String(), "not finished") {
		t.Errorf("unfinishedRollout = %q, log:\n%s", m.UnfinishedRollout, log.String())
	}
}

func TestUnfinishedRolloutDuringAbort(t *testing.T) {
	st := &state.State{Version: "0.8.0", PreviousVersion: "0.9.0", RollingBack: true}
	why, schema := unfinishedRollout(st)
	if schema != "0.9.0" || !strings.Contains(why, "abort back to 0.8.0 from 0.9.0") {
		t.Errorf("why=%q schema=%q", why, schema)
	}
}
