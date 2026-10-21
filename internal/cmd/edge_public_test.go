package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
)

func TestPublicFlagsOnConfigureBootstrapAndDoctor(t *testing.T) {
	for _, c := range []*cobra.Command{configureCmd, bootstrapCmd} {
		for _, name := range publicFlagNames {
			f := c.Flags().Lookup(name)
			if f == nil {
				t.Errorf("%s has no --%s", c.Name(), name)
				continue
			}
			// Opt-in: with no flag nothing is published.
			if f.DefValue != "" {
				t.Errorf("%s --%s defaults to %q", c.Name(), name, f.DefValue)
			}
			if !strings.Contains(f.Usage, "vps only") {
				t.Errorf("%s --%s does not say it is vps only: %s", c.Name(), name, f.Usage)
			}
		}
	}
	for _, name := range publicFlagNames {
		if doctorCmd.Flags().Lookup(name) == nil {
			t.Errorf("doctor has no --%s", name)
		}
	}
	if f := bootstrapCmd.Flags().Lookup("no-certs-timer"); f == nil || f.DefValue != "false" {
		t.Errorf("--no-certs-timer = %+v: the timer is part of bootstrap vps by default", f)
	}
	if f := certsRenewCmd.Flags().Lookup("pin-wait"); f == nil || f.DefValue != deploy.DefaultPinWait.String() {
		t.Errorf("--pin-wait = %+v", f)
	}
}

func TestRejectVpsOnlyFlagsCoversPublicNames(t *testing.T) {
	for _, name := range publicFlagNames {
		c := &cobra.Command{Use: "x"}
		addPublicFlags(c)
		if err := rejectVpsOnlyFlags(c, "local"); err != nil {
			t.Fatalf("no flag passed: %v", err)
		}
		if err := c.Flags().Set(name, "x.example.com"); err != nil {
			t.Fatal(err)
		}
		if err := rejectVpsOnlyFlags(c, "local"); err == nil || !strings.Contains(err.Error(), "--"+name+" only applies to vps") {
			t.Errorf("--%s on local: %v", name, err)
		}
		if err := rejectVpsOnlyFlags(c, "vps"); err != nil {
			t.Errorf("--%s on vps: %v", name, err)
		}
	}
}

// writeVpsState records a vps deployment in a fresh HOME.
func writeVpsState(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".versola", "active")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDomainsKeepsOverridesAndClears(t *testing.T) {
	fresh := func() *cobra.Command {
		c := &cobra.Command{Use: "x"}
		addPublicFlags(c)
		return c
	}
	// Nothing recorded, no flag: nothing is published.
	t.Setenv("HOME", t.TempDir())
	if p, err := publicDomains(fresh(), "vps"); err != nil || p != (deploy.PublicDomains{}) {
		t.Fatalf("= %+v, %v", p, err)
	}

	writeVpsState(t, `{"schemaVersion":4,"target":"vps","version":"0.8.0","authUrl":"https://id.example.com","proxyMode":"nginx",
		"mtlsDomain":"id-mtls.example.com","centralMtlsDomain":"central-mtls.example.com","centralDomain":"central.example.com"}`)
	stored := deploy.PublicDomains{MTLS: "id-mtls.example.com", CentralMTLS: "central-mtls.example.com", Enroll: "central.example.com"}

	// No flag keeps what was configured (rollout reconfigures from the record the same way).
	if p, err := publicDomains(fresh(), "vps"); err != nil || p != stored {
		t.Errorf("no flags: %+v, %v", p, err)
	}
	// A flag overrides its name only.
	c := fresh()
	_ = c.Flags().Set("mtls-domain", "other-mtls.example.com")
	want := stored
	want.MTLS = "other-mtls.example.com"
	if p, _ := publicDomains(c, "vps"); p != want {
		t.Errorf("override: %+v, want %+v", p, want)
	}
	// An empty value is "stop publishing it", not "not given".
	c = fresh()
	_ = c.Flags().Set("central-domain", "")
	want = stored
	want.Enroll = ""
	if p, _ := publicDomains(c, "vps"); p != want {
		t.Errorf("clear: %+v, want %+v", p, want)
	}
	// local publishes nothing, whatever is recorded for vps.
	if p, err := publicDomains(fresh(), "local"); err != nil || p != (deploy.PublicDomains{}) {
		t.Errorf("local: %+v, %v", p, err)
	}
}

func TestPublicNameChecksNeedTheAuthURLAndValidNames(t *testing.T) {
	if r, err := publicNameChecks("", "", "", ""); err != nil || r != nil {
		t.Errorf("no names: %v, %v", r, err)
	}
	if _, err := publicNameChecks("", "id-mtls.example.com", "", ""); err == nil || !strings.Contains(err.Error(), "--auth-url") {
		t.Errorf("without --auth-url: %v", err)
	}
	for _, tc := range []struct{ mtls, central, enroll string }{
		{"https://id-mtls.example.com", "", ""},
		{"", "203.0.113.7", ""},
		{"id.example.com", "", ""}, // the auth URL's own name
		{"x.example.com", "x.example.com", ""},
	} {
		if _, err := publicNameChecks("https://id.example.com", tc.mtls, tc.central, tc.enroll); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}

type edgeAddFake struct {
	opts deploy.EdgeAddOptions
	res  deploy.EdgeAddResult
	err  error
}

func (f *edgeAddFake) do(o deploy.EdgeAddOptions) (deploy.EdgeAddResult, error) {
	f.opts = o
	return f.res, f.err
}

func fakeResult() deploy.EdgeAddResult {
	return deploy.EdgeAddResult{
		KeyID: "k1", PrivateKey: "PRIVATE-KEY-VALUE",
		Bundle: deploy.BootstrapBundle{
			Version: deploy.BundleVersion, EdgeID: "eu", Group: "grp", EnrollmentToken: "TOKEN-VALUE", TokenExpiresAt: time.Now().Add(time.Hour),
			EnrollURL: "https://central.example.com", CentralURL: "https://central-mtls.example.com",
		},
	}
}

// The registration's key goes to a file of its own, mode 0600, and never to
// stdout (which is the bundle, and travels); the bundle never carries the key,
// and the token is on stdout alone, not in what is for the operator to read.
func TestEdgeAddWritesBundleToStdoutAndKeyToFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "eu.key.json")
	f := &edgeAddFake{res: fakeResult()}
	var stdout, stderr bytes.Buffer
	if err := edgeAdd(&stdout, &stderr, "eu", "grp", false, "", keyPath, f.do); err != nil {
		t.Fatal(err)
	}
	if f.opts != (deploy.EdgeAddOptions{ID: "eu", Group: "grp"}) {
		t.Errorf("options = %+v", f.opts)
	}
	if !strings.Contains(stdout.String(), "TOKEN-VALUE") || strings.Contains(stdout.String(), "PRIVATE-KEY-VALUE") {
		t.Errorf("stdout = %s", stdout.String())
	}
	if strings.Contains(stderr.String(), "TOKEN-VALUE") || strings.Contains(stderr.String(), "PRIVATE-KEY-VALUE") {
		t.Errorf("a secret on stderr: %s", stderr.String())
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || !strings.Contains(string(key), "PRIVATE-KEY-VALUE") || !strings.Contains(string(key), `"edgeId": "eu"`) {
		t.Fatalf("key file = %q, %v", key, err)
	}
	if st, _ := os.Stat(keyPath); st.Mode().Perm() != 0o600 && !isWindows() {
		t.Errorf("key file mode = %v", st.Mode())
	}
	for _, w := range []string{"Wrote the edge's key", "group grp", "central.example.com", "Fingerprints"} {
		if !strings.Contains(stderr.String(), w) {
			t.Errorf("stderr lacks %q:\n%s", w, stderr.String())
		}
	}
}

func TestEdgeAddToFileAndRefusals(t *testing.T) {
	dir := t.TempDir()
	out, keyPath := filepath.Join(dir, "bundle.json"), filepath.Join(dir, "key.json")
	f := &edgeAddFake{res: fakeResult()}
	var stdout, stderr bytes.Buffer
	if err := edgeAdd(&stdout, &stderr, "eu", "", false, out, keyPath, f.do); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q with -o", stdout.String())
	}
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), "TOKEN-VALUE") {
		t.Errorf("bundle file = %q, %v", b, err)
	}
	// Neither file is overwritten, and nothing is registered for a request that cannot be kept.
	f2 := &edgeAddFake{res: fakeResult()}
	if err := edgeAdd(&stdout, &stderr, "eu", "", false, "", keyPath, f2.do); err == nil || !strings.Contains(err.Error(), "exists") || f2.opts.ID != "" {
		t.Errorf("an existing key file: %v (central called: %v)", err, f2.opts)
	}
	if err := edgeAdd(&stdout, &stderr, "eu", "", true, out, keyPath, f2.do); err == nil || f2.opts.ID != "" {
		t.Errorf("an existing bundle file: %v", err)
	}
	// An invalid id never reaches central.
	if err := edgeAdd(&stdout, &stderr, "Eu Central", "", false, "", keyPath+"2", f2.do); err == nil || f2.opts.ID != "" {
		t.Errorf("a bad id: %v", err)
	}
	// An error from central comes back as it is, with no files left behind.
	f3 := &edgeAddFake{err: errors.New("central refused")}
	k3 := filepath.Join(dir, "k3.json")
	if err := edgeAdd(&stdout, &stderr, "eu2", "", false, "", k3, f3.do); err == nil || !strings.Contains(err.Error(), "central refused") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(k3); !os.IsNotExist(err) {
		t.Errorf("a key file after a failed registration: %v", err)
	}
}

func TestEdgeAddExistingWritesNoKey(t *testing.T) {
	dir := t.TempDir()
	res := fakeResult()
	res.KeyID, res.PrivateKey = "", ""
	f := &edgeAddFake{res: res}
	keyPath := filepath.Join(dir, "key.json")
	var stdout, stderr bytes.Buffer
	if err := edgeAdd(&stdout, &stderr, "eu", "", true, "", keyPath, f.do); err != nil {
		t.Fatal(err)
	}
	if !f.opts.Existing {
		t.Error("--existing was not passed on")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("a key file for an existing edge: %v", err)
	}
	// Re-issuing a token needs no free key path: the file is not written.
	if err := os.WriteFile(keyPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := edgeAdd(&stdout, &stderr, "eu", "", true, "", keyPath, f.do); err != nil {
		t.Errorf("--existing with a key file already there: %v", err)
	}
}

func TestEdgeCommandsAreWired(t *testing.T) {
	sub := map[string]*cobra.Command{}
	for _, c := range edgeCmd.Commands() {
		sub[c.Name()] = c
	}
	if sub["add"] == nil || sub["bootstrap"] == nil {
		t.Fatalf("subcommands = %v", sub)
	}
	found := false
	for _, c := range rootCmd.Commands() {
		found = found || c == edgeCmd
	}
	if !found {
		t.Error("`edge` is not on the root command")
	}
	for _, name := range []string{"group", "existing", "output", "key-out"} {
		if sub["add"].Flags().Lookup(name) == nil {
			t.Errorf("edge add has no --%s", name)
		}
	}
	for _, name := range []string{"dir", "key-file", "version", "no-start"} {
		if sub["bootstrap"].Flags().Lookup(name) == nil {
			t.Errorf("edge bootstrap has no --%s", name)
		}
	}
	if err := sub["add"].Args(sub["add"], nil); err == nil {
		t.Error("edge add without an id")
	}
	if err := sub["bootstrap"].Args(sub["bootstrap"], []string{"a", "b"}); err == nil {
		t.Error("edge bootstrap with two files")
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }

// A token that fails after the registration must not lose the key central showed once.
func TestEdgeAddWritesKeyEvenWhenTheTokenFails(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "k.json")
	f := &edgeAddFake{res: deploy.EdgeAddResult{KeyID: "k1", PrivateKey: "PRIVATE-KEY-VALUE"}, err: errors.New("central answered 500")}
	var stdout, stderr bytes.Buffer
	err := edgeAdd(&stdout, &stderr, "eu", "", false, "", keyPath, f.do)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	if b, rerr := os.ReadFile(keyPath); rerr != nil || !strings.Contains(string(b), "PRIVATE-KEY-VALUE") {
		t.Errorf("key file = %q, %v", b, rerr)
	}
}
