package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestPostgresOptionsValidate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("ca.pem", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	notPEM := write("bad.pem", "hello")

	cases := map[string]struct {
		target  string
		opts    PostgresOptions
		wantErr string
	}{
		"nothing set, local": {"local", PostgresOptions{}, ""},
		"nothing set, vps":   {"vps", PostgresOptions{}, ""},
		"db ok":              {"vps", PostgresOptions{DB: "versola_prod-1"}, ""},
		"db max length":      {"vps", PostgresOptions{DB: strings.Repeat("a", 63)}, ""},
		"db too long":        {"vps", PostgresOptions{DB: strings.Repeat("a", 64)}, "not a valid database name"},
		"db with space":      {"vps", PostgresOptions{DB: "my db"}, "not a valid database name"},
		"db with quote":      {"vps", PostgresOptions{DB: `a"b`}, "not a valid database name"},
		"db with dot":        {"vps", PostgresOptions{DB: "a.b"}, "not a valid database name"},
		"db local":           {"local", PostgresOptions{DB: "x"}, "--postgres-db only applies to vps"},
		"ca ok":              {"vps", PostgresOptions{CAFile: good}, ""},
		"ca local":           {"local", PostgresOptions{CAFile: good}, "--postgres-ca only applies to vps"},
		"ca missing":         {"vps", PostgresOptions{CAFile: filepath.Join(dir, "nope.pem")}, "--postgres-ca:"},
		"ca is a directory":  {"vps", PostgresOptions{CAFile: dir}, "not a regular file"},
		"ca not a PEM":       {"vps", PostgresOptions{CAFile: notPEM}, "does not contain a PEM certificate"},
		"both ok":            {"vps", PostgresOptions{DB: "versola", CAFile: good}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := c.opts.Validate(c.target)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.CAFile != "" && !filepath.IsAbs(got.CAFile) {
					t.Fatalf("CAFile not absolute: %q", got.CAFile)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestPostgresOptionsValidateMakesCAAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := PostgresOptions{CAFile: "ca.pem"}.Validate("vps")
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir may sit behind a symlink (macOS /var), so compare by base.
	if !filepath.IsAbs(got.CAFile) || filepath.Base(got.CAFile) != "ca.pem" {
		t.Fatalf("CAFile = %q, want an absolute path to ca.pem", got.CAFile)
	}
}

func TestPostgresOptionsToolsArgs(t *testing.T) {
	cases := map[string]struct {
		opts PostgresOptions
		want []string
	}{
		"none": {PostgresOptions{}, nil},
		"db":   {PostgresOptions{DB: "versola"}, []string{"-e", "POSTGRES_DB=versola"}},
		"ca": {PostgresOptions{CAFile: "/etc/ssl/db-ca.pem"},
			[]string{"-v", "/etc/ssl/db-ca.pem:/input/postgres-ca.crt:ro", "-e", "POSTGRES_CA_FILE=/input/postgres-ca.crt"}},
		"both": {PostgresOptions{DB: "versola", CAFile: "/ca.pem"},
			[]string{"-e", "POSTGRES_DB=versola", "-v", "/ca.pem:/input/postgres-ca.crt:ro", "-e", "POSTGRES_CA_FILE=/input/postgres-ca.crt"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.opts.toolsArgs(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("toolsArgs = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPostgresOptionsStored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle-1")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := func(target string) {
		t.Helper()
		if err := state.Finalize(state.FinalizeOptions{Target: target, Version: "1.0.0", BundleDir: bundle,
			AuthURL: "https://id.example.com", ProxyMode: "external", PostgresHost: "db:5432",
			PostgresDB: "versola", PostgresCAFile: ca}); err != nil {
			t.Fatal(err)
		}
	}
	record("vps")
	stored := filepath.Join(dir, state.PostgresCAFileName)

	cases := map[string]struct {
		target string
		in     PostgresOptions
		want   PostgresOptions
	}{
		"no flags keep stored": {"vps", PostgresOptions{}, PostgresOptions{DB: "versola", CAFile: stored}},
		"db flag overrides":    {"vps", PostgresOptions{DB: "other"}, PostgresOptions{DB: "other", CAFile: stored}},
		"ca flag overrides":    {"vps", PostgresOptions{CAFile: "/x/ca.pem"}, PostgresOptions{DB: "versola", CAFile: "/x/ca.pem"}},
		"local ignores stored": {"local", PostgresOptions{}, PostgresOptions{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.in.withStored(c.target); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}

	// What rollout and secrets plan hand to versola-tools.
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-e", "POSTGRES_DB=versola", "-v", stored + ":/input/postgres-ca.crt:ro", "-e", "POSTGRES_CA_FILE=/input/postgres-ca.crt"}
	if got := postgresOptionsOf(st).toolsArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("toolsArgs = %v, want %v", got, want)
	}
}
