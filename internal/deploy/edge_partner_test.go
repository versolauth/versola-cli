package deploy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sampleToken is the token of the PR description: its address is
// https://enroll.northbank.example.
const sampleToken = "vtk1.aHR0cHM6Ly9lbnJvbGwubm9ydGhiYW5rLmV4YW1wbGU.secret"

func tokenFor(address, secret string) string {
	return "vtk1." + base64.RawURLEncoding.EncodeToString([]byte(address)) + "." + secret
}

func TestParseEnrollmentToken(t *testing.T) {
	const secret = "s3cr3t-VALUE_xyz"
	for _, tc := range []struct {
		name, raw, address, err string
	}{
		{"the sample", sampleToken, "https://enroll.northbank.example", ""},
		{"with a newline and padding", tokenFor("https://central.example.com", secret)[:5] + base64.URLEncoding.EncodeToString([]byte("https://central.example.com")) + "." + secret + "\n", "https://central.example.com", ""},
		{"a path is central's to choose", tokenFor("https://example.com/central", secret), "https://example.com/central", ""},
		{"empty", "  \n", "", "it is empty"},
		{"not vtk1", "vtk2." + secret, "", "does not start"},
		{"a bundle", `{"enrollmentToken": "` + secret + `"}`, "", "does not start"},
		{"no secret", "vtk1." + base64.RawURLEncoding.EncodeToString([]byte("https://c.example.com")) + ".", "", "malformed"},
		{"four parts", tokenFor("https://c.example.com", secret) + ".x", "", "malformed"},
		{"two tokens", tokenFor("https://c.example.com", secret) + "\n" + tokenFor("https://c.example.com", secret), "", "more than one"},
		{"garbage address", "vtk1.!!!." + secret, "", "not base64url"},
		{"http address", tokenFor("http://enroll.example.com", secret), "", `"http://enroll.example.com", is not an https URL`},
		{"no host", tokenFor("https://", secret), "", "not an https URL"},
		{"user info", tokenFor("https://u:p@enroll.example.com", secret), "", "not an https URL"},
		{"not a URL", tokenFor("enroll.example.com", secret), "", "not an https URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEnrollmentToken(tc.raw)
			if tc.err == "" {
				if err != nil || got != tc.address {
					t.Fatalf("= %q, %v; want %q", got, err, tc.address)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error shows the token's secret: %v", err)
			}
		})
	}
}

func TestNormalizeAdmin(t *testing.T) {
	for _, tc := range []struct{ raw, want, err string }{
		{"+77010000000", "+77010000000", ""},
		{" +7 (701) 000-00-00 ", "+77010000000", ""},
		{"+1234567", "+1234567", ""},
		{"IT@AcmePay.Example", "it@acmepay.example", ""},
		{`o"neil@acmepay.example`, `o"neil@acmepay.example`, ""},
		{"", "", "blank"},
		{"   ", "", "blank"},
		{"77010000000", "", "international format"},
		{"+07010000000", "", "international format"},
		{"+123456", "", "international format"},
		{"+1234567890123456", "", "international format"},
		{"it@acmepay", "", "not an email"},
		{"it@@acmepay.example", "", "not an email"},
		{"it@acme\x00pay.example", "", "control character"},
	} {
		got, err := normalizeAdmin(tc.raw)
		if tc.err == "" {
			if err != nil || got != tc.want {
				t.Errorf("normalizeAdmin(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("normalizeAdmin(%q) err = %v, want %q", tc.raw, err, tc.err)
		}
	}
}

// testCACertPEM is a throwaway self-signed CA.
func testCACertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Deployment CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadLeafPins(t *testing.T) {
	leaf, next := testCertPEM(t, "central"), testCertPEM(t, "central-next")
	both := writeTemp(t, "both.crt", leaf+next)
	pins, err := readLeafPins("--central-pin", []string{writeTemp(t, "a.crt", leaf), both})
	if err != nil || len(pins) != 2 {
		t.Fatalf("= %d pins, %v; want 2 (the same certificate once)", len(pins), err)
	}
	for _, tc := range []struct {
		name  string
		files []string
		err   string
	}{
		{"none", nil, "--central-pin is required"},
		{"missing file", []string{filepath.Join(t.TempDir(), "nope.crt")}, "couldn't read"},
		{"not a certificate", []string{writeTemp(t, "k.pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")}, "not a certificate"},
		{"no PEM", []string{writeTemp(t, "x.txt", "hello")}, "holds no certificate"},
		{"a CA", []string{writeTemp(t, "ca.crt", testCACertPEM(t))}, "certificate authority"},
		{"a CA after a leaf", []string{writeTemp(t, "chain.crt", leaf+testCACertPEM(t))}, "certificate authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readLeafPins("--central-pin", tc.files); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("err = %v, want %q", err, tc.err)
			}
		})
	}
}

const partnerConfOneAdmin = "# Generated by `versola edge partner`" + ` -- rewritten by it, don't edit by hand:
# what is yours goes into edge.local.conf, which is included last and overrides anything
# here. The ${...} values are the edge's secrets, from edge.secrets.env through the
# container's environment. This edge's id, its key, its group and the addresses of
# central and auth are not here: central tells them at enrollment, and the edge keeps
# them in central.identity.directory.

mode = "partner"
env = prod

# The origin clients and the console reach this edge on (TLS ends at your proxy).
edge-url = "https://login.acmepay.example"

configuration-cache-refresh-interval = "5 minutes"

# The certificate(s) auth's mutual-TLS address presents, pinned (not trusted through a
# CA); several only to overlap a rotation.
versola-internal-trusted-certificates = [AUTHPINS]

# The edge enrolls once with the token below and keeps the certificate central issues,
# and renews, in the identity directory: without that directory it would need a new
# token at every start. Central's mutual-TLS listener is pinned like auth's.
central {
  mutual-tls {
    trusted-certificates = [CENTRALPINS]
  }
  identity {
    directory = "/var/lib/versola-edge"
    enrollment {
      token-file = "/etc/versola-edge/token"
    }
  }
}

partner {
  # The console's first administrators, made invites at every start: each signs in
  # at the operator's auth with this phone number or email, verified.
  admins = [ADMINS]
CONSOLE}

security {
  token-encryption {
    key = ${EDGE_TOKEN_ENC_KEY}
  }

  edge-sessions {
    secret = ${EDGE_SESSIONS_SECRET}
    ttl = 30 days
  }
}

# The edge's own database; with RUN_MIGRATIONS (compose.yml) the edge applies
# its schema at start. In env = prod a database that is not on this host must
# be reached over verified TLS (sslmode=verify-full).
postgres {
  url = "jdbc:postgresql://db.acmepay.example:5432/edge?sslmode=verify-full"
  user = "edge"
  password = ${POSTGRES_PASSWORD}
  maximum-pool-size = 10
  minimum-idle = 10
  connection-timeout = "30 seconds"
  max-lifetime = "30 minutes"
  leak-detection-threshold = "60 seconds"
  pool-metrics-interval = ${?POSTGRES_POOL_METRICS_INTERVAL}
}

cleanup {
  max-threads = 2
  tables = [
    {
      table-name = "pending_logins"
      batch-size = 1000
      interval   = "5 minutes"
      key-column = "state"
    }
    {
      table-name = "edge_sessions"
      batch-size = 500
      interval   = "1 hour"
      key-column = "ctid"
    }
    {
      table-name = "revocations"
      batch-size = 1000
      interval   = "1 hour"
      key-column = "revoked_key"
    }
  ]
}

include file("/etc/versola-edge/edge.local.conf")
`

const partnerConsoleLines = `  # The console's files (partner.html, versola-partner*.js), served under /partner/.
  console-directory = "/app/console"
`

// edge.conf of a partner's edge, compared whole: what the edge requires in
// partner mode, and none of what it refuses there or central tells it.
func TestPartnerEdgeFilesConf(t *testing.T) {
	// confKeys sees the keys a first-party edge.conf sets, so it would see them here.
	fp, err := edgeFiles(sampleBundle(t, "eu", true), "0.8.0", edgeSite{EdgeURL: "https://e.example.com", PostgresURL: "jdbc:postgresql://db/edge", PostgresUser: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(confKeys(string(fp["edge.conf"])), " "); !strings.Contains(got, "id key-id") || !strings.Contains(got, "central.url") ||
		!strings.Contains(got, "central.identity.group") || !strings.Contains(got, "central.identity.enrollment.url") || !strings.Contains(got, "native.blob-key") {
		t.Fatalf("confKeys of a first-party edge.conf = %s", got)
	}
	c1, c2, a1, a2 := testCertPEM(t, "c1"), testCertPEM(t, "c2"), testCertPEM(t, "a1"), testCertPEM(t, "a2")
	for _, tc := range []struct {
		name                    string
		central, auth, admins   []string
		console                 bool
		centralPins, authPins   string
		adminList, consoleLines string
	}{
		{"one admin, one pin each, no console", []string{c1}, []string{a1}, []string{"+77010000000"}, false,
			`"/etc/versola-edge/pins/central-1.crt"`, `"/etc/versola-edge/pins/auth-1.crt"`, `"+77010000000"`, ""},
		{"several admins and pins, a console", []string{c1, c2}, []string{a1, a2}, []string{"+77010000000", "it@acmepay.example"}, true,
			`"/etc/versola-edge/pins/central-1.crt", "/etc/versola-edge/pins/central-2.crt"`,
			`"/etc/versola-edge/pins/auth-1.crt", "/etc/versola-edge/pins/auth-2.crt"`,
			`"+77010000000", "it@acmepay.example"`, partnerConsoleLines},
		{"an admin to escape", []string{c1}, []string{a1}, []string{`o"ne\il@acmepay.example`}, true,
			`"/etc/versola-edge/pins/central-1.crt"`, `"/etc/versola-edge/pins/auth-1.crt"`, `"o\"ne\\il@acmepay.example"`, partnerConsoleLines},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := partnerEdgeFiles(partnerSite{
				EdgeURL: "https://login.acmepay.example", CentralPins: tc.central, AuthPins: tc.auth, Admins: tc.admins,
				PostgresURL: "jdbc:postgresql://db.acmepay.example:5432/edge?sslmode=verify-full", PostgresUser: "edge", Console: tc.console,
			})
			want := strings.NewReplacer("AUTHPINS", tc.authPins, "CENTRALPINS", tc.centralPins, "ADMINS", tc.adminList, "CONSOLE", tc.consoleLines).Replace(partnerConfOneAdmin)
			conf := string(files["edge.conf"])
			if conf != want {
				t.Errorf("edge.conf:\n%s\nwant:\n%s", conf, want)
			}
			// What the edge refuses in partner mode, or a partner edge has no use for.
			keys := confKeys(conf)
			for _, bad := range []string{"id", "key-id", "private-key", "versola-url", "central.url", "central.identity.group",
				"central.identity.enrollment.url", "security.internal-secret", "revocation", "central.mutual-tls.certificate",
				"central.mutual-tls.private-key", "native", "dpop"} {
				for _, k := range keys {
					if k == bad || strings.HasPrefix(k, bad+".") {
						t.Errorf("edge.conf sets %s", k)
					}
				}
			}
			for _, need := range []string{"mode", "env", "edge-url", "versola-internal-trusted-certificates", "central.mutual-tls.trusted-certificates",
				"central.identity.directory", "central.identity.enrollment.token-file", "partner.admins", "security.token-encryption.key",
				"security.edge-sessions.secret", "postgres.url", "postgres.password"} {
				found := false
				for _, k := range keys {
					found = found || k == need
				}
				if !found {
					t.Errorf("edge.conf does not set %s", need)
				}
			}
			if files["pins/central-1.crt"] == nil || files["pins/auth-1.crt"] == nil || len(files) != 1+len(tc.central)+len(tc.auth) {
				t.Errorf("files = %d", len(files))
			}
		})
	}
}

// confKeys are the dotted paths of the keys a generated edge.conf sets: the
// file's own shape (one key or block per line), not HOCON in general.
func confKeys(conf string) []string {
	var path, keys []string
	for _, line := range strings.Split(conf, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "include ") || l == "]":
		case l == "}":
			path = path[:len(path)-1]
		case l == "{":
			path = append(path, "[]")
		case strings.HasSuffix(l, "{"):
			path = append(path, strings.TrimSpace(strings.TrimSuffix(l, "{")))
			keys = append(keys, strings.Join(path, "."))
		default:
			k, _, _ := strings.Cut(l, "=")
			keys = append(keys, strings.Join(append(append([]string{}, path...), strings.TrimSpace(k)), "."))
		}
	}
	return keys
}

const partnerComposeWant = "# Generated by `versola edge partner`" + ` -- rewritten by it, don't edit by hand.
name: versola-partner-edge-login-acmepay-example

services:
  edge:
    image: ghcr.io/versolauth/versola-edge:0.8.0
    container_name: versola-partner-edge-login-acmepay-example
    restart: unless-stopped
    stop_grace_period: 20s
    network_mode: host
    # The image's own user is the unprivileged uid 10001, which can read neither the token (0600) nor
    # the pins directory (0700), both the operator's, nor an edge-identity volume an earlier release
    # created as root. Root keeps what this file always did.
    user: "0:0"
    environment:
      PORT: 8095
      DPORT: 8096
      CONFIG_PATH: /app/config/env.conf
      BIND_HOST: "127.0.0.1"
      # The edge checks its schema at start and stops on an old one: this edge's
      # own database gets its migrations from the edge itself.
      RUN_MIGRATIONS: "true"
    # Secrets (the edge's own keys and the database password).
    env_file:
      - path: ./edge.secrets.env
        required: false
    volumes:
      - ./edge.conf:/app/config/env.conf:ro
      - ./edge.local.conf:/etc/versola-edge/edge.local.conf:ro
      - ./token:/etc/versola-edge/token:ro
      - ./pins:/etc/versola-edge/pins:ro
CONSOLE      # The identity: key and certificate chain. Without it the edge enrolls
      # again at every start, and the token is spent -- keep it.
      - edge-identity:/var/lib/versola-edge

volumes:
  edge-identity:
`

func TestPartnerEdgeCompose(t *testing.T) {
	project := partnerProject("/home/p/.versola/edge/login.acmepay.example")
	if got := partnerEdgeCompose(project, "0.8.0", ""); got != strings.Replace(partnerComposeWant, "CONSOLE", "", 1) {
		t.Errorf("without a console:\n%s", got)
	}
	console := `      # The partner console's files (only partner.html and versola-partner*.js|css are served).
      - type: bind
        source: "/srv/partner console/v1"
        target: /app/console
        read_only: true
`
	if got := partnerEdgeCompose(project, "0.8.0", "/srv/partner console/v1"); got != strings.Replace(partnerComposeWant, "CONSOLE", console, 1) {
		t.Errorf("with a console:\n%s", got)
	}
}

func TestPartnerNames(t *testing.T) {
	for in, want := range map[string]string{
		"https://login.acmepay.example":      "login.acmepay.example",
		"https://Login.AcmePay.example:8443": "login.acmepay.example",
		"https://[::1]":                      "1",
		"https://10.0.0.5":                   "10.0.0.5",
	} {
		if got := partnerDirName(in); got != want {
			t.Errorf("partnerDirName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"/x/login.acmepay.example": "versola-partner-edge-login-acmepay-example",
		"/x/My Edge_1":             "versola-partner-edge-my-edge_1",
		"/x/...":                   "versola-partner-edge-edge",
	} {
		if got := partnerProject(in); got != want {
			t.Errorf("partnerProject(%q) = %q, want %q", in, got, want)
		}
	}
}

// partnerFixture is a partner's inputs on disk: the token, one pin each, a
// password, a console directory.
type partnerFixture struct {
	opts       EdgePartnerOptions
	consoleDir string
}

func newPartnerFixture(t *testing.T) partnerFixture {
	t.Helper()
	in := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(in, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	console := filepath.Join(in, "console")
	write("console/partner.html", "<html></html>")
	write("console/versola-partner.js", "")
	write("console/versola-partner-theme.js", "")
	return partnerFixture{consoleDir: console, opts: EdgePartnerOptions{
		TokenFile: write("token.txt", sampleToken+"\n"), Version: "0.8.0", EdgeURL: "https://login.acmepay.example/",
		CentralPinFiles: []string{write("central.crt", testCertPEM(t, "central"))}, AuthPinFiles: []string{write("auth.crt", testCertPEM(t, "auth"))},
		Admins:      []string{"+7 701 000 00 00", "IT@acmepay.example"},
		PostgresURL: "jdbc:postgresql://db.acmepay.example:5432/edge?sslmode=verify-full", PostgresUser: "edge",
		PostgresPasswordFile: write("pw", "db-pass\n"), ConsoleDir: console,
		Dir: filepath.Join(t.TempDir(), "edge"), NoStart: true,
	}}
}

func TestEdgePartnerWritesTheEdgesDirectory(t *testing.T) {
	f := newPartnerFixture(t)
	res, err := EdgePartner(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	dir := f.opts.Dir
	if res.Dir != dir || res.Started || len(res.Missing) != 0 || len(res.Warnings) != 0 || res.EnrollAddress != "https://enroll.northbank.example" {
		t.Fatalf("= %+v", res)
	}
	conf := readFile(t, dir, "edge.conf")
	for _, w := range []string{`mode = "partner"`, `edge-url = "https://login.acmepay.example"`, `admins = ["+77010000000", "it@acmepay.example"]`, `console-directory = "/app/console"`} {
		if !strings.Contains(conf, w) {
			t.Errorf("edge.conf lacks %q", w)
		}
	}
	if strings.Contains(conf, sampleToken) {
		t.Error("the token is in edge.conf, which is world-readable")
	}
	if readFile(t, dir, "token") != sampleToken+"\n" {
		t.Error("the token file")
	}
	compose := readFile(t, dir, "compose.yml")
	consoleAbs, _ := filepath.Abs(f.consoleDir)
	for _, w := range []string{"image: ghcr.io/versolauth/versola-edge:0.8.0", "name: versola-partner-edge-edge", "target: /app/console", "read_only: true", "edge-identity:/var/lib/versola-edge", `RUN_MIGRATIONS: "true"`} {
		if !strings.Contains(compose, w) {
			t.Errorf("compose.yml lacks %q", w)
		}
	}
	if !strings.Contains(compose, consoleAbs) {
		t.Errorf("compose.yml does not mount %s", consoleAbs)
	}
	env := readFile(t, dir, "edge.secrets.env")
	for _, name := range partnerGeneratedSecrets {
		if raw, err := base64.RawURLEncoding.DecodeString(envValue(t, env, name)); err != nil || len(raw) != 32 {
			t.Errorf("%s = %d bytes, %v", name, len(raw), err)
		}
	}
	if envValue(t, env, "POSTGRES_PASSWORD") != "'db-pass'" {
		t.Error("POSTGRES_PASSWORD")
	}
	for _, name := range []string{"EDGE_DPOP_NONCE_SALT", "EDGE_NATIVE_BLOB_KEY", "EDGE_KEY_ID"} {
		if strings.Contains(env, name) {
			t.Errorf("edge.secrets.env holds %s, which a partner's edge does not use", name)
		}
	}
	if !strings.Contains(readFile(t, dir, "edge.local.conf"), "versola edge partner") {
		t.Error("edge.local.conf")
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{"token": 0o600, "edge.secrets.env": 0o600, "pins": 0o700, "edge.conf": 0o644} {
			if st, err := os.Stat(filepath.Join(dir, name)); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s: %v %v, want %v", name, st, err, want)
			}
		}
	}
}

// A re-run keeps the generated keys and the operator's files, replaces the
// password and the pins, and without a token file keeps the token there.
func TestEdgePartnerRerun(t *testing.T) {
	f := newPartnerFixture(t)
	if _, err := EdgePartner(f.opts); err != nil {
		t.Fatal(err)
	}
	dir := f.opts.Dir
	first := readFile(t, dir, "edge.secrets.env")
	if err := os.WriteFile(filepath.Join(dir, "edge.local.conf"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge.secrets.env"), []byte(first+"MINE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := f.opts
	o.TokenFile = ""
	o.PostgresPasswordFile = writeTemp(t, "pw2", "other\n")
	o.Admins = []string{"it@acmepay.example"}
	o.ConsoleDir = ""
	if _, err := EdgePartner(o); err != nil {
		t.Fatal(err)
	}
	env := readFile(t, dir, "edge.secrets.env")
	for _, name := range partnerGeneratedSecrets {
		if envValue(t, env, name) != envValue(t, first, name) {
			t.Errorf("%s was regenerated", name)
		}
	}
	if envValue(t, env, "POSTGRES_PASSWORD") != "'other'" || envValue(t, env, "MINE") != "1" {
		t.Errorf("edge.secrets.env = merged wrongly")
	}
	if readFile(t, dir, "edge.local.conf") != "mine\n" {
		t.Error("edge.local.conf was overwritten")
	}
	if readFile(t, dir, "token") != sampleToken+"\n" {
		t.Error("the token was not kept")
	}
	conf := readFile(t, dir, "edge.conf")
	if !strings.Contains(conf, `admins = ["it@acmepay.example"]`) || strings.Contains(conf, "console-directory") {
		t.Errorf("edge.conf was not rewritten from the flags:\n%s", conf)
	}
	if strings.Contains(readFile(t, dir, "compose.yml"), "/app/console") {
		t.Error("the console is still mounted")
	}
	// The token file removed by the partner: an empty one, so Compose mounts a file.
	if err := os.Remove(filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	if _, err := EdgePartner(o); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, "token")); err != nil || !st.Mode().IsRegular() || st.Size() != 0 {
		t.Errorf("token = %v, %v", st, err)
	}
}

func TestEdgePartnerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(t *testing.T, o *EdgePartnerOptions)
		err  string
	}{
		{"no token the first time", func(t *testing.T, o *EdgePartnerOptions) { o.TokenFile = "" }, "token file is required the first time"},
		{"a bundle as the token", func(t *testing.T, o *EdgePartnerOptions) { o.TokenFile = writeTemp(t, "b.json", `{"version":1}`) }, "does not start with 'vtk1.'"},
		{"an http token", func(t *testing.T, o *EdgePartnerOptions) {
			o.TokenFile = writeTemp(t, "t", tokenFor("http://enroll.example.com", "zz"))
		}, "not an https URL"},
		{"no edge url", func(t *testing.T, o *EdgePartnerOptions) { o.EdgeURL = "" }, "--edge-url is required"},
		{"an http edge url", func(t *testing.T, o *EdgePartnerOptions) { o.EdgeURL = "http://login.acmepay.example" }, "is not an https origin"},
		{"an edge url with a path", func(t *testing.T, o *EdgePartnerOptions) { o.EdgeURL = "https://acmepay.example/login" }, "is not an https origin"},
		{"no version", func(t *testing.T, o *EdgePartnerOptions) { o.Version = "" }, "--version is required"},
		{"a bad version", func(t *testing.T, o *EdgePartnerOptions) { o.Version = "0.8.0 latest" }, "--version is required"},
		{"no admin", func(t *testing.T, o *EdgePartnerOptions) { o.Admins = nil }, "--admin is required"},
		{"a blank admin", func(t *testing.T, o *EdgePartnerOptions) { o.Admins = []string{"it@acmepay.example", " "} }, "blank"},
		{"a bad admin", func(t *testing.T, o *EdgePartnerOptions) { o.Admins = []string{"admin"} }, "neither an email"},
		{"no central pin", func(t *testing.T, o *EdgePartnerOptions) { o.CentralPinFiles = nil }, "--central-pin is required"},
		{"no auth pin", func(t *testing.T, o *EdgePartnerOptions) { o.AuthPinFiles = nil }, "--auth-pin is required"},
		{"a CA pin", func(t *testing.T, o *EdgePartnerOptions) {
			o.AuthPinFiles = []string{writeTemp(t, "ca.crt", testCACertPEM(t))}
		}, "certificate authority"},
		{"a postgres password in the url", func(t *testing.T, o *EdgePartnerOptions) {
			o.PostgresURL = "jdbc:postgresql://db:5432/edge?password=x"
		}, "holds a password"},
		{"not jdbc", func(t *testing.T, o *EdgePartnerOptions) { o.PostgresURL = "postgres://db:5432/edge" }, "not a JDBC URL"},
		{"no postgres url", func(t *testing.T, o *EdgePartnerOptions) { o.PostgresURL = "" }, "--postgres-url is required"},
		{"a console without partner.html", func(t *testing.T, o *EdgePartnerOptions) { o.ConsoleDir = t.TempDir() }, "has no partner.html"},
		{"a console that is no directory", func(t *testing.T, o *EdgePartnerOptions) { o.ConsoleDir = filepath.Join(t.TempDir(), "x") }, "is not a directory"},
		{"an empty password file", func(t *testing.T, o *EdgePartnerOptions) { o.PostgresPasswordFile = writeTemp(t, "pw", "\n") }, "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPartnerFixture(t)
			tc.mod(t, &f.opts)
			_, err := EdgePartner(f.opts)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if strings.Contains(err.Error(), sampleToken) || strings.Contains(err.Error(), ".zz") {
				t.Error("the error shows the token")
			}
			// Nothing is written before everything is checked.
			if _, err := os.Stat(f.opts.Dir); !os.IsNotExist(err) {
				t.Errorf("%s was created: %v", f.opts.Dir, err)
			}
		})
	}
}

// A directory `edge bootstrap` wrote is not taken over.
func TestEdgePartnerRefusesAnotherEdgesDirectory(t *testing.T) {
	f := newPartnerFixture(t)
	if err := os.MkdirAll(f.opts.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := "# Generated by `versola edge bootstrap` from the bundle of edge eu -- rewritten by it,\n"
	if err := os.WriteFile(filepath.Join(f.opts.Dir, "edge.conf"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EdgePartner(f.opts); err == nil || !strings.Contains(err.Error(), "another edge") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, f.opts.Dir, "edge.conf") != other {
		t.Error("edge.conf was replaced")
	}
}

func TestEdgePartnerWarnsAndWaitsForThePassword(t *testing.T) {
	f := newPartnerFixture(t)
	if err := os.Remove(filepath.Join(f.consoleDir, "versola-partner-theme.js")); err != nil {
		t.Fatal(err)
	}
	f.opts.PostgresPasswordFile = ""
	f.opts.NoStart = false // not started anyway: the password is missing
	res, err := EdgePartner(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Started || len(res.Missing) != 1 || !strings.Contains(res.Missing[0], "POSTGRES_PASSWORD") {
		t.Errorf("= %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "versola-partner-theme.js") {
		t.Errorf("warnings = %q", res.Warnings)
	}
}

// The default directory is named after the edge URL's host, under ~/.versola/edge.
func TestEdgePartnerDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	f := newPartnerFixture(t)
	f.opts.Dir = ""
	res, err := EdgePartner(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".versola", "edge", "login.acmepay.example"); res.Dir != want || res.Project != "versola-partner-edge-login-acmepay-example" {
		t.Errorf("= %s %s, want %s", res.Dir, res.Project, want)
	}
}
