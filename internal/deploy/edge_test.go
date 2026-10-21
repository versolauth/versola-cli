package deploy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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
	api := &fakeAdmin{tkn: "tok", expires: time.Now().Add(time.Hour)}
	res, err := buildEdgeAdd(publishedState(), EdgeAddOptions{ID: "eu-central", Existing: true}, api, pins)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.registered) != 0 || res.PrivateKey != "" || len(api.tokenFor) != 1 {
		t.Errorf("registered %v, key %q, tokens %v", api.registered, res.PrivateKey, api.tokenFor)
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
		Version: BundleVersion, EdgeID: "eu-central", Group: "eu", VersolaVersion: "0.8.0",
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
	// The expired one says how to get a new token.
	b := goodBundle(t)
	b.TokenExpiresAt = now.Add(-time.Hour)
	if _, err := ReadBundle(writeBundle(t, b), now); err == nil || !strings.Contains(err.Error(), "edge add eu-central --existing") {
		t.Errorf("err = %v", err)
	}
}

func TestEdgeBootstrapWritesTheEdgesDirectory(t *testing.T) {
	b := goodBundle(t)
	dir := filepath.Join(t.TempDir(), "edge")
	got, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, b), Dir: dir, NoStart: true}, time.Now())
	if err != nil || got != dir {
		t.Fatalf("= %q, %v", got, err)
	}
	conf := readFile(t, dir, "edge.conf")
	for _, w := range []string{
		`url = "https://central-mtls.example.com"`,
		`url        = "https://central.example.com"`,
		`token-file = "/etc/versola-edge/token"`,
		`directory = "/var/lib/versola-edge"`,
		`"/etc/versola-edge/pins/central-1.crt", "/etc/versola-edge/pins/central-2.crt"`,
		`auth-mutual-tls-url  = "https://id-mtls.example.com"`,
		`"/etc/versola-edge/pins/auth-1.crt"`,
		`include file("/etc/versola-edge/edge.local.conf")`,
	} {
		if !strings.Contains(conf, w) {
			t.Errorf("edge.conf lacks %q:\n%s", w, conf)
		}
	}
	if strings.Contains(conf, "tok-123") {
		t.Error("the token is in edge.conf, which is world-readable and rewritten")
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
	for _, w := range []string{"image: ghcr.io/versolauth/versola-edge:0.8.0", "container_name: versola-edge-eu-central", "edge-identity:/var/lib/versola-edge", "./token:/etc/versola-edge/token:ro", "./pins:/etc/versola-edge/pins:ro"} {
		if !strings.Contains(compose, w) {
			t.Errorf("compose.yml lacks %q:\n%s", w, compose)
		}
	}
	// 2 central pins, 1 auth pin.
	pins, _ := filepath.Glob(filepath.Join(dir, "pins", "*.crt"))
	if len(pins) != 3 {
		t.Errorf("pins = %v", pins)
	}
}

// The operator's files are theirs, a stale pin is not kept, and --version overrides.
func TestEdgeBootstrapKeepsTheOperatorsFilesAndReplacesPins(t *testing.T) {
	b := goodBundle(t)
	dir := t.TempDir()
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, b), Dir: dir, NoStart: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge.local.conf"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge.secrets.env"), []byte("MINE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.CentralPins = b.CentralPins[:1]
	b.AuthURL, b.AuthPins = "", nil
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, b), Dir: dir, NoStart: true, Version: "0.9.1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, dir, "edge.local.conf") != "mine\n" || readFile(t, dir, "edge.secrets.env") != "MINE=1\n" {
		t.Error("the operator's files were overwritten")
	}
	if pins, _ := filepath.Glob(filepath.Join(dir, "pins", "*.crt")); len(pins) != 1 {
		t.Errorf("stale pins left: %v", pins)
	}
	if !strings.Contains(readFile(t, dir, "compose.yml"), "versola-edge:0.9.1") {
		t.Error("--version ignored")
	}
	if strings.Contains(readFile(t, dir, "edge.conf"), "native") {
		t.Error("auth block left after the bundle dropped it")
	}
}

func TestEdgeBootstrapKeyFile(t *testing.T) {
	key, _ := EdgeKeyFile("eu-central", "k1", "PRIV_ATE-key_9")
	keyPath := filepath.Join(t.TempDir(), "k.json")
	_ = os.WriteFile(keyPath, key, 0o600)
	dir := t.TempDir()
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, goodBundle(t)), Dir: dir, KeyFile: keyPath, NoStart: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	env := readFile(t, dir, "edge.secrets.env")
	if !strings.Contains(env, "EDGE_KEY_ID=k1\n") || !strings.Contains(env, "EDGE_PRIVATE_KEY=PRIV_ATE-key_9\n") {
		t.Errorf("edge.secrets.env = %q", env)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"keyId":"k","privateKey":"a b"}`), 0o600)
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, goodBundle(t)), Dir: t.TempDir(), KeyFile: bad, NoStart: true}, time.Now()); err == nil {
		t.Error("a key that cannot be written to an env file was accepted")
	}
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, goodBundle(t)), Dir: t.TempDir(), KeyFile: filepath.Join(t.TempDir(), "none"), NoStart: true}, time.Now()); err == nil {
		t.Error("a missing key file")
	}
}

func TestEdgeBootstrapRefusesBadBundleAndVersion(t *testing.T) {
	b := goodBundle(t)
	b.VersolaVersion = ""
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, b), Dir: t.TempDir(), NoStart: true}, time.Now()); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Errorf("err = %v", err)
	}
	b.VersolaVersion = "0.8.0/../x"
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, b), Dir: t.TempDir(), NoStart: true}, time.Now()); err == nil {
		t.Error("a version that is a path")
	}
	dir := t.TempDir()
	b = goodBundle(t)
	b.TokenExpiresAt = time.Now().Add(-time.Hour)
	if _, err := EdgeBootstrap(EdgeBootstrapOptions{File: writeBundle(t, b), Dir: dir, NoStart: true}, time.Now()); err == nil {
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
