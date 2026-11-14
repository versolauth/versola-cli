package deploy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/state"
)

// testCertPEM is a throwaway self-signed certificate.
func testCertPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

type fakeAdmin struct {
	registered      []string
	groups          []string
	registerErr     error
	tokenErr        error
	tokenFor        []string
	keyID, key, tkn string
	expires         time.Time
	// existing: the edges central has, by id, with their group.
	existing map[string]string
	groupErr error
}

func (f *fakeAdmin) groupOf(id string) (string, bool, error) {
	g, ok := f.existing[id]
	return g, ok, f.groupErr
}

func (f *fakeAdmin) register(id, group string) (string, string, error) {
	f.registered = append(f.registered, id)
	f.groups = append(f.groups, group)
	return f.keyID, f.key, f.registerErr
}
func (f *fakeAdmin) issueToken(id string) (string, time.Time, error) {
	f.tokenFor = append(f.tokenFor, id)
	return f.tkn, f.expires, f.tokenErr
}

func publishedState() *state.State {
	return &state.State{Target: "vps", Version: "0.8.0", AuthURL: "https://id.example.com", ProxyMode: "nginx",
		CentralDomain: "central.example.com", CentralMTLSDomain: "central-mtls.example.com", MTLSDomain: "id-mtls.example.com"}
}

func TestBuildEdgeAdd(t *testing.T) {
	central, auth := testCertPEM(t, "central"), testCertPEM(t, "auth")
	pins := func() ([]string, []string, error) { return []string{central}, []string{auth}, nil }
	expires := time.Now().Add(time.Hour)
	api := &fakeAdmin{keyID: "k1", key: "PRIVATE", tkn: "tok", expires: expires}

	res, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "eu-central", Group: "eu"}, api, pins)
	if err != nil {
		t.Fatal(err)
	}
	b := res.Bundle
	if b.EdgeID != "eu-central" || b.Group != "eu" || b.EnrollmentToken != "tok" || !b.TokenExpiresAt.Equal(expires.UTC()) ||
		b.EnrollURL != "https://central.example.com" || b.CentralURL != "https://central-mtls.example.com" ||
		b.AuthURL != "https://id-mtls.example.com" || b.VersolaVersion != "0.8.0" || b.Version != BundleVersion {
		t.Errorf("bundle = %+v", b)
	}
	if len(b.CentralPins) != 1 || len(b.AuthPins) != 1 {
		t.Errorf("pins = %v / %v", b.CentralPins, b.AuthPins)
	}
	if api.registered[0] != "eu-central" || api.groups[0] != "eu" || api.tokenFor[0] != "eu-central" {
		t.Errorf("api calls = %v %v %v", api.registered, api.groups, api.tokenFor)
	}
	if res.KeyID != "k1" || res.PrivateKey != "PRIVATE" {
		t.Errorf("the registration's key is not handed back: %+v", res)
	}
	// The key is shown once and is the one thing the bundle must not carry.
	enc, _ := EncodeBundle(b)
	if strings.Contains(string(enc), "PRIVATE") || strings.Contains(string(enc), "privateKey") {
		t.Errorf("the bundle holds the registration key:\n%s", enc)
	}
}

func TestBuildEdgeAddExistingOnlyIssuesAToken(t *testing.T) {
	pins := func() ([]string, []string, error) { return []string{testCertPEM(t, "c")}, nil, nil }
	api := &fakeAdmin{tkn: "tok", expires: time.Now().Add(time.Hour), existing: map[string]string{"eu-central": "eu", "plain": ""}}
	res, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "eu-central", Existing: true}, api, pins)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.registered) != 0 || res.PrivateKey != "" || len(api.tokenFor) != 1 {
		t.Errorf("registered %v, key %q, tokens %v", api.registered, res.PrivateKey, api.tokenFor)
	}
	// The bundle carries the group central has for the edge: without it the
	// edge would ask for a certificate in the default group, which central refuses.
	if res.Bundle.Group != "eu" {
		t.Errorf("group = %q, want central's eu", res.Bundle.Group)
	}
	if res, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "plain", Existing: true}, api, pins); err != nil || res.Bundle.Group != "" {
		t.Errorf("default group: %q, %v", res.Bundle.Group, err)
	}
	// An edge central does not have, or a lookup that fails, spends no token.
	api.tokenFor = nil
	if _, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "nobody", Existing: true}, api, pins); err == nil || !strings.Contains(err.Error(), "without --existing") || len(api.tokenFor) != 0 {
		t.Errorf("an unknown edge: %v, tokens %v", err, api.tokenFor)
	}
	api.groupErr = errors.New("central answered 500")
	if _, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "eu-central", Existing: true}, api, pins); err == nil || len(api.tokenFor) != 0 {
		t.Errorf("a failed lookup: %v, tokens %v", err, api.tokenFor)
	}
}

func TestBuildEdgeAddRefusals(t *testing.T) {
	pins := func() ([]string, []string, error) { return []string{testCertPEM(t, "c")}, nil, nil }
	noNames := publishedState()
	noNames.CentralDomain = ""
	noMTLS := publishedState()
	noMTLS.CentralMTLSDomain = ""
	local := publishedState()
	local.Target = "local"
	noAuthName := publishedState()
	noAuthName.MTLSDomain = ""
	for name, tc := range map[string]struct {
		st   *state.State
		o    EdgeAddOptions
		want string
	}{
		"bad id":                 {publishedState(), EdgeAddOptions{ID: "EU Central"}, "edge id"},
		"id starts with a digit": {publishedState(), EdgeAddOptions{ID: "1eu"}, "edge id"},
		"too long":               {publishedState(), EdgeAddOptions{ID: "a" + strings.Repeat("b", 63)}, "edge id"},
		"bad group":              {publishedState(), EdgeAddOptions{ID: "eu", Group: "../x"}, "--group"},
		"group of existing":      {publishedState(), EdgeAddOptions{ID: "eu", Group: "g", Existing: true}, "cannot change"},
		"local deployment":       {local, EdgeAddOptions{ID: "eu"}, "vps deployment"},
		"no deployment":          {nil, EdgeAddOptions{ID: "eu"}, "vps deployment"},
		"no enrollment name":     {noNames, EdgeAddOptions{ID: "eu"}, "--central-domain"},
		"no mutual-TLS name":     {noMTLS, EdgeAddOptions{ID: "eu"}, "--central-mtls-domain"},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAdmin{tkn: "tok"}
			_, err := buildEdgeAdd(tc.st, tc.o, api, pins)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			// Nothing was registered for a request that was never going to work.
			if len(api.registered) != 0 || len(api.tokenFor) != 0 {
				t.Errorf("central was called: %v %v", api.registered, api.tokenFor)
			}
		})
	}
	// Without auth's name the bundle has no auth address.
	api := &fakeAdmin{tkn: "tok", expires: time.Now().Add(time.Hour)}
	res, err := buildEdgeAdd(noAuthName, EdgeAddOptions{ID: "eu"}, api, func() ([]string, []string, error) {
		return []string{testCertPEM(t, "c")}, []string{testCertPEM(t, "a")}, nil
	})
	if err != nil || res.Bundle.AuthURL != "" || len(res.Bundle.AuthPins) != 0 {
		t.Errorf("= %+v, %v", res.Bundle, err)
	}
}

// The bundle carries auth's public URL (the edge's versola-url); a deployment
// without a usable one is refused before anything is registered.
func TestBuildEdgeAddCarriesTheAuthURL(t *testing.T) {
	pins := func() ([]string, []string, error) { return []string{testCertPEM(t, "c")}, nil, nil }
	st := publishedState()
	st.AuthURL = "https://id.example.com/"
	api := &fakeAdmin{tkn: "tok", expires: time.Now().Add(time.Hour)}
	res, err := buildEdgeAdd(st, EdgeAddOptions{ID: "eu"}, api, pins)
	if err != nil || res.Bundle.VersolaURL != "https://id.example.com" {
		t.Fatalf("= %q, %v", res.Bundle.VersolaURL, err)
	}
	for _, bad := range []string{"", "id.example.com", "https://id.example.com/app"} {
		st.AuthURL = bad
		api := &fakeAdmin{tkn: "tok"}
		if _, err := buildEdgeAdd(st, EdgeAddOptions{ID: "eu"}, api, pins); err == nil || !strings.Contains(err.Error(), "--auth-url") || len(api.registered) != 0 {
			t.Errorf("%q: err = %v, registered %v", bad, err, api.registered)
		}
	}
}

// A registration that succeeded and a token that did not: the edge exists,
// its key was shown, and the operator is told how to go on.
func TestBuildEdgeAddTokenFailureAfterRegistration(t *testing.T) {
	pins := func() ([]string, []string, error) { return []string{testCertPEM(t, "c")}, nil, nil }
	api := &fakeAdmin{tokenErr: errors.New("central answered 500"), key: "K"}
	res, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "eu"}, api, pins)
	if res.PrivateKey != "K" {
		t.Errorf("the key central showed once is dropped on a token failure: %+v", res)
	}
	if err == nil || !strings.Contains(err.Error(), "edge add eu --existing") || !strings.Contains(err.Error(), "registered") {
		t.Fatalf("err = %v", err)
	}
	// A pin that cannot be read stops before anything is registered.
	api = &fakeAdmin{}
	_, err = buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "eu"}, api, func() ([]string, []string, error) { return nil, nil, errors.New("no volume") })
	if err == nil || len(api.registered) != 0 {
		t.Errorf("err = %v, registered %v", err, api.registered)
	}
}

func TestHTTPEdgeAdmin(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		gotAuth = user + ":" + pass
		gotPath = r.Method + " " + r.URL.RequestURI()
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(status)
		switch r.URL.Path {
		case "/configuration/edges":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"edges":[{"id":"a","groupId":"default","groupKind":"first_party","hasOldKey":false},{"id":"eu","groupId":"grp"},{"id":"p","groupId":"acme","groupKind":"partner"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"keyId":"k1","privateKey":"PK"}`))
		case "/configuration/edges/enrollment-tokens":
			_, _ = w.Write([]byte(`{"token":"tok","expiresAt":"2030-01-02T03:04:05Z"}`))
		}
	}))
	defer srv.Close()
	a := &httpEdgeAdmin{base: srv.URL, password: "s3cret", http: srv.Client()}

	keyID, key, err := a.register("eu", "grp")
	if err != nil || keyID != "k1" || key != "PK" {
		t.Fatalf("register = %q %q %v", keyID, key, err)
	}
	if gotAuth != "central:s3cret" || gotPath != "POST /configuration/edges" || !strings.Contains(gotBody, `"id":"eu"`) || !strings.Contains(gotBody, `"groupId":"grp"`) {
		t.Errorf("request = %q %q %q", gotAuth, gotPath, gotBody)
	}
	if _, _, err := a.register("eu", ""); err != nil || strings.Contains(gotBody, "groupId") {
		t.Errorf("an empty group went into the body: %q (%v)", gotBody, err)
	}
	status = http.StatusOK
	for id, want := range map[string]struct {
		group string
		found bool
	}{"eu": {"grp", true}, "a": {"", true}, "zz": {"", false}} {
		if g, found, err := a.groupOf(id); err != nil || g != want.group || found != want.found {
			t.Errorf("groupOf(%s) = %q %v %v", id, g, found, err)
		}
	}
	if _, _, err := a.groupOf("p"); err == nil || !strings.Contains(err.Error(), "partner") {
		t.Errorf("a partner's edge: %v", err)
	}
	if gotPath != "GET /configuration/edges" || gotAuth != "central:s3cret" {
		t.Errorf("groupOf request = %q %q", gotAuth, gotPath)
	}
	tok, exp, err := a.issueToken("eu")
	if err != nil || tok != "tok" || exp.Year() != 2030 {
		t.Fatalf("issueToken = %q %v %v", tok, exp, err)
	}
	if gotPath != "POST /configuration/edges/enrollment-tokens?edgeId=eu" {
		t.Errorf("path = %q", gotPath)
	}

	for code, want := range map[int]string{
		http.StatusUnauthorized: "refused the admin credentials",
		http.StatusConflict:     "already exists",
		http.StatusNotFound:     "no edge enrollment",
		http.StatusBadRequest:   "answered 400",
	} {
		status = code
		_, _, err := a.issueToken("eu")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d: err = %v, want one containing %q", code, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the credentials are in the error: %v", err)
		}
	}
	// A central that is down says how to find out why.
	srv.Close()
	if _, _, err := a.issueToken("eu"); err == nil || !strings.Contains(err.Error(), "couldn't reach central") {
		t.Errorf("err = %v", err)
	}
}

func TestReadEnvValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "central.secrets.env")
	if err := os.WriteFile(path, []byte("OTHER=1\nCENTRAL_RESOURCE_SECRET=\"abc-DEF_9\"\nEMPTY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := readEnvValue(path, "CENTRAL_RESOURCE_SECRET"); err != nil || v != "abc-DEF_9" {
		t.Errorf("= %q, %v", v, err)
	}
	for _, key := range []string{"EMPTY", "MISSING"} {
		if _, err := readEnvValue(path, key); err == nil || strings.Contains(err.Error(), "abc-DEF_9") {
			t.Errorf("%s: err = %v", key, err)
		}
	}
	if _, err := readEnvValue(filepath.Join(t.TempDir(), "none"), "X"); err == nil {
		t.Error("a missing file")
	}
}

func TestNormalizePins(t *testing.T) {
	a, b := testCertPEM(t, "a"), testCertPEM(t, "b")
	out, err := normalizePins([]string{a, a + b, "  " + b})
	if err != nil || len(out) != 2 {
		t.Fatalf("= %d, %v; want the two distinct certificates once", len(out), err)
	}
	for name, in := range map[string]string{
		"empty":      "",
		"text":       "not a pem",
		"a key":      "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
		"not a cert": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		if _, err := normalizePins([]string{in}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if fp := Fingerprint(a); len(fp) != 95 || strings.Count(fp, ":") != 31 {
		t.Errorf("fingerprint = %q", fp)
	}
	if Fingerprint("junk") != "" {
		t.Error("a fingerprint of junk")
	}
}

func writeBundle(t *testing.T, b BootstrapBundle) string {
	t.Helper()
	data, err := EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func goodBundle(t *testing.T) BootstrapBundle {
	return BootstrapBundle{
		Version: BundleVersion, EdgeID: "eu-central", Group: "eu", VersolaVersion: "0.8.0", VersolaURL: "https://id.example.com",
		EnrollmentToken: "tok-123", TokenExpiresAt: time.Now().Add(time.Hour),
		EnrollURL: "https://central.example.com", CentralURL: "https://central-mtls.example.com",
		CentralPins: []string{testCertPEM(t, "central"), testCertPEM(t, "central-next")},
		AuthURL:     "https://id-mtls.example.com", AuthPins: []string{testCertPEM(t, "auth")},
	}
}

func TestReadBundle(t *testing.T) {
	now := time.Now()
	if b, err := ReadBundle(writeBundle(t, goodBundle(t)), now); err != nil || b.EdgeID != "eu-central" {
		t.Fatalf("= %+v, %v", b, err)
	}
	for name, mutate := range map[string]func(*BootstrapBundle){
		"wrong version":    func(b *BootstrapBundle) { b.Version = 9 },
		"bad id":           func(b *BootstrapBundle) { b.EdgeID = "../etc" },
		"no token":         func(b *BootstrapBundle) { b.EnrollmentToken = "" },
		"expired":          func(b *BootstrapBundle) { b.TokenExpiresAt = now.Add(-time.Minute) },
		"http enroll":      func(b *BootstrapBundle) { b.EnrollURL = "http://central.example.com" },
		"a path in a url":  func(b *BootstrapBundle) { b.CentralURL = "https://central-mtls.example.com/edge" },
		"a quote in a url": func(b *BootstrapBundle) { b.CentralURL = "https://central-mtls.example.com\" } include \"/etc/passwd" },
		"no central pin":   func(b *BootstrapBundle) { b.CentralPins = nil },
		"a bad pin":        func(b *BootstrapBundle) { b.CentralPins = []string{"junk"} },
		"auth without pin": func(b *BootstrapBundle) { b.AuthPins = nil },
		"no authUrl":       func(b *BootstrapBundle) { b.VersolaURL = "" },
		"authUrl a path":   func(b *BootstrapBundle) { b.VersolaURL = "https://id.example.com/login" },
		"authUrl ftp":      func(b *BootstrapBundle) { b.VersolaURL = "ftp://id.example.com" },
		"authUrl a quote":  func(b *BootstrapBundle) { b.VersolaURL = "https://id.example.com\"x" },
		"bad group":        func(b *BootstrapBundle) { b.Group = "../x" },
	} {
		t.Run(name, func(t *testing.T) {
			b := goodBundle(t)
			mutate(&b)
			if _, err := ReadBundle(writeBundle(t, b), now); err == nil {
				t.Error("accepted")
			}
		})
	}
	// Unknown fields are refused: a bundle of a newer layout must not be half understood.
	path := filepath.Join(t.TempDir(), "b.json")
	_ = os.WriteFile(path, []byte(`{"version":1,"surprise":true}`), 0o600)
	if _, err := ReadBundle(path, now); err == nil {
		t.Error("an unknown field was ignored")
	}
	if _, err := ReadBundle(filepath.Join(t.TempDir(), "none.json"), now); err == nil {
		t.Error("a missing file")
	}
	// authUrl round-trips under its JSON name, and plain http is accepted
	// (a deployment behind its own TLS terminator on a private network).
	b0 := goodBundle(t)
	b0.VersolaURL = "http://id.internal:8080"
	path0 := writeBundle(t, b0)
	if raw, _ := os.ReadFile(path0); !strings.Contains(string(raw), `"authUrl": "http://id.internal:8080"`) {
		t.Errorf("bundle JSON = %s", raw)
	}
	if got, err := ReadBundle(path0, now); err != nil || got.VersolaURL != "http://id.internal:8080" {
		t.Errorf("= %+v, %v", got, err)
	}
	// An older versola's bundle (no authUrl) says how to get one that works.
	b0.VersolaURL = ""
	if _, err := ReadBundle(writeBundle(t, b0), now); err == nil || !strings.Contains(err.Error(), "edge add eu-central --existing") {
		t.Errorf("err = %v", err)
	}
	// The expired one says how to get a new token.
	b := goodBundle(t)
	b.TokenExpiresAt = now.Add(-time.Hour)
	if _, err := ReadBundle(writeBundle(t, b), now); err == nil || !strings.Contains(err.Error(), "edge add eu-central --existing") {
		t.Errorf("err = %v", err)
	}
}

func goodSite() EdgeBootstrapOptions {
	return EdgeBootstrapOptions{
		EdgeURL: "https://edge.example.com", PostgresURL: "jdbc:postgresql://db.example.com:5432/edge?sslmode=verify-full", PostgresUser: "edge",
	}
}

// bootstrapOpts is goodSite for a bundle file and a directory, never started.
func bootstrapOpts(file, dir string) EdgeBootstrapOptions {
	o := goodSite()
	o.File, o.Dir, o.NoStart = file, dir, true
	return o
}

// sampleBundle is the bundle of the PR description: fixed values, so
// edge.conf can be compared whole.
func sampleBundle(t *testing.T, group string, withAuth bool) BootstrapBundle {
	b := BootstrapBundle{
		Version: BundleVersion, EdgeID: "edge-eu-west-1", Group: group, VersolaVersion: "0.8.0", VersolaURL: "https://id.example.com",
		EnrollmentToken: "vtk1.sample", TokenExpiresAt: time.Now().Add(time.Hour),
		EnrollURL: "https://central.example.com", CentralURL: "https://central-mtls.example.com",
		CentralPins: []string{testCertPEM(t, "central"), testCertPEM(t, "central-next")},
	}
	if withAuth {
		b.AuthURL, b.AuthPins = "https://id-mtls.example.com", []string{testCertPEM(t, "auth")}
	}
	return b
}

const sampleConfHead = `# Generated by ` + "`versola edge bootstrap`" + ` from the bundle of edge edge-eu-west-1 -- rewritten by it,
# don't edit by hand: what is yours goes into edge.local.conf, which is included last
# and overrides anything here. The ${...} values are the edge's secrets, from
# edge.secrets.env through the container's environment.

env = prod
id = "edge-eu-west-1"
key-id = ${EDGE_KEY_ID}
private-key = ${EDGE_PRIVATE_KEY}

configuration-cache-refresh-interval = "5 minutes"

# Auth's public URL: where browsers are sent to sign in, and the issuer the
# edge checks tokens against.
versola-url = "https://id.example.com"
# The origin clients reach this edge on -- what a DPoP proof's htu is
# rebuilt against, not trusting a forwarded Host header.
edge-url = "https://edge.example.com"

# The edge connects to central over mutual TLS with the identity certificate
# it enrolls for the first time with the token below and renews on its own
# (at a third of its lifetime left); the key never leaves the identity directory.
# Central's and auth's listener certificates are pinned, not trusted through a CA.
central {
  url = "https://central-mtls.example.com"
  mutual-tls {
    trusted-certificates = ["/etc/versola-edge/pins/central-1.crt", "/etc/versola-edge/pins/central-2.crt"]
  }
  identity {
    directory = "/var/lib/versola-edge"
GROUP    enrollment {
      url        = "https://central.example.com"
      token-file = "/etc/versola-edge/token"
    }
  }
}

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
  url = "jdbc:postgresql://db.example.com:5432/edge?sslmode=verify-full"
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
`

const sampleConfNative = `
# The native-app back channel, straight to auth's mutual-TLS listener. The pin
# is a list so auth's listener certificate can be rotated with an overlap window.
native {
  auth-mutual-tls-url  = "https://id-mtls.example.com"
  trusted-certificates = ["/etc/versola-edge/pins/auth-1.crt"]
  blob-key             = ${EDGE_NATIVE_BLOB_KEY}
}

# RFC 9449 proof validation: the salt keying this edge's own nonce space, and
# the two windows. What a proof may be signed with, and whether a nonce is
# required, come from central.
dpop {
  nonce-salt = ${EDGE_DPOP_NONCE_SALT}
  iat-leeway = "60 seconds"
  nonce-ttl = "600 seconds"
}
`

const sampleConfTail = "\ninclude file(\"/etc/versola-edge/edge.local.conf\")\n"

// edge.conf is the whole configuration the edge requires, compared whole:
// central.identity nested in central (a top-level identity, or an enrollment
// outside identity, is ignored by the edge, which then refuses to start), the
// group only when the bundle has one, native and dpop together or not at all,
// never central.mutual-tls.certificate (central.identity names the certificate).
func TestEdgeFilesConf(t *testing.T) {
	site := edgeSite{EdgeURL: "https://edge.example.com", PostgresURL: "jdbc:postgresql://db.example.com:5432/edge?sslmode=verify-full", PostgresUser: "edge"}
	for _, tc := range []struct {
		name, group string
		auth        bool
		groupLine   string
	}{
		{"default group, no auth listener", "", false, ""},
		{"default group, auth listener", "", true, ""},
		{"group eu, auth listener", "eu", true, "    group     = \"eu\"\n"},
		{"group eu, no auth listener", "eu", false, "    group     = \"eu\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := edgeFiles(sampleBundle(t, tc.group, tc.auth), "0.8.0", site)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Replace(sampleConfHead, "GROUP", tc.groupLine, 1)
			if tc.auth {
				want += sampleConfNative
			}
			want += sampleConfTail
			if got := string(files["edge.conf"]); got != want {
				t.Errorf("edge.conf:\n%s\nwant:\n%s", got, want)
			}
			if strings.Contains(string(files["edge.conf"]), "vtk1.sample") {
				t.Error("the token is in edge.conf, which is world-readable and rewritten")
			}
			if strings.Contains(string(files["edge.conf"]), "certificate =") || strings.Contains(string(files["edge.conf"]), "\nidentity") {
				t.Error("a static certificate or a top-level identity block")
			}
			wantPins := 2
			if tc.auth {
				wantPins = 3
			}
			n := 0
			for name := range files {
				if strings.HasPrefix(name, "pins/") {
					n++
				}
			}
			if n != wantPins {
				t.Errorf("%d pin files, want %d", n, wantPins)
			}
		})
	}
}

func TestEdgeBootstrapWritesTheEdgesDirectory(t *testing.T) {
	b := goodBundle(t)
	dir := filepath.Join(t.TempDir(), "edge")
	got, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, b), dir), time.Now())
	if err != nil || got.Dir != dir || got.Started {
		t.Fatalf("= %+v, %v", got, err)
	}
	// Without --key-file and --postgres-password-file the operator is told what is missing.
	if len(got.Missing) != 2 || !strings.Contains(got.Missing[0], "--key-file") || !strings.Contains(got.Missing[1], "--postgres-password-file") {
		t.Errorf("missing = %q", got.Missing)
	}
	conf := readFile(t, dir, "edge.conf")
	for _, w := range []string{
		`url = "https://central-mtls.example.com"`,
		`versola-url = "https://id.example.com"`,
		`edge-url = "https://edge.example.com"`,
		`group     = "eu"`,
		`include file("/etc/versola-edge/edge.local.conf")`,
	} {
		if !strings.Contains(conf, w) {
			t.Errorf("edge.conf lacks %q:\n%s", w, conf)
		}
	}
	if readFile(t, dir, "token") != "tok-123\n" {
		t.Error("the token file")
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{"token": 0o600, "edge.secrets.env": 0o600} {
			if st, err := os.Stat(filepath.Join(dir, name)); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s: %v %v, want %v", name, st, err, want)
			}
		}
	}
	compose := readFile(t, dir, "compose.yml")
	for _, w := range []string{"image: ghcr.io/versolauth/versola-edge:0.8.0", "container_name: versola-edge-eu-central", "edge-identity:/var/lib/versola-edge", "./token:/etc/versola-edge/token:ro", "./pins:/etc/versola-edge/pins:ro", `user: "0:0"`, `RUN_MIGRATIONS: "true"`} {
		if !strings.Contains(compose, w) {
			t.Errorf("compose.yml lacks %q:\n%s", w, compose)
		}
	}
	// 2 central pins, 1 auth pin.
	pins, _ := filepath.Glob(filepath.Join(dir, "pins", "*.crt"))
	if len(pins) != 3 {
		t.Errorf("pins = %v", pins)
	}
	// The edge's own keys are generated: 32 random bytes, base64url without padding.
	env := readFile(t, dir, "edge.secrets.env")
	for _, name := range edgeGeneratedSecrets {
		v := envValue(t, env, name)
		if raw, err := base64.RawURLEncoding.DecodeString(v); err != nil || len(raw) != 32 {
			t.Errorf("%s = %d bytes, %v", name, len(raw), err)
		}
	}
}

// envValue is the value of key in an env file's content, failing when absent.
func envValue(t *testing.T, content, key string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("%s is not in %q", key, content)
	return ""
}

// The operator's files are theirs, a stale pin is not kept, and --version overrides.
func TestEdgeBootstrapKeepsTheOperatorsFilesAndReplacesPins(t *testing.T) {
	b := goodBundle(t)
	dir := t.TempDir()
	if _, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, b), dir), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge.local.conf"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.CentralPins = b.CentralPins[:1]
	b.AuthURL, b.AuthPins = "", nil
	o := bootstrapOpts(writeBundle(t, b), dir)
	o.Version = "0.9.1"
	if _, err := EdgeBootstrap(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, dir, "edge.local.conf") != "mine\n" {
		t.Error("the operator's edge.local.conf was overwritten")
	}
	if pins, _ := filepath.Glob(filepath.Join(dir, "pins", "*.crt")); len(pins) != 1 {
		t.Errorf("stale pins left: %v", pins)
	}
	if !strings.Contains(readFile(t, dir, "compose.yml"), "versola-edge:0.9.1") {
		t.Error("--version ignored")
	}
	if conf := readFile(t, dir, "edge.conf"); strings.Contains(conf, "native") || strings.Contains(conf, "dpop") {
		t.Error("auth block left after the bundle dropped it")
	}
}

// rsaKeyB64URL is a real RSA-2048 PKCS#8 key as central hands it out
// (base64url, no padding), one whose text has a '-' or '_' in it: what the
// edge's standard-alphabet decoder refused.
func rsaKeyB64URL(t *testing.T) (der []byte, b64url string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, err = x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		b64url = base64.RawURLEncoding.EncodeToString(der)
		if strings.ContainsAny(b64url, "-_") {
			return der, b64url
		}
	}
	t.Fatal("no key with a '-' or '_' in 20 tries")
	return nil, ""
}

func TestStandardBase64(t *testing.T) {
	der, b64url := rsaKeyB64URL(t)
	for name, in := range map[string]string{
		"base64url":          b64url,
		"base64url, padded":  base64.URLEncoding.EncodeToString(der),
		"already standard":   base64.StdEncoding.EncodeToString(der),
		"standard, unpadded": base64.RawStdEncoding.EncodeToString(der),
	} {
		got, err := standardBase64(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// What the edge does: java.util.Base64.getDecoder(), the standard alphabet.
		back, err := base64.StdEncoding.Strict().DecodeString(got)
		if err != nil || string(back) != string(der) || strings.ContainsAny(got, "-_") {
			t.Errorf("%s: %q does not decode to the key with the standard alphabet: %v", name, got, err)
		}
	}
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	for name, in := range map[string]string{
		"not base64": "not*base64",
		"not a key":  base64.RawURLEncoding.EncodeToString([]byte("hello")),
		"an EC key":  base64.RawURLEncoding.EncodeToString(ecDER),
	} {
		if _, err := standardBase64(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func writeKeyFile(t *testing.T, keyID, privateKey string) string {
	t.Helper()
	key, _ := EdgeKeyFile("eu-central", keyID, privateKey)
	path := filepath.Join(t.TempDir(), "k.json")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEdgeBootstrapKeyFile(t *testing.T) {
	der, b64url := rsaKeyB64URL(t)
	o := bootstrapOpts(writeBundle(t, goodBundle(t)), t.TempDir())
	o.KeyFile = writeKeyFile(t, "k1", b64url)
	if _, err := EdgeBootstrap(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	env := readFile(t, o.Dir, "edge.secrets.env")
	if envValue(t, env, "EDGE_KEY_ID") != "k1" || envValue(t, env, "EDGE_PRIVATE_KEY") != base64.StdEncoding.EncodeToString(der) {
		t.Errorf("edge.secrets.env does not carry the key in the standard alphabet")
	}
	for name, content := range map[string]string{
		"a key id with a space": `{"keyId":"k 1","privateKey":"` + b64url + `"}`,
		"a key that is not one": `{"keyId":"k","privateKey":"abc"}`,
		"no key":                `{"keyId":"k"}`,
	} {
		bad := filepath.Join(t.TempDir(), "bad.json")
		_ = os.WriteFile(bad, []byte(content), 0o600)
		o := bootstrapOpts(writeBundle(t, goodBundle(t)), t.TempDir())
		o.KeyFile = bad
		if _, err := EdgeBootstrap(o, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		} else if entries, _ := os.ReadDir(o.Dir); len(entries) != 0 {
			t.Errorf("%s: wrote files for a refused key file: %v", name, entries)
		}
	}
	o = bootstrapOpts(writeBundle(t, goodBundle(t)), t.TempDir())
	o.KeyFile = filepath.Join(t.TempDir(), "none")
	if _, err := EdgeBootstrap(o, time.Now()); err == nil {
		t.Error("a missing key file")
	}
}

// edge.secrets.env is merged: a second run keeps the generated keys (a new
// EDGE_TOKEN_ENC_KEY would make every stored session unreadable) and the
// operator's own lines, --key-file replaces only the key's two lines, and a
// password already there is kept unless --postgres-password-file is passed.
func TestEdgeBootstrapMergesTheSecrets(t *testing.T) {
	dir := t.TempDir()
	_, key1 := rsaKeyB64URL(t)
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("first pa$$word\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := bootstrapOpts(writeBundle(t, goodBundle(t)), dir)
	o.KeyFile, o.PostgresPasswordFile = writeKeyFile(t, "k1", key1), pwFile
	res, err := EdgeBootstrap(o, time.Now())
	if err != nil || len(res.Missing) != 0 {
		t.Fatalf("= %+v, %v", res, err)
	}
	first := readFile(t, dir, "edge.secrets.env")
	if envValue(t, first, "POSTGRES_PASSWORD") != "'first pa$$word'" {
		t.Errorf("POSTGRES_PASSWORD is not the file's, single-quoted")
	}
	// The operator adds a line of their own.
	if err := os.WriteFile(filepath.Join(dir, "edge.secrets.env"), []byte(first+"MINE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second run without the flags changes nothing.
	if _, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, goodBundle(t)), dir), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "edge.secrets.env"); got != first+"MINE=1\n" {
		t.Errorf("a run without flags changed edge.secrets.env:\n%s\nwas:\n%s", got, first)
	}

	// A new key replaces only the key's lines.
	der2, key2 := rsaKeyB64URL(t)
	o = bootstrapOpts(writeBundle(t, goodBundle(t)), dir)
	o.KeyFile = writeKeyFile(t, "k2", key2)
	if _, err := EdgeBootstrap(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	third := readFile(t, dir, "edge.secrets.env")
	if envValue(t, third, "EDGE_KEY_ID") != "k2" || envValue(t, third, "EDGE_PRIVATE_KEY") != base64.StdEncoding.EncodeToString(der2) {
		t.Error("--key-file did not replace the key")
	}
	for _, name := range append([]string{"POSTGRES_PASSWORD", "MINE"}, edgeGeneratedSecrets...) {
		if envValue(t, third, name) != envValue(t, first+"MINE=1\n", name) {
			t.Errorf("%s changed with --key-file", name)
		}
	}
	if strings.Count(third, "EDGE_PRIVATE_KEY=") != 1 {
		t.Errorf("the key is in edge.secrets.env more than once")
	}

	// A new password replaces only the password.
	if err := os.WriteFile(pwFile, []byte("second\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o = bootstrapOpts(writeBundle(t, goodBundle(t)), dir)
	o.PostgresPasswordFile = pwFile
	if _, err := EdgeBootstrap(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	fourth := readFile(t, dir, "edge.secrets.env")
	if envValue(t, fourth, "POSTGRES_PASSWORD") != "'second'" || envValue(t, fourth, "EDGE_KEY_ID") != "k2" {
		t.Error("--postgres-password-file did not replace only the password")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, "edge.secrets.env")); st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", st.Mode())
		}
	}
}

// An earlier versola wrote the key base64url, which the edge cannot read: a
// run without --key-file re-encodes it, and changes nothing else.
func TestEdgeBootstrapReencodesAnOldKey(t *testing.T) {
	der, b64url := rsaKeyB64URL(t)
	dir := t.TempDir()
	old := "# The edge's secrets. Never overwritten by `versola edge bootstrap`.\nEDGE_KEY_ID=k1\nEDGE_PRIVATE_KEY=" + b64url + "\n"
	if err := os.WriteFile(filepath.Join(dir, "edge.secrets.env"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, goodBundle(t)), dir), time.Now()); err != nil {
		t.Fatal(err)
	}
	env := readFile(t, dir, "edge.secrets.env")
	if envValue(t, env, "EDGE_PRIVATE_KEY") != base64.StdEncoding.EncodeToString(der) || envValue(t, env, "EDGE_KEY_ID") != "k1" ||
		!strings.HasPrefix(env, "# The edge's secrets. Never overwritten") {
		t.Errorf("the old key was not re-encoded in place")
	}
}

// A secrets file written by an earlier versola (the key only, or a line
// left empty) gets what it lacks and keeps what it has.
func TestMergeEnv(t *testing.T) {
	in := "# mine\nEDGE_KEY_ID=old\nexport EDGE_TOKEN_ENC_KEY=\nEDGE_SESSIONS_SECRET=\"kept\"\n\nOTHER=x"
	got := mergeEnv(in, []envEntry{
		{"EDGE_KEY_ID", "new", true},
		{"EDGE_TOKEN_ENC_KEY", "gen1", false},
		{"EDGE_SESSIONS_SECRET", "gen2", false},
		{"EDGE_DPOP_NONCE_SALT", "gen3", false},
	})
	want := "# mine\nEDGE_KEY_ID=new\nEDGE_TOKEN_ENC_KEY=gen1\nEDGE_SESSIONS_SECRET=\"kept\"\n\nOTHER=x\nEDGE_DPOP_NONCE_SALT=gen3\n"
	if got != want {
		t.Errorf("mergeEnv =\n%q\nwant\n%q", got, want)
	}
	// A generated key with a value on one line and an empty duplicate keeps
	// both as they are: rewriting the empty (last) one would change the key.
	dup := "EDGE_TOKEN_ENC_KEY=kept\nEDGE_TOKEN_ENC_KEY=\n"
	if got := mergeEnv(dup, []envEntry{{"EDGE_TOKEN_ENC_KEY", "gen", false}}); got != dup {
		t.Errorf("mergeEnv with a duplicate = %q", got)
	}
	if !envHasValue(got, "EDGE_SESSIONS_SECRET") || envHasValue("A=''\n", "A") || envHasValue("B=1\n", "A") {
		t.Error("envHasValue")
	}
}

func TestEdgeBootstrapFlagValidation(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	for name, tc := range map[string]struct {
		mutate func(*EdgeBootstrapOptions)
		want   string
	}{
		"no edge url":          {func(o *EdgeBootstrapOptions) { o.EdgeURL = "" }, "--edge-url is required"},
		"edge url with a path": {func(o *EdgeBootstrapOptions) { o.EdgeURL = "https://edge.example.com/x" }, "--edge-url"},
		"edge url no scheme":   {func(o *EdgeBootstrapOptions) { o.EdgeURL = "edge.example.com" }, "--edge-url"},
		"edge url with quote":  {func(o *EdgeBootstrapOptions) { o.EdgeURL = `https://e.example.com"` }, "--edge-url"},
		"no postgres url":      {func(o *EdgeBootstrapOptions) { o.PostgresURL = "" }, "--postgres-url is required"},
		"not jdbc":             {func(o *EdgeBootstrapOptions) { o.PostgresURL = "postgres://db/edge" }, "--postgres-url"},
		"jdbc with a quote":    {func(o *EdgeBootstrapOptions) { o.PostgresURL = `jdbc:postgresql://db/edge"` }, "--postgres-url"},
		"password in jdbc url": {func(o *EdgeBootstrapOptions) { o.PostgresURL = "jdbc:postgresql://db/edge?user=e&Password=it's" }, "--postgres-password-file"},
		"sslpassword in url":   {func(o *EdgeBootstrapOptions) { o.PostgresURL = "jdbc:postgresql://db/edge?sslpassword=x" }, "--postgres-password-file"},
		"edge url empty host":  {func(o *EdgeBootstrapOptions) { o.EdgeURL = "https://:443" }, "--edge-url"},
		"userinfo in jdbc url": {func(o *EdgeBootstrapOptions) { o.PostgresURL = "jdbc:postgresql://e:it's@db/edge" }, "--postgres-password-file"},
		"bad user":             {func(o *EdgeBootstrapOptions) { o.PostgresUser = `ed"ge` }, "--postgres-user"},
		"empty user":           {func(o *EdgeBootstrapOptions) { o.PostgresUser = "" }, "--postgres-user"},
		"missing pw file":      {func(o *EdgeBootstrapOptions) { o.PostgresPasswordFile = pw + "-none" }, "--postgres-password-file"},
		"empty pw":             {func(o *EdgeBootstrapOptions) { _ = os.WriteFile(pw, []byte("\n"), 0o600); o.PostgresPasswordFile = pw }, "empty"},
		"pw with a quote": {func(o *EdgeBootstrapOptions) {
			_ = os.WriteFile(pw, []byte("it's"), 0o600)
			o.PostgresPasswordFile = pw
		}, "quote"},
		"pw over two lines": {func(o *EdgeBootstrapOptions) {
			_ = os.WriteFile(pw, []byte("a\nb\n"), 0o600)
			o.PostgresPasswordFile = pw
		}, "line break"},
	} {
		t.Run(name, func(t *testing.T) {
			o := bootstrapOpts(writeBundle(t, goodBundle(t)), t.TempDir())
			tc.mutate(&o)
			_, err := EdgeBootstrap(o, time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "it's") || strings.Contains(err.Error(), "a\nb") {
				t.Errorf("the password is in the error: %v", err)
			}
			if entries, _ := os.ReadDir(o.Dir); len(entries) != 0 {
				t.Errorf("wrote files for refused flags: %v", entries)
			}
		})
	}
	// http is accepted for --edge-url (a TLS terminator in front of it), a
	// trailing slash is dropped, and a managed Postgres's user@server is a user.
	o := bootstrapOpts(writeBundle(t, goodBundle(t)), t.TempDir())
	o.EdgeURL, o.PostgresUser = "http://edge.internal:8095/", "edge@server"
	if _, err := EdgeBootstrap(o, time.Now()); err != nil {
		t.Fatal(err)
	}
	if conf := readFile(t, o.Dir, "edge.conf"); !strings.Contains(conf, `edge-url = "http://edge.internal:8095"`+"\n") || !strings.Contains(conf, `user = "edge@server"`) {
		t.Errorf("edge.conf:\n%s", conf)
	}
}

func TestEdgeBootstrapRefusesBadBundleAndVersion(t *testing.T) {
	b := goodBundle(t)
	b.VersolaVersion = ""
	if _, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, b), t.TempDir()), time.Now()); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Errorf("err = %v", err)
	}
	b.VersolaVersion = "0.8.0/../x"
	if _, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, b), t.TempDir()), time.Now()); err == nil {
		t.Error("a version that is a path")
	}
	dir := t.TempDir()
	b = goodBundle(t)
	b.TokenExpiresAt = time.Now().Add(-time.Hour)
	if _, err := EdgeBootstrap(bootstrapOpts(writeBundle(t, b), dir), time.Now()); err == nil {
		t.Fatal("an expired bundle")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote files for a refused bundle: %v", entries)
	}
}

func TestEncodeBundleRoundTrips(t *testing.T) {
	b := goodBundle(t)
	data, err := EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	var back BootstrapBundle
	if err := json.Unmarshal(data, &back); err != nil || back.EdgeID != b.EdgeID || back.EnrollmentToken != b.EnrollmentToken || len(back.CentralPins) != 2 {
		t.Errorf("= %+v, %v", back, err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Central shows an edge's key once. A 2xx that carries none is an error that
// says the key was not delivered, and does not echo what came back.
func TestRegisterRefusesAnAnswerWithoutTheKey(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":   `{}`,
		"renamed fields": `{"key_id":"k1","private_key":"SECRET-HALF"}`,
		"key id only":    `{"keyId":"k1","privateKey":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			api := &httpEdgeAdmin{base: srv.URL, password: "x", http: srv.Client()}
			keyID, key, err := api.register("eu-central", "")
			if err == nil {
				t.Fatalf("registered with %q, key %q", keyID, key)
			}
			if !strings.Contains(err.Error(), "eu-central") || strings.Contains(err.Error(), "SECRET-HALF") {
				t.Errorf("error = %v", err)
			}
		})
	}
}
