package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestVpsOptionsValidate(t *testing.T) {
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
		opts    VpsOptions
		wantErr string
	}{
		"env ok":             {"vps", VpsOptions{Env: "qa-2_x"}, ""},
		"env prod":           {"vps", VpsOptions{Env: "prod"}, ""},
		"env max length":     {"vps", VpsOptions{Env: strings.Repeat("a", 32)}, ""},
		"env too long":       {"vps", VpsOptions{Env: strings.Repeat("a", 33)}, "not a valid environment name"},
		"env upper case":     {"vps", VpsOptions{Env: "QA"}, "not a valid environment name"},
		"env with space":     {"vps", VpsOptions{Env: "q a"}, "not a valid environment name"},
		"env with dot":       {"vps", VpsOptions{Env: "q.a"}, "not a valid environment name"},
		"env with slash":     {"vps", VpsOptions{Env: "a/b"}, "not a valid environment name"},
		"env local":          {"local", VpsOptions{Env: "qa"}, "--env only applies to vps"},
		"nothing set, local": {"local", VpsOptions{}, ""},
		"nothing set, vps":   {"vps", VpsOptions{}, ""},
		"db ok":              {"vps", VpsOptions{DB: "versola_prod-1"}, ""},
		"db max length":      {"vps", VpsOptions{DB: strings.Repeat("a", 63)}, ""},
		"db too long":        {"vps", VpsOptions{DB: strings.Repeat("a", 64)}, "not a valid database name"},
		"db with space":      {"vps", VpsOptions{DB: "my db"}, "not a valid database name"},
		"db with quote":      {"vps", VpsOptions{DB: `a"b`}, "not a valid database name"},
		"db with dot":        {"vps", VpsOptions{DB: "a.b"}, "not a valid database name"},
		"db local":           {"local", VpsOptions{DB: "x"}, "--postgres-db only applies to vps"},
		"ca ok":              {"vps", VpsOptions{CAFile: good}, ""},
		"ca local":           {"local", VpsOptions{CAFile: good}, "--postgres-ca only applies to vps"},
		"ca missing":         {"vps", VpsOptions{CAFile: filepath.Join(dir, "nope.pem")}, "--postgres-ca:"},
		"ca is a directory":  {"vps", VpsOptions{CAFile: dir}, "not a regular file"},
		"ca not a PEM":       {"vps", VpsOptions{CAFile: notPEM}, "does not contain a PEM certificate"},
		"both ok":            {"vps", VpsOptions{DB: "versola", CAFile: good}, ""},
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

func TestVpsOptionsValidateMakesCAAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := VpsOptions{CAFile: "ca.pem"}.Validate("vps")
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir may sit behind a symlink (macOS /var), so compare by base.
	if !filepath.IsAbs(got.CAFile) || filepath.Base(got.CAFile) != "ca.pem" {
		t.Fatalf("CAFile = %q, want an absolute path to ca.pem", got.CAFile)
	}
}

func TestVpsOptionsToolsArgs(t *testing.T) {
	cases := map[string]struct {
		opts VpsOptions
		want []string
	}{
		"none":          {VpsOptions{}, []string{"-e", "ENV_NAME=prod"}},
		"env":           {VpsOptions{Env: "qa"}, []string{"-e", "ENV_NAME=qa"}},
		"explicit prod": {VpsOptions{Env: "prod"}, []string{"-e", "ENV_NAME=prod"}},
		"db":            {VpsOptions{DB: "versola"}, []string{"-e", "ENV_NAME=prod", "-e", "POSTGRES_DB=versola"}},
		"ca": {VpsOptions{CAFile: "/etc/ssl/db-ca.pem"},
			[]string{"-e", "ENV_NAME=prod", "-v", "/etc/ssl/db-ca.pem:/input/postgres-ca.crt:ro", "-e", "POSTGRES_CA_FILE=/input/postgres-ca.crt"}},
		"both": {VpsOptions{Env: "staging", DB: "versola", CAFile: "/ca.pem"},
			[]string{"-e", "ENV_NAME=staging", "-e", "POSTGRES_DB=versola", "-v", "/ca.pem:/input/postgres-ca.crt:ro", "-e", "POSTGRES_CA_FILE=/input/postgres-ca.crt"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.opts.toolsArgs(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("toolsArgs = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVpsOptionsStored(t *testing.T) {
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
			Env: "qa", PostgresDB: "versola", PostgresCAFile: ca}); err != nil {
			t.Fatal(err)
		}
	}
	record("vps")
	stored := filepath.Join(dir, state.PostgresCAFileName)

	cases := map[string]struct {
		target string
		in     VpsOptions
		want   VpsOptions
	}{
		"no flags keep stored": {"vps", VpsOptions{}, VpsOptions{Env: "qa", DB: "versola", CAFile: stored}},
		"env flag overrides":   {"vps", VpsOptions{Env: "staging"}, VpsOptions{Env: "staging", DB: "versola", CAFile: stored}},
		"explicit prod wins":   {"vps", VpsOptions{Env: "prod"}, VpsOptions{Env: "prod", DB: "versola", CAFile: stored}},
		"db flag overrides":    {"vps", VpsOptions{DB: "other"}, VpsOptions{Env: "qa", DB: "other", CAFile: stored}},
		"ca flag overrides":    {"vps", VpsOptions{CAFile: "/x/ca.pem"}, VpsOptions{Env: "qa", DB: "versola", CAFile: "/x/ca.pem"}},
		"local ignores stored": {"local", VpsOptions{}, VpsOptions{}},
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
	want := []string{"-e", "ENV_NAME=qa", "-e", "POSTGRES_DB=versola", "-v", stored + ":/input/postgres-ca.crt:ro", "-e", "POSTGRES_CA_FILE=/input/postgres-ca.crt"}
	if got := vpsOptionsOf(st).toolsArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("toolsArgs = %v, want %v", got, want)
	}
}

func TestTestModeWarning(t *testing.T) {
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
	record := func(env string) {
		t.Helper()
		if err := state.Finalize(state.FinalizeOptions{Target: "vps", Version: "1.0.0", BundleDir: bundle,
			AuthURL: "https://id.example.com", ProxyMode: "external", PostgresHost: "db:5432", Env: env}); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]struct {
		stored string
		target string
		opts   VpsOptions
		want   string // substring; "" means no warning
	}{
		"prod by default":       {"", "vps", VpsOptions{}, ""},
		"explicit prod":         {"", "vps", VpsOptions{Env: "prod"}, ""},
		"flag gives test mode":  {"", "vps", VpsOptions{Env: "qa"}, `"qa"`},
		"stored test mode kept": {"qa", "vps", VpsOptions{}, `"qa"`},
		"explicit prod resets":  {"qa", "vps", VpsOptions{Env: "prod"}, ""},
		"local never warns":     {"qa", "local", VpsOptions{}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			record(c.stored)
			got := TestModeWarning(c.target, c.opts)
			if c.want == "" {
				if got != "" {
					t.Fatalf("got warning %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, c.want) || !strings.Contains(got, "123456") {
				t.Fatalf("warning %q lacks %q or the OTP note", got, c.want)
			}
		})
	}
}
