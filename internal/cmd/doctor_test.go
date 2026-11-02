package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/proxy"
)

// doctorWith is a fresh doctor command with args parsed; the package's flag
// variables are back at their defaults after the test.
func doctorWith(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	t.Cleanup(func() { addDoctorFlags(&cobra.Command{}) })
	c := &cobra.Command{Use: "doctor", RunE: runDoctor}
	addDoctorFlags(c)
	if err := c.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigureLine(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "versola configure vps <version> --auth-url https://<your domain>"},
		{[]string{"--auth-url", "https://id.example.com"}, "versola configure vps <version> --auth-url https://id.example.com"},
		// --proxy given is written out, nginx too: it may override a recorded external.
		{[]string{"--auth-url", "https://id.example.com", "--proxy", "nginx"}, "versola configure vps <version> --auth-url https://id.example.com --proxy nginx"},
		{
			[]string{"--target", "vps", "--auth-url", "https://id.example.com", "--proxy", "external", "--postgres-host", "db.internal:5432", "--registry", "cr.yandex/crp1"},
			"versola configure vps <version> --auth-url https://id.example.com --proxy external --postgres-host db.internal:5432 --registry cr.yandex/crp1",
		},
	} {
		if got := configureLine(doctorWith(t, c.args...)); got != c.want {
			t.Errorf("%v:\n got %s\nwant %s", c.args, got, c.want)
		}
	}
}

func TestShellArg(t *testing.T) {
	if got := shellArg("https://id.example.com"); got != "https://id.example.com" {
		t.Errorf("plain: %s", got)
	}
	if got := shellArg("it's here"); got != `'it'\''s here'` {
		t.Errorf("quoted: %s", got)
	}
}

// A vps flag on local is refused before any check runs, not ignored.
func TestDoctorLocalRefusesVpsFlags(t *testing.T) {
	for _, flag := range []string{"--proxy=external", "--postgres-host=db:5432", "--registry=cr.yandex/x", "--auth-url=https://id.example.com"} {
		c := doctorWith(t, flag)
		err := runDoctor(c, nil)
		if err == nil || !strings.Contains(err.Error(), "only applies to --target vps") {
			t.Errorf("%s: %v", flag, err)
		}
	}
}

// Values configure would refuse are refused by doctor too, before any check.
func TestParseVpsInputs(t *testing.T) {
	if _, err := parseVpsInputs("", "apache", "", "", "", "", "", ""); err == nil || !strings.Contains(err.Error(), "--proxy") {
		t.Errorf("bad --proxy: %v", err)
	}
	if _, err := parseVpsInputs("", proxy.ModeNginx, "", "https://cr.yandex", "", "", "", ""); err == nil || !strings.Contains(err.Error(), "--registry") {
		t.Errorf("bad --registry: %v", err)
	}
	// nginx serves on 80/443 only: a port in the URL is configure's refusal.
	if _, err := parseVpsInputs("https://id.example.com:8443", proxy.ModeNginx, "", "", "", "", "", ""); err == nil {
		t.Errorf("port with nginx: no error")
	}
	in, err := parseVpsInputs("https://ID.example.com", proxy.ModeExternal, "db:5432", "", "", "", "", "")
	if err != nil || in.auth == nil || in.auth.Host != "id.example.com" || in.mode != proxy.ModeExternal || in.postgresHost != "db:5432" {
		t.Errorf("valid: %+v %v", in, err)
	}
	if in, err := parseVpsInputs("", proxy.ModeNginx, "", "", "", "", "", ""); err != nil || in.auth != nil {
		t.Errorf("no --auth-url: %+v %v", in, err)
	}
}

func TestLingerCheck(t *testing.T) {
	fakeRun(t, nil)
	if r, ok := lingerCheck("no"); !ok || !r.OK || !r.Warn || !strings.Contains(r.Detail, "sudo loginctl enable-linger ops") {
		t.Errorf("off: %v %+v", ok, r)
	}
	if r, ok := lingerCheck("yes"); !ok || !r.OK || r.Warn {
		t.Errorf("on: %v %+v", ok, r)
	}
	if _, ok := lingerCheck(""); ok {
		t.Errorf("unknown: a line")
	}
}

// A bad --central-url is refused with the other flags, before any check.
func TestParseVpsInputsCentralURL(t *testing.T) {
	if _, err := parseVpsInputs("https://id.example.com", proxy.ModeNginx, "", "", "ftp://central", "", "", ""); err == nil {
		t.Errorf("bad --central-url: no error")
	}
}

// On a redeploy, doctor takes what configure remembers: a deployment behind
// the host's own web server is checked on 2821, not on 80/443.
func TestRecordedVpsFlags(t *testing.T) {
	rec := &deploy.RecordedVps{AuthURL: "https://id.example.com", ProxyMode: proxy.ModeExternal}
	pub := deploy.PublicDomains{MTLS: "mtls.example.com"}
	none := func(string) bool { return false }

	fill := recordedVpsFlags(none, rec, pub)
	if fill["auth-url"] != "https://id.example.com" || fill["proxy"] != proxy.ModeExternal || fill["mtls-domain"] != "mtls.example.com" || len(fill) != 3 {
		t.Errorf("nothing given: %v", fill)
	}
	if got := describeRecorded(fill); got != "--auth-url https://id.example.com, --proxy external, --mtls-domain mtls.example.com" {
		t.Errorf("described: %s", got)
	}

	// A flag given wins, as in configure.
	given := func(name string) bool { return name == "proxy" || name == "auth-url" }
	if fill := recordedVpsFlags(given, rec, pub); len(fill) != 1 || fill["mtls-domain"] == "" {
		t.Errorf("flags given: %v", fill)
	}
	// Nothing recorded: nothing filled.
	if fill := recordedVpsFlags(none, nil, deploy.PublicDomains{}); len(fill) != 0 {
		t.Errorf("no record: %v", fill)
	}
}

// A recorded --auth-url needs no placeholder in the printed line.
func TestConfigureLineRecordedAuthURL(t *testing.T) {
	c := doctorWith(t, "--target", "vps")
	applyRecorded(map[string]string{"auth-url": "https://id.example.com", "proxy": proxy.ModeExternal})
	if got := configureLine(c); got != "versola configure vps <version>" {
		t.Errorf("got %s", got)
	}
}
