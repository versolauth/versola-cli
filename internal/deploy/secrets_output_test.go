package deploy

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/openbao"
)

// A password no real one looks like, with the characters that need quoting
// in SQL (') and that psql would treat specially outside a literal (: \ $).
const canaryPassword = `canary-PW-'q':x\y$z`

const testAuthConf = `postgres {
  url = "jdbc:postgresql://127.0.0.1:5432/auth?currentSchema=auth"
  user = "versola_app"
  password = ${POSTGRES_PASSWORD}
}`

func bundleWithAuthConf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(testAuthConf), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPostgresRoleScript(t *testing.T) {
	got := postgresRoleScript(`we"i'rd`, `pa'ss`, true)
	for _, want := range []string{
		"\\set ON_ERROR_STOP on\nSET standard_conforming_strings = on;\n",
		`SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'we"i''rd') AS versola_create_role \gset` + "\n",
		"\\if :versola_create_role\n",
		`CREATE ROLE "we""i'rd" WITH LOGIN PASSWORD 'pa''ss';` + "\n\\else\n",
		`ALTER ROLE "we""i'rd" WITH LOGIN PASSWORD 'pa''ss';` + "\n\\endif\n",
		"verifier, not the password",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script lacks %q:\n%s", want, got)
		}
	}
	if plain := postgresRoleScript("r", "p", false); !strings.Contains(plain, "holds the password itself") {
		t.Errorf("a script with the password itself must say so:\n%s", plain)
	}
}

func TestScramVerifierMatchesRFC7677(t *testing.T) {
	// RFC 7677's example (password "pencil"); the expected keys were computed
	// independently (Python hashlib) and checked against the RFC's server
	// signature.
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	got, err := scramVerifierWithSalt("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestScramVerifier(t *testing.T) {
	a, ok, err := scramVerifier(canaryPassword)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	b, _, _ := scramVerifier(canaryPassword)
	if !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") || strings.Contains(a, "canary") || strings.ContainsAny(a, `'\`) {
		t.Errorf("verifier %q", a)
	}
	if a == b {
		t.Error("the salt must be random")
	}
	if _, ok, err := scramVerifier("пароль"); ok || err != nil {
		t.Errorf("non-ASCII: ok=%v err=%v", ok, err)
	}
}

// fakeHome points the home directory at a fresh temp dir, so the role script
// lands in <it>/.versola/openbao.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func roleScriptPath(home string) string {
	return filepath.Join(home, ".versola", "openbao", "vps-postgres-role.sql")
}

func TestAnnouncePostgresRoleWritesTheFileAndNeverPrintsThePassword(t *testing.T) {
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	var out bytes.Buffer
	path, err := announcePostgresRole(&out, dir, "vps", canaryPassword)
	if err != nil {
		t.Fatal(err)
	}
	if path != roleScriptPath(home) {
		t.Errorf("path %q", path)
	}

	if strings.Contains(out.String(), "canary-PW") {
		t.Fatalf("the password reached the terminal:\n%s", out.String())
	}
	for _, want := range []string{path, "sudo -u postgres psql < " + shellQuote(path), `"versola_app"`, filepath.Join(dir, "auth.secrets.env")} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "canary-PW") {
		t.Errorf("the file holds the password, not its verifier:\n%s", b)
	}
	if !strings.Contains(string(b), `CREATE ROLE "versola_app" WITH LOGIN PASSWORD 'SCRAM-SHA-256$4096:`) {
		t.Errorf("file content:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != want {
				t.Errorf("%s: mode %o, want %o", p, mode, want)
			}
		}
	}
}

func TestAnnouncePostgresRoleOutlivesTheBundle(t *testing.T) {
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	if _, err := announcePostgresRole(&bytes.Buffer{}, dir, "vps", canaryPassword); err != nil {
		t.Fatal(err)
	}
	// A later configure removes the bundle (pruneBundles).
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(roleScriptPath(home)); err != nil {
		t.Errorf("the SQL went with the bundle: %v", err)
	}
}

func TestAnnouncePostgresRoleReplacesAnOlderFile(t *testing.T) {
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	if _, err := announcePostgresRole(&bytes.Buffer{}, dir, "vps", "old-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := announcePostgresRole(&bytes.Buffer{}, dir, "vps", canaryPassword); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(roleScriptPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "old-password") || !strings.Contains(string(b), "SCRAM-SHA-256$") {
		t.Errorf("not a fresh script:\n%s", b)
	}
}

func TestAnnouncePostgresRoleDoesNotWriteThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	path := roleScriptPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("left here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if _, err := announcePostgresRole(&bytes.Buffer{}, dir, "vps", canaryPassword); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(elsewhere); string(b) != "left here" {
		t.Errorf("wrote through the link: %q", b)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the link is still in place: %v", err)
	}
}

func TestAnnouncePostgresRoleWhenTheFileCantBeWritten(t *testing.T) {
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	// ~/.versola/openbao is a file, so the directory can't be created.
	if err := os.MkdirAll(filepath.Join(home, ".versola"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".versola", "openbao"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := announcePostgresRole(&out, dir, "vps", canaryPassword)
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "canary-PW") || strings.Contains(out.String(), "canary-PW") {
		t.Fatalf("the password leaked: %v / %s", err, out.String())
	}
	if !strings.Contains(err.Error(), "auth.secrets.env") {
		t.Errorf("the error should say where the password is: %v", err)
	}
}

func TestAnnouncePostgresRoleWithoutARoleName(t *testing.T) {
	home := fakeHome(t)
	dir := t.TempDir() // no auth.conf
	var out bytes.Buffer
	path, err := announcePostgresRole(&out, dir, "vps", canaryPassword)
	// The password is stored already: say what to do, don't stop configure.
	if err != nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if strings.Contains(out.String(), "canary-PW") {
		t.Fatalf("printed the password:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "auth.secrets.env") {
		t.Errorf("should say where the password is:\n%s", out.String())
	}
	if _, serr := os.Stat(roleScriptPath(home)); !os.IsNotExist(serr) {
		t.Errorf("a script was written anyway: %v", serr)
	}
}

func TestAnnouncePostgresRoleReplacesWhateverIsAtThePath(t *testing.T) {
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	path := roleScriptPath(home)

	// An empty directory where the file goes: removed, then the file is written.
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := announcePostgresRole(&bytes.Buffer{}, dir, "vps", canaryPassword); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Errorf("not a regular file: %v", err)
	}
}

func TestAnnouncePostgresRoleWarnsWhenAnOlderFileCantBeRemoved(t *testing.T) {
	home := fakeHome(t)
	dir := bundleWithAuthConf(t)
	path := roleScriptPath(home)

	// A non-empty directory can neither be removed nor replaced by the
	// rename: the error must warn not to run what is there.
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := announcePostgresRole(&bytes.Buffer{}, dir, "vps", canaryPassword)
	if err == nil || !strings.Contains(err.Error(), "do NOT run it") || strings.Contains(err.Error(), "canary-PW") {
		t.Errorf("err = %v", err)
	}
}

// Every way of not writing a fresh script must also get rid of an older one.
func TestAnnouncePostgresRoleWithoutARoleNameRemovesAnOlderScript(t *testing.T) {
	home := fakeHome(t)
	path := roleScriptPath(home)
	if _, err := announcePostgresRole(&bytes.Buffer{}, bundleWithAuthConf(t), "vps", "old-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// A new OpenBao, and an auth.conf that names no Postgres user.
	var out bytes.Buffer
	if _, err := announcePostgresRole(&out, t.TempDir(), "vps", canaryPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the older script, which sets the old password, is still there: %v", err)
	}
}

func TestAnnouncePostgresRoleTightensAnOlderFilesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes on Windows")
	}
	home := fakeHome(t)
	path := roleScriptPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := announcePostgresRole(&bytes.Buffer{}, bundleWithAuthConf(t), "vps", canaryPassword); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, err %v", info.Mode().Perm(), err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/.versola/openbao/vps-postgres-role.sql": "/home/u/.versola/openbao/vps-postgres-role.sql",
		"/home/John Smith/x":                             "'/home/John Smith/x'",
		`/h/a'b\"$c!`:                                    `'/h/a'\''b\"$c!'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPrintOpenBaoInfoNamesTheFileNotTheValues(t *testing.T) {
	var out bytes.Buffer
	printOpenBaoInfo(&out, "vps", &openbao.Credentials{Address: "http://localhost:8200", RoleID: "canary-role-id", SecretID: "canary-secret-id"})
	if strings.Contains(out.String(), "canary-") {
		t.Fatalf("credentials reached the terminal:\n%s", out.String())
	}
	for _, want := range []string{"http://localhost:8200", "~/.versola/openbao/vps.json"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
