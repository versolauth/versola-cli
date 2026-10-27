package deploy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestResolvePostgres(t *testing.T) {
	external := &state.State{Target: "vps", PostgresHost: "db.example.com:5432"}
	bundled := &state.State{Target: "vps", PostgresHost: BundledPostgresHost, PostgresMode: PostgresModeBundled}
	local := &state.State{Target: "local"}

	cases := []struct {
		name     string
		target   string
		host     string
		stored   *state.State
		staged   *stagedConfigure
		wantMode string
		wantHost string
		wantErr  string
	}{
		{name: "local ignores everything", target: "local", host: "x:1", stored: bundled, wantHost: "x:1"},
		{name: "a first vps install runs its own", target: "vps", wantMode: PostgresModeBundled, wantHost: BundledPostgresHost},
		{name: "a first vps install with a host is external", target: "vps", host: "db.example.com:5432", wantHost: "db.example.com:5432"},
		{name: "a local record is no vps deployment", target: "vps", stored: local, wantMode: PostgresModeBundled, wantHost: BundledPostgresHost},
		{name: "no flag keeps an external one at its host", target: "vps", stored: external, wantHost: "db.example.com:5432"},
		{name: "a flag over an external one wins", target: "vps", host: "other:5432", stored: external, wantHost: "other:5432"},
		{name: "no flag keeps its own", target: "vps", stored: bundled, wantMode: PostgresModeBundled, wantHost: BundledPostgresHost},
		{name: "an old record without a host is not guessed", target: "vps", stored: &state.State{Target: "vps"}, wantErr: "does not say which Postgres"},
		{name: "an old record with a flag is external", target: "vps", host: "db.example.com:5432", stored: &state.State{Target: "vps"}, wantHost: "db.example.com:5432"},
		{name: "a flag over its own is refused", target: "vps", host: "db.example.com:5432", stored: bundled, wantErr: "backup and a restore"},
		{name: "even the same address is refused", target: "vps", host: BundledPostgresHost, stored: bundled, wantErr: "backup and a restore"},
		{name: "a rollout keeps its own whatever it is told", target: "vps", host: "x:1", stored: bundled, staged: &stagedConfigure{from: bundled}, wantMode: PostgresModeBundled, wantHost: BundledPostgresHost},
		{name: "a rollout keeps an external one", target: "vps", host: "db.example.com:5432", stored: external, staged: &stagedConfigure{from: external}, wantHost: "db.example.com:5432"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, host, err := resolvePostgres(c.target, c.host, c.stored, c.staged)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != c.wantMode || host != c.wantHost {
				t.Errorf("got (%q, %q), want (%q, %q)", mode, host, c.wantMode, c.wantHost)
			}
		})
	}
}

func TestPostgresOptionsMode(t *testing.T) {
	bundled := PostgresOptions{Mode: PostgresModeBundled}
	if got := strings.Join(bundled.toolsArgs(), " "); got != "-e POSTGRES_MODE=bundled" {
		t.Errorf("toolsArgs = %q", got)
	}
	if got := (PostgresOptions{DB: "versola"}).toolsArgs(); strings.Contains(strings.Join(got, " "), "POSTGRES_MODE") {
		t.Errorf("an external Postgres passes no mode: %v", got)
	}
	// What a rolling upgrade runs versola-tools with comes from the record.
	if got := postgresOptionsOf(&state.State{Target: "vps", PostgresMode: PostgresModeBundled}); got.Mode != PostgresModeBundled {
		t.Errorf("postgresOptionsOf lost the mode: %+v", got)
	}

	if _, err := bundled.Validate("vps"); err != nil {
		t.Errorf("bundled alone is valid: %v", err)
	}
	if _, err := (PostgresOptions{Mode: PostgresModeBundled, CAFile: "ca.pem"}).Validate("vps"); err == nil || !strings.Contains(err.Error(), "--postgres-ca") {
		t.Errorf("a CA file with the container's Postgres must be refused, got %v", err)
	}
	if _, err := (PostgresOptions{Mode: "embedded"}).Validate("vps"); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

func TestRequireBundledPostgres(t *testing.T) {
	dir := t.TempDir()
	with := filepath.Join(dir, "with.yml")
	without := filepath.Join(dir, "without.yml")
	if err := os.WriteFile(with, []byte("services:\n  openbao:\n    image: x\n  postgres:\n    image: postgres:18.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(without, []byte("services:\n  openbao:\n    image: x\n  central:\n    environment:\n      postgres: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireBundledPostgres(with, "0.9.0", false); err != nil {
		t.Errorf("a compose file with the service is accepted: %v", err)
	}
	err := requireBundledPostgres(without, "0.9.0", false)
	if err == nil || !strings.Contains(err.Error(), "versola-tools 0.9.0 cannot run Postgres in a container") || !strings.Contains(err.Error(), "--postgres-host") {
		t.Errorf("an older release must be refused with what to do, got %v", err)
	}
	// A rollout cannot be given --postgres-host, so it is not suggested.
	err = requireBundledPostgres(without, "0.9.0", true)
	if err == nil || strings.Contains(err.Error(), "--postgres-host") || !strings.Contains(err.Error(), "Roll out to a release that can") {
		t.Errorf("a rollout must be told to roll out to another release, got %v", err)
	}
	if err := requireBundledPostgres(filepath.Join(dir, "missing.yml"), "0.9.0", false); err == nil {
		t.Error("a missing compose file is an error")
	}
}

func TestWritePostgresSecretsEnv(t *testing.T) {
	dir := t.TempDir()
	auth := "POSTGRES_PASSWORD=s3cret-value\nADMIN_BOOTSTRAP_PASSWORD=admin-value\nPASSWORDS_SECRET=other\n"
	if err := os.WriteFile(filepath.Join(dir, "auth.secrets.env"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePostgresSecretsEnv(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "postgres.secrets.env")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "POSTGRES_PASSWORD=s3cret-value\n" {
		t.Errorf("postgres.secrets.env = %q: it must hold the password and nothing else of auth's secrets", b)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestWritePostgresSecretsEnvWithoutPassword(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.secrets.env"), []byte("ADMIN_BOOTSTRAP_PASSWORD=admin-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writePostgresSecretsEnv(dir)
	if err == nil || !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "admin-value") {
		t.Errorf("the error leaks a secret value: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "postgres.secrets.env")); !os.IsNotExist(statErr) {
		t.Errorf("no file may be left behind: %v", statErr)
	}
}

func TestRefuseBundledDBChange(t *testing.T) {
	bundled := &state.State{Target: "vps", PostgresMode: PostgresModeBundled}
	named := &state.State{Target: "vps", PostgresMode: PostgresModeBundled, PostgresDB: "versola"}
	external := &state.State{Target: "vps", PostgresDB: "versola"}

	for _, ok := range []struct {
		name   string
		stored *state.State
		db     string
	}{
		{"no record", nil, "other"},
		{"no flag", bundled, ""},
		{"the default name spelled out", bundled, "auth"},
		{"the recorded name again", named, "versola"},
		{"an external one may rename (a different database of its own)", external, "other"},
	} {
		if err := refuseBundledDBChange(ok.stored, ok.db); err != nil {
			t.Errorf("%s: %v", ok.name, err)
		}
	}
	if err := refuseBundledDBChange(bundled, "versola"); err == nil || !strings.Contains(err.Error(), `"auth"`) {
		t.Errorf("a new name over the default must be refused, naming the current one: %v", err)
	}
	if err := refuseBundledDBChange(named, "auth"); err == nil || !strings.Contains(err.Error(), `"versola"`) {
		t.Errorf("a new name over a recorded one must be refused, naming the current one: %v", err)
	}
}

func TestForgottenPostgresPasswordNote(t *testing.T) {
	note := forgottenPostgresPasswordNote("versola_app", "/home/u/.versola/active")
	for _, want := range []string{PostgresVolumeName(), "versola_app", "versola-postgres-vps", "auth.secrets.env"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not mention %q:\n%s", want, note)
		}
	}
}
