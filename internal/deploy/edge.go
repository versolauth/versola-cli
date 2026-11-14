package deploy

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/fsutil"
	"github.com/versolauth/versola-cli/internal/state"
)

// BundleVersion is the layout of the bootstrap bundle `edge add` prints and
// `edge bootstrap` reads.
const BundleVersion = 1

// BootstrapBundle is what an operator carries to a machine that is to run an
// edge: who it is, a one-time token to get its first certificate with, where
// to get it and where to connect afterwards, and the certificates of
// central's and auth's listeners to pin (the edge does not trust a CA for
// them: a leaf of the deployment's CA is not valid for any other host).
//
// It holds nothing that can be used again: the token is spent by the first
// enrollment and expires an hour after it was issued. The key central
// registered the edge with (what the edge signs its sync requests with) is
// deliberately not in it: central shows it once, `edge add` writes it to a file
// of its own, and it travels by another channel.
type BootstrapBundle struct {
	Version int    `json:"version"`
	EdgeID  string `json:"edgeId"`
	Group   string `json:"group,omitempty"`
	// VersolaVersion is the release the central side runs: the edge is run
	// from the same.
	VersolaVersion string `json:"versolaVersion,omitempty"`
	// VersolaURL is auth's public URL (the deployment's --auth-url): the
	// edge's `versola-url`, where browsers are sent to sign in and the issuer
	// its tokens are checked against.
	VersolaURL string `json:"authUrl"`

	EnrollmentToken string    `json:"enrollmentToken"`
	TokenExpiresAt  time.Time `json:"tokenExpiresAt"`
	// EnrollURL is central.<domain>: plain TLS with a public certificate,
	// where the first certificate is asked for.
	EnrollURL string `json:"enrollUrl"`
	// CentralURL is central-mtls.<domain>, where the edge connects with it.
	CentralURL string `json:"centralMtlsUrl"`
	// CentralPins are the PEM certificates central's listener presents now
	// and next (see `certs renew`).
	CentralPins []string `json:"centralPins"`
	// AuthURL and AuthPins: auth's mutual-TLS listener (id-mtls.<domain>),
	// when the deployment publishes it.
	AuthURL  string   `json:"authMtlsUrl,omitempty"`
	AuthPins []string `json:"authPins,omitempty"`
}

var edgeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidEdgeID: central's EdgeId (OpenAPI: ^[a-z][a-z0-9-]*$), bounded: it
// ends up in file names and container names.
func ValidEdgeID(id string) error {
	if len(id) > 63 || !edgeIDPattern.MatchString(id) {
		return fmt.Errorf("edge id %q: use lowercase letters, digits and '-', starting with a letter, up to 63 characters (central's rule for an edge id)", id)
	}
	return nil
}

// edgeAdmin is the part of central's admin API `edge add` uses
// (/configuration/edges and /configuration/edges/enrollment-tokens), a seam so
// the command can be tested without a central.
type edgeAdmin interface {
	// register creates the edge, in group when it is not "". The key it
	// returns is shown once.
	register(id, group string) (keyID, privateKey string, err error)
	// issueToken creates a one-time enrollment token for an existing edge.
	issueToken(id string) (token string, expiresAt time.Time, err error)
	// groupOf is the group a registered edge is in ("" for the default
	// group); found is false when central has no such edge.
	groupOf(id string) (group string, found bool, err error)
}

// EdgeAddOptions: see `versola edge add`.
type EdgeAddOptions struct {
	ID    string
	Group string
	// Existing: the edge is already registered; only a new token is issued
	// (an edge that lost its identity volume enrolls again).
	Existing bool
}

// EdgeAddResult is what `edge add` hands the operator: the bundle, and the
// key central registered the edge with (nil for an Existing one).
type EdgeAddResult struct {
	Bundle BootstrapBundle
	// KeyID and PrivateKey: shown once by central, "" when the edge was not
	// registered by this call.
	KeyID, PrivateKey string
}

// pinReader reads the PEM certificates of central's and auth's listeners.
type pinReader func() (central, auth []string, err error)

// buildEdgeAdd is `edge add` without its I/O: validates, registers (or not),
// issues the token and assembles the bundle.
func buildEdgeAdd(st *state.State, o EdgeAddOptions, api edgeAdmin, pins pinReader) (EdgeAddResult, error) {
	if err := ValidEdgeID(o.ID); err != nil {
		return EdgeAddResult{}, err
	}
	if o.Group != "" {
		if err := ValidEdgeID(o.Group); err != nil {
			return EdgeAddResult{}, fmt.Errorf("--group: %w", err)
		}
	}
	if o.Existing && o.Group != "" {
		return EdgeAddResult{}, fmt.Errorf("--group is for registering a new edge: the group of an existing one cannot change")
	}
	if st == nil || st.Target != "vps" {
		return EdgeAddResult{}, fmt.Errorf("`versola edge add` is for a vps deployment (a remote edge connects to the one this machine runs); nothing is configured for vps here")
	}
	// A remote edge needs both names, and nothing else tells it where to go:
	// failing now, before an edge is registered, beats a bundle that cannot be used.
	if st.CentralDomain == "" || st.CentralMTLSDomain == "" {
		return EdgeAddResult{}, fmt.Errorf("a remote edge needs central's enrollment name and its mutual-TLS name: configure with --central-domain central.<domain> and --central-mtls-domain central-mtls.<domain> first (and `versola up`) -- see the README, \"Public names\"")
	}
	// The edge's versola-url: without it the edge cannot start, so a bundle
	// without it is refused here, before anything is registered.
	authURL := strings.TrimSuffix(st.AuthURL, "/")
	if err := checkOrigin(authURL, "https", "http"); err != nil {
		return EdgeAddResult{}, fmt.Errorf("the deployment's auth URL %q (--auth-url, the edge's versola-url) %v -- configure with --auth-url https://id.<domain> first", st.AuthURL, err)
	}
	centralPins, authPins, err := pins()
	if err != nil {
		return EdgeAddResult{}, err
	}
	if st.MTLSDomain == "" {
		authPins = nil
	}

	res := EdgeAddResult{Bundle: BootstrapBundle{
		Version: BundleVersion, EdgeID: o.ID, Group: o.Group, VersolaVersion: st.Version, VersolaURL: authURL,
		EnrollURL: "https://" + st.CentralDomain, CentralURL: "https://" + st.CentralMTLSDomain, CentralPins: centralPins,
	}}
	if st.MTLSDomain != "" {
		res.Bundle.AuthURL, res.Bundle.AuthPins = "https://"+st.MTLSDomain, authPins
	}
	if !o.Existing {
		if res.KeyID, res.PrivateKey, err = api.register(o.ID, o.Group); err != nil {
			return EdgeAddResult{}, err
		}
	} else {
		// The edge names its group when it asks for its certificate, and
		// central refuses another one than it was registered in: the bundle of
		// an existing edge carries the group central has for it, asked before
		// a token is spent on a bundle that cannot enroll.
		group, found, err := api.groupOf(o.ID)
		if err != nil {
			return EdgeAddResult{}, err
		}
		if !found {
			return EdgeAddResult{}, fmt.Errorf("central has no edge %s: register it with `versola edge add %s` (without --existing)", o.ID, o.ID)
		}
		res.Bundle.Group = group
	}
	token, expires, err := api.issueToken(o.ID)
	if err != nil {
		if !o.Existing {
			err = fmt.Errorf("%w -- the edge %s is registered; once central answers, `versola edge add %s --existing` issues its token (its key is written to the key file, central shows it once)", err, o.ID, o.ID)
			// The key central just showed must reach the caller, who writes it
			// before reporting the error: nothing else holds it.
			return EdgeAddResult{KeyID: res.KeyID, PrivateKey: res.PrivateKey}, err
		}
		return EdgeAddResult{}, err
	}
	res.Bundle.EnrollmentToken, res.Bundle.TokenExpiresAt = token, expires.UTC()
	return res, nil
}

// EncodeBundle is the bundle as the file `edge bootstrap` reads.
func EncodeBundle(b BootstrapBundle) ([]byte, error) {
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// EdgeKeyFile is the file the registration's key goes to: JSON with the key's
// id and the base64url PKCS#8 private key central returned.
func EdgeKeyFile(id, keyID, privateKey string) ([]byte, error) {
	out, err := json.MarshalIndent(map[string]string{"edgeId": id, "keyId": keyID, "privateKey": privateKey}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// EdgeAdd registers an edge with the central of the vps deployment st records
// -- over the admin API on this host, as the CLI is -- issues its enrollment
// token and returns the bootstrap bundle. The caller holds no lock: nothing in
// the deployment's state changes.
func EdgeAdd(st *state.State, o EdgeAddOptions) (EdgeAddResult, error) {
	if st == nil {
		return EdgeAddResult{}, fmt.Errorf("nothing has been configured yet")
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return EdgeAddResult{}, err
	}
	if !exists {
		return EdgeAddResult{}, fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}
	dir := filepath.Dir(composePath)
	topo, err := loadTopology(composePath)
	if err != nil {
		return EdgeAddResult{}, err
	}
	central, err := topo.Service(CentralService)
	if err != nil {
		return EdgeAddResult{}, err
	}
	secret, err := readEnvValue(filepath.Join(dir, "central.secrets.env"), "CENTRAL_RESOURCE_SECRET")
	if err != nil {
		return EdgeAddResult{}, err
	}
	api := &httpEdgeAdmin{base: fmt.Sprintf("http://127.0.0.1:%d", central.Port), password: secret, http: &http.Client{Timeout: 20 * time.Second}}
	return buildEdgeAdd(st, o, api, func() ([]string, []string, error) { return readPins(composePath) })
}

// readEnvValue reads one KEY=value from an env file (as Compose reads it:
// optional quotes), without ever printing it.
func readEnvValue(path, key string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("couldn't read %s: %w -- is this the machine central runs on, and has `versola configure` run?", filepath.Base(path), err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v == "" {
			break
		}
		return v, nil
	}
	return "", fmt.Errorf("%s has no %s", filepath.Base(path), key)
}

// readPins reads the certificates the listeners pin on, from the mutual-TLS
// volume through the compose project's own mtls-init image: the current leaf
// and, when it differs, the staged next one (see `certs renew`).
func readPins(composePath string) (central, auth []string, err error) {
	read := func(file string) (string, error) {
		args := state.ComposeArgs(composePath, "run", "--rm", "-T", "--no-deps", "--entrypoint", "cat", MTLSInitService, "/mtls/"+file)
		out, err := docker.Output(args...)
		if err != nil {
			return "", fmt.Errorf("couldn't read %s from the mutual-TLS volume: %w", file, err)
		}
		return string(out), nil
	}
	for _, g := range []struct {
		to    *[]string
		files []string
	}{{&central, []string{"central-server.crt", "central-server.next.crt"}}, {&auth, []string{"server.crt", "server.next.crt"}}} {
		var pems []string
		for i, f := range g.files {
			text, err := read(f)
			if err != nil {
				if i == 1 {
					continue // no staged one: from before the overlap, or nothing is due
				}
				return nil, nil, err
			}
			pems = append(pems, text)
		}
		if *g.to, err = normalizePins(pems); err != nil {
			return nil, nil, err
		}
	}
	return central, auth, nil
}

// normalizePins keeps each distinct certificate once, as a clean PEM block,
// and refuses anything that is not a certificate.
func normalizePins(inputs []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, in := range inputs {
		rest := []byte(strings.TrimSpace(in))
		found := 0
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				return nil, fmt.Errorf("a pin file holds a %q block, not a certificate", block.Type)
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, fmt.Errorf("a pin is not a certificate: %w", err)
			}
			found++
			sum := sha256.Sum256(block.Bytes)
			id := hex.EncodeToString(sum[:])
			if !seen[id] {
				seen[id] = true
				out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})))
			}
		}
		if found == 0 {
			return nil, fmt.Errorf("a pin file holds no certificate")
		}
	}
	return out, nil
}

// Fingerprint is the SHA-256 of a pin's DER, as hex pairs: what an operator
// compares by eye on both machines.
func Fingerprint(pemText string) string {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	pairs := make([]string, 0, len(h)/2)
	for i := 0; i+1 < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":")
}

// httpEdgeAdmin talks to central's admin API (HTTP Basic, user "central",
// the central resource secret as the password).
type httpEdgeAdmin struct {
	base, password string
	http           *http.Client
}

func (a *httpEdgeAdmin) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth("central", a.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach central at %s: %w -- is the deployment up (`versola status`)?", a.base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return adminError(method, path, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("central's answer to %s %s is not what the API documents: %w", method, path, err)
	}
	return nil
}

// adminError says what central refused. Its body is quoted only for a 4xx
// that is not a credentials error, bounded: it describes the request, which
// holds no secret but the token's response is never quoted.
func adminError(method, path string, status int, body []byte) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("central refused the admin credentials (%d) -- the CENTRAL_RESOURCE_SECRET in the deployment's central.secrets.env is not the one central has", status)
	case http.StatusConflict:
		return fmt.Errorf("central says it already exists (409) -- for an edge that is registered, use --existing to issue a new token")
	case http.StatusNotFound:
		return fmt.Errorf("central answers 404 for %s %s -- this release of Versola has no edge enrollment (it needs one with the enrollment API)", method, path)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 || status >= 500 {
		msg = ""
	}
	if msg != "" {
		return fmt.Errorf("central answered %d for %s %s: %s", status, method, path, msg)
	}
	return fmt.Errorf("central answered %d for %s %s", status, method, path)
}

func (a *httpEdgeAdmin) register(id, group string) (string, string, error) {
	req := map[string]string{"id": id}
	if group != "" {
		req["groupId"] = group
	}
	var resp struct {
		KeyID      string `json:"keyId"`
		PrivateKey string `json:"privateKey"`
	}
	if err := a.do(http.MethodPost, "/configuration/edges", req, &resp); err != nil {
		return "", "", err
	}
	// Central shows the key once: a 2xx without it is not a success to report, and
	// the body is not echoed (it may hold the half that did arrive).
	if resp.KeyID == "" || resp.PrivateKey == "" {
		return "", "", fmt.Errorf("central answered 2xx for POST /configuration/edges without a keyId and privateKey -- the edge %s may be registered, but its key was not delivered: delete it in central and add it again", id)
	}
	return resp.KeyID, resp.PrivateKey, nil
}

func (a *httpEdgeAdmin) groupOf(id string) (string, bool, error) {
	var resp struct {
		Edges []struct {
			ID        string `json:"id"`
			GroupID   string `json:"groupId"`
			GroupKind string `json:"groupKind"`
		} `json:"edges"`
	}
	if err := a.do(http.MethodGet, "/configuration/edges", nil, &resp); err != nil {
		return "", false, err
	}
	for _, e := range resp.Edges {
		if e.ID != id {
			continue
		}
		// A partner's edge runs another configuration (mode = "partner"),
		// which `edge bootstrap` does not write.
		if e.GroupKind != "" && e.GroupKind != "first_party" {
			return "", false, fmt.Errorf("the edge %s is in a partner's group (%s): a partner's edge is not set up with `versola edge bootstrap` (the partner runs `versola edge partner` with the enrollment token of the partner's edge)", id, e.GroupID)
		}
		if e.GroupID == "default" {
			return "", true, nil
		}
		if e.GroupID != "" {
			if err := ValidEdgeID(e.GroupID); err != nil {
				return "", false, fmt.Errorf("central's group for the edge %s: %w", id, err)
			}
		}
		return e.GroupID, true, nil
	}
	return "", false, nil
}

func (a *httpEdgeAdmin) issueToken(id string) (string, time.Time, error) {
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := a.do(http.MethodPost, "/configuration/edges/enrollment-tokens?edgeId="+url.QueryEscape(id), nil, &resp); err != nil {
		return "", time.Time{}, err
	}
	if resp.Token == "" {
		return "", time.Time{}, errors.New("central issued no token")
	}
	return resp.Token, resp.ExpiresAt, nil
}

// ---- edge bootstrap ----

// EdgeBootstrapOptions: see `versola edge bootstrap`.
type EdgeBootstrapOptions struct {
	// File is the bundle `edge add` printed.
	File string
	// Dir is where the edge's files go: ~/.versola/edge/<id> by default.
	Dir string
	// KeyFile is the registration's key file `edge add` wrote, "" when it is
	// not here yet (the edge's secrets file then lacks it, and says so).
	KeyFile string
	// Version overrides the release the bundle names.
	Version string
	// NoStart writes the files and stops.
	NoStart bool
	// EdgeURL is the origin clients reach this edge on (its `edge-url`).
	EdgeURL string
	// PostgresURL (JDBC) and PostgresUser are the edge's own database.
	PostgresURL, PostgresUser string
	// PostgresPasswordFile holds the database password, "" to keep (or
	// later add) POSTGRES_PASSWORD in edge.secrets.env by hand.
	PostgresPasswordFile string
}

// EdgeBootstrapResult says where the files went and whether the edge was
// started: it is not when edge.secrets.env still lacks something the edge
// cannot start without (Missing names it, a variable and how to supply it).
type EdgeBootstrapResult struct {
	Dir     string
	Started bool
	Missing []string
}

// edge's paths inside its container.
const (
	edgeConfigDir   = "/etc/versola-edge"
	edgeIdentityDir = "/var/lib/versola-edge"
)

// ReadBundle reads and validates a bootstrap bundle: a token that has
// expired or a URL that is not https is refused here, not by the edge a
// minute later.
func ReadBundle(path string, now time.Time) (BootstrapBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BootstrapBundle{}, fmt.Errorf("couldn't read the bundle: %w", err)
	}
	var b BootstrapBundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return BootstrapBundle{}, fmt.Errorf("%s is not a bootstrap bundle: %w", path, err)
	}
	if b.Version != BundleVersion {
		return BootstrapBundle{}, fmt.Errorf("%s is bundle version %d; this versola reads version %d", path, b.Version, BundleVersion)
	}
	if err := ValidEdgeID(b.EdgeID); err != nil {
		return BootstrapBundle{}, err
	}
	if b.Group != "" {
		if err := ValidEdgeID(b.Group); err != nil {
			return BootstrapBundle{}, fmt.Errorf("the bundle's group: %w", err)
		}
	}
	if b.EnrollmentToken == "" {
		return BootstrapBundle{}, fmt.Errorf("the bundle has no enrollment token")
	}
	if !b.TokenExpiresAt.After(now) {
		return BootstrapBundle{}, fmt.Errorf("the enrollment token expired at %s -- on the central's machine, `versola edge add %s --existing` issues a new one", b.TokenExpiresAt.Local().Format(time.RFC3339), b.EdgeID)
	}
	// A bundle from a versola before authUrl was carried: the edge cannot
	// start without auth's public URL, and nothing on this machine knows it.
	if b.VersolaURL == "" {
		return BootstrapBundle{}, fmt.Errorf("the bundle has no authUrl (auth's public URL, the edge's versola-url): it was printed by an older versola -- update versola on the central's machine and run `versola edge add %s --existing` there", b.EdgeID)
	}
	if err := checkOrigin(b.VersolaURL, "https", "http"); err != nil {
		return BootstrapBundle{}, fmt.Errorf("the bundle's authUrl %q %v", b.VersolaURL, err)
	}
	b.VersolaURL = strings.TrimSuffix(b.VersolaURL, "/")
	for _, f := range []struct{ name, url string }{{"enrollUrl", b.EnrollURL}, {"centralMtlsUrl", b.CentralURL}} {
		if err := httpsOrigin(f.name, f.url); err != nil {
			return BootstrapBundle{}, err
		}
	}
	if b.AuthURL != "" {
		if err := httpsOrigin("authMtlsUrl", b.AuthURL); err != nil {
			return BootstrapBundle{}, err
		}
	}
	if len(b.CentralPins) == 0 {
		return BootstrapBundle{}, fmt.Errorf("the bundle has no pin for central: the edge would have to trust whatever answers")
	}
	if b.AuthURL != "" && len(b.AuthPins) == 0 {
		return BootstrapBundle{}, fmt.Errorf("the bundle names auth's mutual-TLS address but has no pin for it")
	}
	if _, err := normalizePins(b.CentralPins); err != nil {
		return BootstrapBundle{}, fmt.Errorf("centralPins: %w", err)
	}
	if _, err := normalizePins(b.AuthPins); b.AuthURL != "" && err != nil {
		return BootstrapBundle{}, fmt.Errorf("authPins: %w", err)
	}
	return b, nil
}

// httpsOrigin: an https URL with a host, nothing else a config could carry.
func httpsOrigin(name, raw string) error {
	if err := checkOrigin(raw, "https"); err != nil {
		return fmt.Errorf("the bundle's %s %q %v", name, raw, err)
	}
	return nil
}

// checkOrigin: a URL of one of schemes with a host and nothing else (no
// path, query, fragment or user), and no character that could end the quoted
// HOCON string it is written into. The error reads after the value.
func checkOrigin(raw string, schemes ...string) error {
	if strings.ContainsAny(raw, "\"\\\n\r\t $") {
		return errors.New("has characters a config cannot carry")
	}
	u, err := url.Parse(raw)
	ok := err == nil && u.Hostname() != "" && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" && u.User == nil && !u.ForceQuery
	if ok {
		ok = false
		for _, s := range schemes {
			ok = ok || u.Scheme == s
		}
	}
	if !ok {
		return fmt.Errorf("is not an %s origin (%s://host[:port])", strings.Join(schemes, " or "), schemes[0])
	}
	return nil
}

var (
	pgUserPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.@-]{0,62}$`)
	// a password in the JDBC URL would land in edge.conf, which is 0644 and
	// rewritten: it belongs in edge.secrets.env.
	jdbcPasswordPattern = regexp.MustCompile(`(?i)[?&;](ssl)?(password|passwd)=`)
)

// checkSite validates what `edge bootstrap` is told about this machine,
// before the bundle is read and anything is written.
func checkSite(o EdgeBootstrapOptions) error {
	if o.EdgeURL == "" {
		return fmt.Errorf("--edge-url is required: the public origin clients reach this edge on (https://edge.<domain>), its edge-url")
	}
	if err := checkOrigin(o.EdgeURL, "https", "http"); err != nil {
		return fmt.Errorf("--edge-url %q %v", o.EdgeURL, err)
	}
	return checkPostgres(o.PostgresURL, o.PostgresUser)
}

// checkPostgres validates the edge's own database as `edge bootstrap` and
// `edge partner` are told it: a JDBC URL without a password, and a user.
func checkPostgres(postgresURL, postgresUser string) error {
	if postgresURL == "" {
		return fmt.Errorf("--postgres-url is required: the JDBC URL of the edge's own database (jdbc:postgresql://<host>:5432/<db>?sslmode=verify-full)")
	}
	// Not echoed: whatever it holds is the operator's, and may be a secret.
	authority := strings.TrimPrefix(postgresURL, "jdbc:postgresql://")
	if i := strings.IndexAny(authority, "/?"); i >= 0 {
		authority = authority[:i]
	}
	if jdbcPasswordPattern.MatchString(postgresURL) || strings.Contains(authority, "@") {
		return fmt.Errorf("--postgres-url holds a password: pass it with --postgres-password-file instead (it goes into edge.secrets.env, not the world-readable edge.conf)")
	}
	if !strings.HasPrefix(postgresURL, "jdbc:postgresql://") || strings.ContainsAny(postgresURL, "\"\\\n\r\t $") {
		return fmt.Errorf("--postgres-url is not a JDBC URL a config can carry: jdbc:postgresql://<host>:5432/<db>?sslmode=verify-full, without quotes, spaces or '$'")
	}
	if !pgUserPattern.MatchString(postgresUser) {
		return fmt.Errorf("--postgres-user %q: use letters, digits, '_', '.', '@' and '-', starting with a letter or '_', up to 63 characters", postgresUser)
	}
	return nil
}

// edgeSite is what the bundle cannot know: this machine's side of the edge.
type edgeSite struct {
	EdgeURL, PostgresURL, PostgresUser string
}

// edgeFiles renders what `edge bootstrap` puts in the edge's directory, keyed
// by name; modes are applied by the writer. edge.conf is complete: every key
// the edge requires is in it, the secrets as ${VAR} substitutions that
// edge.secrets.env supplies through the container's environment.
func edgeFiles(b BootstrapBundle, version string, site edgeSite) (map[string][]byte, error) {
	centralPins, err := normalizePins(b.CentralPins)
	if err != nil {
		return nil, err
	}
	var authPins []string
	if b.AuthURL != "" {
		if authPins, err = normalizePins(b.AuthPins); err != nil {
			return nil, err
		}
	}
	files := map[string][]byte{}
	pinList := func(prefix string, pins []string) string {
		var paths []string
		for i, p := range pins {
			name := fmt.Sprintf("pins/%s-%d.crt", prefix, i+1)
			files[name] = []byte(p)
			paths = append(paths, fmt.Sprintf("%q", edgeConfigDir+"/"+name))
		}
		return strings.Join(paths, ", ")
	}
	// Central refuses the certificate request of an edge that names another
	// group than the one it was registered in (422); the edge's own default
	// is "default", which is what an edge without --group was registered in.
	group := ""
	if b.Group != "" {
		group = fmt.Sprintf("    group     = %q\n", b.Group)
	}
	var conf bytes.Buffer
	fmt.Fprintf(&conf, `# Generated by `+"`versola edge bootstrap`"+` from the bundle of edge %[1]s -- rewritten by it,
# don't edit by hand: what is yours goes into edge.local.conf, which is included last
# and overrides anything here. The ${...} values are the edge's secrets, from
# edge.secrets.env through the container's environment.

env = prod
id = %[2]q
key-id = ${EDGE_KEY_ID}
private-key = ${EDGE_PRIVATE_KEY}

configuration-cache-refresh-interval = "5 minutes"

# Auth's public URL: where browsers are sent to sign in, and the issuer the
# edge checks tokens against.
versola-url = %[3]q
# The origin clients reach this edge on -- what a DPoP proof's htu is
# rebuilt against, not trusting a forwarded Host header.
edge-url = %[4]q

# The edge connects to central over mutual TLS with the identity certificate
# it enrolls for the first time with the token below and renews on its own
# (at a third of its lifetime left); the key never leaves the identity directory.
# Central's and auth's listener certificates are pinned, not trusted through a CA.
central {
  url = %[5]q
  mutual-tls {
    trusted-certificates = [%[6]s]
  }
  identity {
    directory = %[7]q
%[8]s    enrollment {
      url        = %[9]q
      token-file = %[10]q
    }
  }
}

%[11]s`, b.EdgeID, b.EdgeID, b.VersolaURL, site.EdgeURL,
		b.CentralURL, pinList("central", centralPins), edgeIdentityDir, group, b.EnrollURL, edgeConfigDir+"/token",
		edgeCommonConf(site.PostgresURL, site.PostgresUser))
	if b.AuthURL != "" {
		// The native flow is DPoP end to end: the edge refuses a native block
		// without a dpop one.
		fmt.Fprintf(&conf, `
# The native-app back channel, straight to auth's mutual-TLS listener. The pin
# is a list so auth's listener certificate can be rotated with an overlap window.
native {
  auth-mutual-tls-url  = %q
  trusted-certificates = [%s]
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
`, b.AuthURL, pinList("auth", authPins))
	}
	fmt.Fprintf(&conf, "\ninclude file(%q)\n", edgeConfigDir+"/edge.local.conf")
	files["edge.conf"] = conf.Bytes()
	files["token"] = []byte(b.EnrollmentToken + "\n")
	files["compose.yml"] = []byte(edgeCompose(b.EdgeID, version))
	return files, nil
}

// edgeCommonConf is the part of edge.conf both kinds of edge share: its own
// keys, its database and the cleanup of its tables.
func edgeCommonConf(postgresURL, postgresUser string) string {
	return fmt.Sprintf(`security {
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
  url = %[1]q
  user = %[2]q
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
`, postgresURL, postgresUser)
}

// edgeCompose is the edge's own compose file. host networking: the edge's
// ports are whatever its edge.local.conf's operator puts a proxy in front of.
func edgeCompose(id, version string) string {
	return composeForEdge("`versola edge bootstrap`", "versola-edge-"+id, version,
		"Secrets (the edge's key, see `versola edge add`, its own keys and the database password).", "")
}

// composeForEdge is the compose file of one edge, of either kind: generator
// names the command that writes it, project is the Compose project and
// container name, secrets the comment on edge.secrets.env, and extraVolumes
// more entries of the edge's volumes list (each line indented and ending in
// a newline), mounted before the identity volume.
func composeForEdge(generator, project, version, secrets, extraVolumes string) string {
	return fmt.Sprintf(`# Generated by %[5]s -- rewritten by it, don't edit by hand.
name: %[1]s

services:
  edge:
    image: ghcr.io/versolauth/versola-edge:%[2]s
    container_name: %[1]s
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
    # %[6]s
    env_file:
      - path: ./edge.secrets.env
        required: false
    volumes:
      - ./edge.conf:/app/config/env.conf:ro
      - ./edge.local.conf:%[3]s/edge.local.conf:ro
      - ./token:%[3]s/token:ro
      - ./pins:%[3]s/pins:ro
%[7]s      # The identity: key and certificate chain. Without it the edge enrolls
      # again at every start, and the token is spent -- keep it.
      - edge-identity:%[4]s

volumes:
  edge-identity:
`, project, version, edgeConfigDir, edgeIdentityDir, generator, secrets, extraVolumes)
}

// edgeGeneratedSecrets are the edge's own keys: 32 random bytes each,
// generated once into edge.secrets.env and never replaced there --
// EDGE_TOKEN_ENC_KEY encrypts the sessions the edge stores, so a new one
// makes every stored session unreadable.
var edgeGeneratedSecrets = []string{"EDGE_TOKEN_ENC_KEY", "EDGE_SESSIONS_SECRET", "EDGE_DPOP_NONCE_SALT", "EDGE_NATIVE_BLOB_KEY"}

const edgeSecretsHeader = "# The edge's secrets. `versola edge bootstrap` keeps every line here: it adds the edge's\n" +
	"# own keys when they are missing, and replaces EDGE_KEY_ID/EDGE_PRIVATE_KEY only with\n" +
	"# --key-file and POSTGRES_PASSWORD only with --postgres-password-file.\n"

// EdgeBootstrap puts the edge's configuration and state volume on this
// machine and starts it. Written under a 0700 directory, the token and the
// secrets 0600; nothing is overwritten that the operator owns
// (edge.local.conf; edge.secrets.env is merged, never replaced).
func EdgeBootstrap(o EdgeBootstrapOptions, now time.Time) (EdgeBootstrapResult, error) {
	o.EdgeURL = strings.TrimSuffix(o.EdgeURL, "/")
	if err := checkSite(o); err != nil {
		return EdgeBootstrapResult{}, err
	}
	b, err := ReadBundle(o.File, now)
	if err != nil {
		return EdgeBootstrapResult{}, err
	}
	version := o.Version
	if version == "" {
		version = b.VersolaVersion
	}
	if version == "" || strings.ContainsAny(version, "/:@ \"\\\n") {
		return EdgeBootstrapResult{}, fmt.Errorf("the bundle names no Versola release (or an unusable one): pass --version")
	}
	files, err := edgeFiles(b, version, edgeSite{EdgeURL: o.EdgeURL, PostgresURL: o.PostgresURL, PostgresUser: o.PostgresUser})
	if err != nil {
		return EdgeBootstrapResult{}, err
	}
	// Everything that is read from the operator's files is read before
	// anything is written: a bad key or password file leaves no half-made
	// directory behind.
	var set []envEntry
	if o.KeyFile != "" {
		keyID, privateKey, err := keyFileEnv(o.KeyFile)
		if err != nil {
			return EdgeBootstrapResult{}, err
		}
		set = append(set, envEntry{"EDGE_KEY_ID", keyID, true}, envEntry{"EDGE_PRIVATE_KEY", privateKey, true})
	}
	if o.PostgresPasswordFile != "" {
		pw, err := readPasswordFile(o.PostgresPasswordFile)
		if err != nil {
			return EdgeBootstrapResult{}, err
		}
		set = append(set, envEntry{"POSTGRES_PASSWORD", pw, true})
	}
	for _, name := range edgeGeneratedSecrets {
		v, err := randomKey()
		if err != nil {
			return EdgeBootstrapResult{}, err
		}
		set = append(set, envEntry{name, v, false})
	}

	dir := o.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return EdgeBootstrapResult{}, err
		}
		dir = filepath.Join(home, ".versola", "edge", b.EdgeID)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return EdgeBootstrapResult{}, err
	}
	res := EdgeBootstrapResult{Dir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "pins"), 0o700); err != nil {
		return res, err
	}
	// pins/ is replaced as a whole: a pin that is gone from the bundle must not linger.
	old, _ := filepath.Glob(filepath.Join(dir, "pins", "*.crt"))
	for _, f := range old {
		_ = os.Remove(f)
	}
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "token" {
			mode = 0o600
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, name), content, mode); err != nil {
			return res, fmt.Errorf("couldn't write %s: %w", name, err)
		}
	}
	if err := ensureOwnerFile(filepath.Join(dir, "edge.local.conf"), "# Your own edge configuration: edge.conf includes this file last, so what is set here\n# overrides what `versola edge bootstrap` generates there. Never overwritten.\n#\n# Behind a reverse proxy, and only if the edge is reachable solely through it, read the\n# caller's address for the rate limits from the proxy's header:\n# client-ip = \"forwarded\"\n", 0o644); err != nil {
		return res, err
	}
	secretsPath := filepath.Join(dir, "edge.secrets.env")
	existing, err := os.ReadFile(secretsPath)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("couldn't read edge.secrets.env: %w", err)
	}
	if os.IsNotExist(err) {
		existing = []byte(edgeSecretsHeader)
	}
	// An earlier versola wrote the key as central hands it out (base64url),
	// which the edge cannot read: re-encoded in place, never printed.
	if o.KeyFile == "" {
		if v := envLastValue(string(existing), "EDGE_PRIVATE_KEY"); strings.ContainsAny(v, "-_") {
			if std, err := standardBase64(v); err == nil {
				set = append(set, envEntry{"EDGE_PRIVATE_KEY", std, true})
			}
		}
	}
	merged := mergeEnv(string(existing), set)
	if err := fsutil.WriteFileAtomic(secretsPath, []byte(merged), 0o600); err != nil {
		return res, fmt.Errorf("couldn't write edge.secrets.env: %w", err)
	}
	if !envHasValue(merged, "EDGE_KEY_ID") || !envHasValue(merged, "EDGE_PRIVATE_KEY") {
		res.Missing = append(res.Missing, "EDGE_KEY_ID/EDGE_PRIVATE_KEY (the key `versola edge add` wrote: pass --key-file <id>.edge-key.json)")
	}
	if !envHasValue(merged, "POSTGRES_PASSWORD") {
		res.Missing = append(res.Missing, "POSTGRES_PASSWORD (the edge's database password: pass --postgres-password-file <file>)")
	}
	// An edge started without them stops at once and is restarted again and
	// again (restart: unless-stopped): not started until they are there.
	if o.NoStart || len(res.Missing) > 0 {
		return res, nil
	}
	// --force-recreate: edge.conf is bind-mounted and replaced by a rename,
	// which Compose does not see as a change; a re-run with other values must
	// restart the edge on them. Its state is in the identity volume, kept.
	if err := docker.Run("compose", "-f", filepath.Join(dir, "compose.yml"), "up", "-d", "--force-recreate"); err != nil {
		return res, fmt.Errorf("couldn't start the edge: %w -- the files are in %s", err, dir)
	}
	res.Started = true
	return res, nil
}

// ensureOwnerFile creates a file that is the operator's, and leaves it alone
// when it exists.
func ensureOwnerFile(path, content string, mode os.FileMode) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(content), mode)
}

// envEntry is a variable `edge bootstrap` puts into edge.secrets.env:
// replace says whether a value already there gives way to this one (the key
// and password the operator just passed) or is kept (generated keys).
type envEntry struct {
	key, value string
	replace    bool
}

// envLineKey is the variable a line of an env file sets, "" for a comment
// or a blank line.
func envLineKey(line string) string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return ""
	}
	t = strings.TrimPrefix(t, "export ")
	k, _, ok := strings.Cut(t, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(k)
}

// envHasValue: content sets key to something that is not empty.
func envHasValue(content, key string) bool {
	return envLastValue(content, key) != ""
}

// envLastValue is the value content sets key to, unquoted: that of the last
// line for it, the one Compose uses.
func envLastValue(content, key string) string {
	value := ""
	for _, line := range strings.Split(content, "\n") {
		if envLineKey(line) != key {
			continue
		}
		_, v, _ := strings.Cut(line, "=")
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		value = v
	}
	return value
}

// mergeEnv keeps every line of content and puts each entry in: a line that
// sets its key is rewritten in place when the entry replaces, or when the
// value there is empty; otherwise it is kept. A key with no line is
// appended.
func mergeEnv(content string, entries []envEntry) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, e := range entries {
		seen := false
		// A key kept is kept on every line: with a duplicate left empty,
		// rewriting that one would change what Compose uses (the last line).
		keep := false
		for _, line := range lines {
			keep = keep || (!e.replace && envLineKey(line) == e.key && envHasValue(line, e.key))
		}
		for i, line := range lines {
			if envLineKey(line) != e.key {
				continue
			}
			seen = true
			if !keep {
				lines[i] = e.key + "=" + e.value
			}
		}
		if !seen {
			lines = append(lines, e.key+"="+e.value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// randomKey is 32 random bytes, base64url without padding: what the edge
// reads as a 32-byte secret.
func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// readPasswordFile reads the database password, without its final newline.
// It goes into edge.secrets.env single-quoted (taken literally by Compose),
// so a quote or a line break cannot be carried. Never echoed.
func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("couldn't read the --postgres-password-file: %w", err)
	}
	pw := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if pw == "" {
		return "", fmt.Errorf("--postgres-password-file %s is empty", path)
	}
	if strings.ContainsAny(pw, "'\n\r\x00") {
		return "", fmt.Errorf("--postgres-password-file %s holds a password with a quote (') or a line break, which edge.secrets.env cannot carry: choose another password", path)
	}
	return "'" + pw + "'", nil
}

// keyFileEnv reads the key file `edge add` wrote, never echoing it: the key
// id, and the private key as the edge reads it. Central hands the key out as
// base64url PKCS#8; the edge decodes private-key with the standard base64
// alphabet, which refuses '-' and '_', so it is re-encoded (padded) here.
func keyFileEnv(path string) (keyID, privateKey string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("couldn't read the key file: %w", err)
	}
	var k struct {
		KeyID      string `json:"keyId"`
		PrivateKey string `json:"privateKey"`
	}
	if err := json.Unmarshal(data, &k); err != nil || k.KeyID == "" || k.PrivateKey == "" {
		return "", "", fmt.Errorf("%s is not a key file `versola edge add` wrote", path)
	}
	if strings.ContainsAny(k.KeyID, " \n\r\"'\\$#=") {
		return "", "", fmt.Errorf("%s holds a key id that cannot be written to an env file", path)
	}
	std, err := standardBase64(k.PrivateKey)
	if err != nil {
		return "", "", fmt.Errorf("%s holds a private key that is not base64 PKCS#8 RSA: %w", path, err)
	}
	return k.KeyID, std, nil
}

// standardBase64 re-encodes a base64url (or standard) PKCS#8 RSA private key
// in the standard alphabet, padded. The key is parsed: a value that is not
// one is refused here rather than by the edge at start.
func standardBase64(s string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(s), "=")
	der, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		if der, err = base64.RawStdEncoding.DecodeString(trimmed); err != nil {
			return "", errors.New("not base64")
		}
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return "", errors.New("not a PKCS#8 private key")
	}
	if _, ok := key.(*rsa.PrivateKey); !ok {
		return "", errors.New("not an RSA key")
	}
	return base64.StdEncoding.EncodeToString(der), nil
}
