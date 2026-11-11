package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// newVpsFlagsCmd registers the five remembered flags with the defaults
// configure and bootstrap give them (TestRememberedVpsFlagsOnConfigureAndBootstrap
// checks those).
func newVpsFlagsCmd(args ...string) *cobra.Command {
	c := &cobra.Command{Use: "x"}
	c.Flags().String("auth-url", "", "")
	c.Flags().String("postgres-host", "", "")
	c.Flags().String("proxy", proxy.ModeNginx, "")
	c.Flags().Bool("acme-staging", false, "")
	c.Flags().Bool("setup-openbao", false, "")
	if err := c.Flags().Parse(args); err != nil {
		panic(err)
	}
	return c
}

// provisionedOpenBao writes the admin credentials file a CLI that provisioned
// vps's OpenBao keeps (dummy values): its absence means "set up by hand".
func provisionedOpenBao(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".versola", "openbao")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vps-admin.json"), []byte(`{"rootToken":"t","unsealKey":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stagingBundle writes the bundle the record points at (bundles/b1) with a
// proxy configuration that gets its certificate from Let's Encrypt staging.
func stagingBundle(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".versola", "active", "bundles", "b1", "proxy", "conf.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "versola.conf"), []byte("acme_directory "+proxy.ACMEStaging+";\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const recordedExternal = `{"schemaVersion":4,"target":"vps","version":"0.8.0","bundleDir":"bundles/b1",
	"authUrl":"https://id.example.com","proxyMode":"external","postgresHost":"db.example.com:5432"}`

func TestResolveVpsFlagsWithoutRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	_, err := resolveVpsFlags(newVpsFlagsCmd(), "vps")
	if err == nil || err.Error() != "--auth-url is required for vps deployments (e.g. --auth-url https://id.example.com)" {
		t.Fatalf("no record, no --auth-url: %v", err)
	}
	// Given: taken as is, the flag defaults for the rest.
	v, err := resolveVpsFlags(newVpsFlagsCmd("--auth-url", "https://id.example.com"), "vps")
	if err != nil || v.AuthURL != "https://id.example.com" || v.Proxy != proxy.ModeNginx || v.ACMEStaging || v.SetupOpenBao || v.recorded != nil {
		t.Fatalf("= %+v, %v", v, err)
	}
	// A record from a newer versola: its own error, not "--auth-url is required".
	writeVpsState(t, `{"schemaVersion":99,"target":"vps","version":"9.0.0","authUrl":"https://id.example.com","proxyMode":"nginx"}`)
	if _, err := resolveVpsFlags(newVpsFlagsCmd(), "vps"); !errors.Is(err, state.ErrNewerSchema) {
		t.Fatalf("newer record: %v", err)
	}
	// A local record gives vps nothing.
	writeVpsState(t, `{"schemaVersion":4,"target":"local","version":"0.8.0","authUrl":"http://localhost:2821","proxyMode":"local"}`)
	if _, err := resolveVpsFlags(newVpsFlagsCmd(), "vps"); err == nil || !strings.Contains(err.Error(), "--auth-url is required") {
		t.Fatalf("local record: %v", err)
	}
}

func TestResolveVpsFlagsFromRecord(t *testing.T) {
	writeVpsState(t, recordedExternal)
	provisionedOpenBao(t)
	v, err := resolveVpsFlags(newVpsFlagsCmd(), "vps")
	if err != nil {
		t.Fatal(err)
	}
	if v.AuthURL != "https://id.example.com" || v.Proxy != proxy.ModeExternal || v.ACMEStaging || v.SetupOpenBao || v.PostgresHost != "" {
		t.Fatalf("no flags: %+v", v)
	}
	if v.recorded == nil || v.recorded.PostgresText() != "your own, at db.example.com:5432" {
		t.Fatalf("recorded = %+v", v.recorded)
	}
	// Local ignores the record entirely.
	if v, err := resolveVpsFlags(newVpsFlagsCmd(), "local"); err != nil || v.AuthURL != "" || v.Proxy != proxy.ModeNginx {
		t.Fatalf("local: %+v, %v", v, err)
	}
}

func TestResolveVpsFlagsOverrides(t *testing.T) {
	writeVpsState(t, `{"schemaVersion":5,"target":"vps","version":"0.8.0","bundleDir":"bundles/b1",
		"authUrl":"https://id.example.com","proxyMode":"nginx","postgresMode":"bundled","postgresHost":"127.0.0.1:5432"}`)
	stagingBundle(t)
	// No admin credentials: OpenBao was set up by hand.

	v, err := resolveVpsFlags(newVpsFlagsCmd(), "vps")
	if err != nil || !v.ACMEStaging || !v.SetupOpenBao || v.Proxy != proxy.ModeNginx {
		t.Fatalf("no flags: %+v, %v", v, err)
	}
	cases := []struct {
		args  []string
		check func(vpsFlags) bool
	}{
		{[]string{"--auth-url", "https://login.example.com"}, func(v vpsFlags) bool { return v.AuthURL == "https://login.example.com" && v.ACMEStaging }},
		{[]string{"--acme-staging=false"}, func(v vpsFlags) bool { return !v.ACMEStaging && v.given["acme-staging"] }},
		{[]string{"--setup-openbao=false"}, func(v vpsFlags) bool { return !v.SetupOpenBao }},
		{[]string{"--postgres-host", "db:5432"}, func(v vpsFlags) bool { return v.PostgresHost == "db:5432" }},
		// The recorded staging no longer applies: left out, not an error.
		{[]string{"--proxy", "external"}, func(v vpsFlags) bool { return v.Proxy == proxy.ModeExternal && !v.ACMEStaging && v.droppedStaging }},
		{[]string{"--auth-url", "http://1.2.3.4"}, func(v vpsFlags) bool { return !v.ACMEStaging && v.droppedStaging }},
		// Given explicitly, it is kept (deploy.Configure refuses it).
		{[]string{"--proxy", "external", "--acme-staging"}, func(v vpsFlags) bool { return v.ACMEStaging && !v.droppedStaging }},
	}
	for _, c := range cases {
		v, err := resolveVpsFlags(newVpsFlagsCmd(c.args...), "vps")
		if err != nil || !c.check(v) {
			t.Errorf("%v: %+v, %v", c.args, v, err)
		}
	}
}

func TestPrintVpsSettings(t *testing.T) {
	writeVpsState(t, `{"schemaVersion":5,"target":"vps","version":"0.8.0","bundleDir":"bundles/b1",
		"authUrl":"https://id.example.com","proxyMode":"nginx","postgresMode":"bundled","postgresHost":"127.0.0.1:5432"}`)
	stagingBundle(t)
	v, err := resolveVpsFlags(newVpsFlagsCmd("--auth-url", "https://login.example.com"), "vps")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printVpsSettings(&b, v)
	want := `
Using the recorded settings of this deployment (pass a flag to change one):
  --auth-url       https://login.example.com  (given now)
  --proxy          nginx
  Postgres         in a container of this deployment, on 127.0.0.1:5432
  --acme-staging   on: the certificate comes from Let's Encrypt staging (untrusted)
  --setup-openbao  on: OpenBao is set up by hand, credentials from "versola secrets login vps"
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}

	// The recorded staging left out, and nothing printed on a first deployment.
	v, _ = resolveVpsFlags(newVpsFlagsCmd("--proxy", "external"), "vps")
	b.Reset()
	printVpsSettings(&b, v)
	if !strings.Contains(b.String(), "--acme-staging   not kept: it only applies to --proxy nginx with an https --auth-url") {
		t.Errorf("dropped staging not shown:\n%s", b.String())
	}
	b.Reset()
	printVpsSettings(&b, vpsFlags{AuthURL: "https://id.example.com", Proxy: proxy.ModeNginx})
	if b.Len() != 0 {
		t.Errorf("first deployment printed:\n%s", b.String())
	}
}

// The settings come before the question, so the answer is given with them on
// screen.
func TestConfirmVpsRunPrintsSettingsFirst(t *testing.T) {
	writeVpsState(t, recordedExternal)
	provisionedOpenBao(t)
	v, err := resolveVpsFlags(newVpsFlagsCmd(), "vps")
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = confirmVpsRun(w, v, true, "redeploy")
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	settings, question := strings.Index(s, "Using the recorded settings"), strings.Index(s, "This will redeploy.")
	if settings < 0 || question < 0 || settings > question {
		t.Fatalf("settings at %d, question at %d:\n%s", settings, question, s)
	}
}

func TestRememberedVpsFlagsOnConfigureAndBootstrap(t *testing.T) {
	for _, c := range []*cobra.Command{configureCmd, bootstrapCmd} {
		for _, name := range rememberedVpsFlags {
			f := c.Flags().Lookup(name)
			if f == nil {
				t.Errorf("%s has no --%s", c.Name(), name)
				continue
			}
			if !strings.Contains(strings.ToLower(f.Usage), "remembered") {
				t.Errorf("%s --%s does not say it is remembered: %s", c.Name(), name, f.Usage)
			}
		}
		if f := c.Flags().Lookup("proxy"); f.DefValue != proxy.ModeNginx {
			t.Errorf("%s --proxy defaults to %q", c.Name(), f.DefValue)
		}
	}
}
