package deploy

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
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
	centralPins, authPins, err := pins()
	if err != nil {
		return EdgeAddResult{}, err
	}
	if st.MTLSDomain == "" {
		authPins = nil
	}

	res := EdgeAddResult{Bundle: BootstrapBundle{
		Version: BundleVersion, EdgeID: o.ID, Group: o.Group, VersolaVersion: st.Version,
		EnrollURL: "https://" + st.CentralDomain, CentralURL: "https://" + st.CentralMTLSDomain, CentralPins: centralPins,
	}}
	if st.MTLSDomain != "" {
		res.Bundle.AuthURL, res.Bundle.AuthPins = "https://"+st.MTLSDomain, authPins
	}
	if !o.Existing {
		if res.KeyID, res.PrivateKey, err = api.register(o.ID, o.Group); err != nil {
			return EdgeAddResult{}, err
		}
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
	if b.EnrollmentToken == "" {
		return BootstrapBundle{}, fmt.Errorf("the bundle has no enrollment token")
	}
	if !b.TokenExpiresAt.After(now) {
		return BootstrapBundle{}, fmt.Errorf("the enrollment token expired at %s -- on the central's machine, `versola edge add %s --existing` issues a new one", b.TokenExpiresAt.Local().Format(time.RFC3339), b.EdgeID)
	}
	for name, u := range map[string]string{"enrollUrl": b.EnrollURL, "centralMtlsUrl": b.CentralURL} {
		if err := httpsOrigin(name, u); err != nil {
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
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("the bundle's %s %q is not an https origin", name, raw)
	}
	if strings.ContainsAny(raw, "\"\\\n\r ") {
		return fmt.Errorf("the bundle's %s has characters a config cannot carry", name)
	}
	return nil
}

// edgeFiles renders what `edge bootstrap` puts in the edge's directory, keyed
// by name; modes are applied by the writer.
func edgeFiles(b BootstrapBundle, version string) (map[string][]byte, error) {
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
	var conf bytes.Buffer
	fmt.Fprintf(&conf, `# Generated by `+"`versola edge bootstrap`"+` from the bundle of edge %s -- rewritten by it,
# don't edit by hand: what is yours goes into edge.local.conf, which is included last.
#
# The edge connects to central over mutual TLS with the identity certificate
# it enrolls for the first time with the token below and renews on its own
# (at a third of its lifetime left); the key never leaves the identity directory.
# Central's and auth's listener certificates are pinned, not trusted through a CA.
central {
  url = %q
  enrollment {
    url        = %q
    token-file = %q
  }
  mutual-tls {
    trusted-certificates = [%s]
  }
}

identity {
  directory = %q
}
`, b.EdgeID, b.CentralURL, b.EnrollURL, edgeConfigDir+"/token", pinList("central", centralPins), edgeIdentityDir)
	if b.AuthURL != "" {
		fmt.Fprintf(&conf, `
native {
  auth-mutual-tls-url  = %q
  trusted-certificates = [%s]
}
`, b.AuthURL, pinList("auth", authPins))
	}
	fmt.Fprintf(&conf, "\ninclude file(%q)\n", edgeConfigDir+"/edge.local.conf")
	files["edge.conf"] = conf.Bytes()
	files["token"] = []byte(b.EnrollmentToken + "\n")
	files["compose.yml"] = []byte(edgeCompose(b.EdgeID, version))
	return files, nil
}

// edgeCompose is the edge's own compose file. host networking: the edge's
// ports are whatever its edge.local.conf's operator puts a proxy in front of.
func edgeCompose(id, version string) string {
	return fmt.Sprintf(`# Generated by `+"`versola edge bootstrap`"+` -- rewritten by it, don't edit by hand.
name: versola-edge-%[1]s

services:
  edge:
    image: ghcr.io/versolauth/versola-edge:%[2]s
    container_name: versola-edge-%[1]s
    restart: unless-stopped
    stop_grace_period: 20s
    network_mode: host
    environment:
      PORT: 8095
      DPORT: 8096
      CONFIG_PATH: /app/config/env.conf
      BIND_HOST: "127.0.0.1"
    # Secrets (the edge's key, see `+"`versola edge add`"+`) are the operator's, in this file.
    env_file:
      - path: ./edge.secrets.env
        required: false
    volumes:
      - ./edge.conf:/app/config/env.conf:ro
      - ./edge.local.conf:%[3]s/edge.local.conf:ro
      - ./token:%[3]s/token:ro
      - ./pins:%[3]s/pins:ro
      # The identity: key and certificate chain. Without it the edge enrolls
      # again at every start, and the token is spent -- keep it.
      - edge-identity:%[4]s

volumes:
  edge-identity:
`, id, version, edgeConfigDir, edgeIdentityDir)
}

// EdgeBootstrap puts the edge's configuration and state volume on this
// machine and starts it. Written under a 0700 directory, the token and the
// secrets 0600; nothing is overwritten that the operator owns
// (edge.local.conf, edge.secrets.env).
func EdgeBootstrap(o EdgeBootstrapOptions, now time.Time) (dir string, err error) {
	b, err := ReadBundle(o.File, now)
	if err != nil {
		return "", err
	}
	version := o.Version
	if version == "" {
		version = b.VersolaVersion
	}
	if version == "" || strings.ContainsAny(version, "/:@ \"\\\n") {
		return "", fmt.Errorf("the bundle names no Versola release (or an unusable one): pass --version")
	}
	files, err := edgeFiles(b, version)
	if err != nil {
		return "", err
	}
	dir = o.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".versola", "edge", b.EdgeID)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "pins"), 0o700); err != nil {
		return "", err
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
			return "", fmt.Errorf("couldn't write %s: %w", name, err)
		}
	}
	if err := ensureOwnerFile(filepath.Join(dir, "edge.local.conf"), "# Your own edge configuration (its public URL, keys, whatever the edge needs beyond\n# what `versola edge bootstrap` writes in edge.conf). Never overwritten.\n", 0o644); err != nil {
		return "", err
	}
	secrets := "# The edge's secrets. Never overwritten by `versola edge bootstrap`.\n"
	if o.KeyFile != "" {
		kv, err := keyFileEnv(o.KeyFile)
		if err != nil {
			return "", err
		}
		secrets += kv
	}
	if err := ensureOwnerFile(filepath.Join(dir, "edge.secrets.env"), secrets, 0o600); err != nil {
		return "", err
	}
	if o.KeyFile != "" {
		// A key file that was passed is the newest word on the secret.
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, "edge.secrets.env"), []byte(secrets), 0o600); err != nil {
			return "", err
		}
	}
	if o.NoStart {
		return dir, nil
	}
	if err := docker.Run("compose", "-f", filepath.Join(dir, "compose.yml"), "up", "-d"); err != nil {
		return dir, fmt.Errorf("couldn't start the edge: %w -- the files are in %s", err, dir)
	}
	return dir, nil
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

// keyFileEnv turns the key file `edge add` wrote into the edge's secret
// variables, never echoing them.
func keyFileEnv(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("couldn't read the key file: %w", err)
	}
	var k struct {
		KeyID      string `json:"keyId"`
		PrivateKey string `json:"privateKey"`
	}
	if err := json.Unmarshal(data, &k); err != nil || k.KeyID == "" || k.PrivateKey == "" {
		return "", fmt.Errorf("%s is not a key file `versola edge add` wrote", path)
	}
	if strings.ContainsAny(k.KeyID+k.PrivateKey, " \n\r\"'\\$") {
		return "", fmt.Errorf("%s holds a key that cannot be written to an env file", path)
	}
	return fmt.Sprintf("EDGE_KEY_ID=%s\nEDGE_PRIVATE_KEY=%s\n", k.KeyID, k.PrivateKey), nil
}
