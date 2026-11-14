package deploy

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/fsutil"
)

// A partner's edge (`versola edge partner`) is run by an organisation outside
// the operator, on its own host and with its own Postgres. It has no bundle
// and no connection to the operator's machine: what the operator hands over
// is an enrollment token (one string, one use, 24 hours), the certificates of
// central's and auth's mutual-TLS listeners, and the console's files. The
// edge's id, key, group and the addresses of central and auth are told to it
// by central at enrollment and kept in its identity directory; edge.conf must
// not name them (the edge refuses each at start).

// EnrollmentTokenPrefix is the version prefix of central's enrollment token,
// `vtk1.<base64url(enroll address)>.<secret>` (versola's EnrollmentToken).
const EnrollmentTokenPrefix = "vtk1"

// ParseEnrollmentToken checks a token as central's own parser does, and
// returns the address it enrolls at. No error quotes the token or its secret:
// only what is wrong, and the address, which is not a secret.
func ParseEnrollmentToken(raw string) (address string, err error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", fmt.Errorf("it is empty")
	}
	if !strings.HasPrefix(token, EnrollmentTokenPrefix+".") {
		return "", fmt.Errorf("it does not start with '%s.': it is not an enrollment token", EnrollmentTokenPrefix)
	}
	if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("it holds more than one token, or a token with spaces in it: the file holds the token alone")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("the token is malformed: it is %s.<address>.<secret>", EnrollmentTokenPrefix)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("the address in the token is not base64url")
	}
	address = string(decoded)
	u, err := url.Parse(address)
	if err != nil || strings.IndexFunc(address, unicode.IsControl) >= 0 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		shown := address
		if len(shown) > 200 || strings.IndexFunc(shown, unicode.IsControl) >= 0 {
			shown = "(unreadable)"
		}
		return "", fmt.Errorf("the address in the token, %q, is not an https URL: the edge sends the token there, and refuses anything but https", shown)
	}
	return address, nil
}

var (
	// partner.admins as the edge reads them (versola's Identifiers.parse): an
	// email address, lowercased, or a phone number in international format
	// once spaces, hyphens and parentheses are taken out.
	adminPhonePattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	adminEmailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// normalizeAdmin is one --admin in the form the edge keeps it: an email
// lowercased, a phone number without separators.
func normalizeAdmin(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("--admin is blank: give a phone number (+77010000000) or an email address")
	}
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("--admin %q holds a control character", v)
	}
	if strings.Contains(v, "@") {
		v = strings.ToLower(v)
		if !adminEmailPattern.MatchString(v) {
			return "", fmt.Errorf("--admin %q is not an email address", raw)
		}
		return v, nil
	}
	v = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '(' || r == ')' {
			return -1
		}
		return r
	}, v)
	if !adminPhonePattern.MatchString(v) {
		return "", fmt.Errorf("--admin %q is neither an email address nor a phone number in international format (+ and 7 to 15 digits, the first not 0, e.g. +7 701 000 00 00)", raw)
	}
	return v, nil
}

// hoconString quotes s as a HOCON (JSON) string. s holds no control
// character (normalizeAdmin refuses them).
func hoconString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// readLeafPins reads the pin files of one listener: each holds PEM
// certificates, none of them a certificate authority -- the edge refuses a
// CA as a pin, since it does no hostname check on these connections and a CA
// would let any certificate it issued stand in for the listener.
func readLeafPins(flag string, files []string) ([]string, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("%s is required: the certificate the listener presents, as the operator handed it over (a PEM file; repeat the flag for the next one during a rotation)", flag)
	}
	var all []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: couldn't read %s: %w", flag, f, err)
		}
		pins, err := normalizePins([]string{string(data)})
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", flag, f, err)
		}
		for _, p := range pins {
			block, _ := pem.Decode([]byte(p))
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", flag, f, err)
			}
			if cert.IsCA {
				return nil, fmt.Errorf("%s %s holds a certificate authority (%s), not the listener's own certificate: the edge refuses a CA as a pin -- ask the operator for the leaf certificate the listener presents", flag, f, cert.Subject.String())
			}
		}
		all = append(all, pins...)
	}
	return normalizePins(all)
}

// The files of the console the edge serves under /partner/ (versola's
// PartnerConsole): partner.html is what makes /partner/ answer; the scripts
// are what makes it work.
const partnerConsolePage = "partner.html"

var partnerConsoleScripts = []string{"versola-partner.js", "versola-partner-theme.js"}

// checkConsoleDir: the directory holds partner.html; a missing script is a
// warning (the page loads, and does nothing useful without it).
func checkConsoleDir(dir string) (abs string, warnings []string, err error) {
	abs, err = filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", nil, fmt.Errorf("--console-dir %s is not a directory: it is where the console files the operator handed over are (%s, %s)", dir, partnerConsolePage, strings.Join(partnerConsoleScripts, ", "))
	}
	if st, err := os.Stat(filepath.Join(abs, partnerConsolePage)); err != nil || !st.Mode().IsRegular() {
		return "", nil, fmt.Errorf("--console-dir %s has no %s: it is the directory of the console files the operator handed over", dir, partnerConsolePage)
	}
	for _, s := range partnerConsoleScripts {
		if st, err := os.Stat(filepath.Join(abs, s)); err != nil || !st.Mode().IsRegular() {
			warnings = append(warnings, fmt.Sprintf("--console-dir %s has no %s: the console page will load but not work until it is there", dir, s))
		}
	}
	return abs, warnings, nil
}

// EdgePartnerOptions: see `versola edge partner`.
type EdgePartnerOptions struct {
	// TokenFile holds the enrollment token; "" on a later run keeps the
	// edge's token file as it is.
	TokenFile string
	// Version is the Versola release of the edge's image.
	Version string
	// EdgeURL is the edge's public https origin (the partner console's URL).
	EdgeURL string
	// CentralPinFiles and AuthPinFiles: the certificates of central's and
	// auth's mutual-TLS listeners.
	CentralPinFiles, AuthPinFiles []string
	// Admins are the console's first administrators: phone numbers or emails.
	Admins []string
	// PostgresURL (JDBC), PostgresUser and PostgresPasswordFile: the edge's
	// own database.
	PostgresURL, PostgresUser, PostgresPasswordFile string
	// ConsoleDir holds the console's files, "" for none.
	ConsoleDir string
	// Dir is where the edge's files go: ~/.versola/edge/<host of EdgeURL> by default.
	Dir string
	// NoStart writes the files and stops.
	NoStart bool
}

// EdgePartnerResult says where the files went, what the edge is, and whether
// it was started (see EdgeBootstrapResult).
type EdgePartnerResult struct {
	Dir, Project string
	Started      bool
	Missing      []string
	// Warnings are about the console's files.
	Warnings []string
	// EnrollAddress is where the token enrolls ("" when no token was given).
	EnrollAddress string
	// CentralPins and AuthPins are the PEM pins, for their fingerprints.
	CentralPins, AuthPins []string
}

// partnerConfHeader starts every edge.conf `edge partner` writes; a directory
// whose edge.conf starts otherwise holds another edge and is not taken over.
const partnerConfHeader = "# Generated by `versola edge partner`"

// partnerGeneratedSecrets are a partner edge's own keys, generated once and
// never replaced: EDGE_TOKEN_ENC_KEY encrypts the stored sessions and keys
// the team's identifiers, so a new one loses both.
var partnerGeneratedSecrets = []string{"EDGE_TOKEN_ENC_KEY", "EDGE_SESSIONS_SECRET"}

const partnerSecretsHeader = "# The edge's secrets. `versola edge partner` keeps every line here: it adds the edge's\n" +
	"# own keys when they are missing (and never replaces them: EDGE_TOKEN_ENC_KEY keys the\n" +
	"# stored sessions and the team), and replaces POSTGRES_PASSWORD only with --postgres-password-file.\n"

const partnerLocalConf = "# Your own edge configuration: edge.conf includes this file last, so what is set here\n" +
	"# overrides what `versola edge partner` generates there. Never overwritten.\n" +
	"#\n" +
	"# Behind a reverse proxy, and only if the edge is reachable solely through it, read the\n" +
	"# caller's address for the rate limits from the proxy's header:\n" +
	"# client-ip = \"forwarded\"\n"

// partnerSite is what edge.conf is made of, validated.
type partnerSite struct {
	EdgeURL                   string
	CentralPins, AuthPins     []string
	Admins                    []string
	PostgresURL, PostgresUser string
	Console                   bool
}

// partnerEdgeFiles renders edge.conf and the pins of a partner's edge. Every
// key the edge requires in partner mode is there; none of those central tells
// it at enrollment (id, key-id, private-key, versola-url, central.url,
// central.identity.group, the enrollment url) nor what a partner edge refuses
// (security.internal-secret, revocation.feed) or has no use for (native, dpop).
func partnerEdgeFiles(site partnerSite) map[string][]byte {
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
	admins := make([]string, len(site.Admins))
	for i, a := range site.Admins {
		admins[i] = hoconString(a)
	}
	console := ""
	if site.Console {
		console = fmt.Sprintf("  # The console's files (partner.html, versola-partner*.js), served under /partner/.\n  console-directory = %q\n", partnerConsoleMount)
	}
	var conf bytes.Buffer
	fmt.Fprintf(&conf, partnerConfHeader+` -- rewritten by it, don't edit by hand:
# what is yours goes into edge.local.conf, which is included last and overrides anything
# here. The ${...} values are the edge's secrets, from edge.secrets.env through the
# container's environment. This edge's id, its key, its group and the addresses of
# central and auth are not here: central tells them at enrollment, and the edge keeps
# them in central.identity.directory.

mode = "partner"
env = prod

# The origin clients and the console reach this edge on (TLS ends at your proxy).
edge-url = %[1]q

configuration-cache-refresh-interval = "5 minutes"

# The certificate(s) auth's mutual-TLS address presents, pinned (not trusted through a
# CA); several only to overlap a rotation.
versola-internal-trusted-certificates = [%[2]s]

# The edge enrolls once with the token below and keeps the certificate central issues,
# and renews, in the identity directory: without that directory it would need a new
# token at every start. Central's mutual-TLS listener is pinned like auth's.
central {
  mutual-tls {
    trusted-certificates = [%[3]s]
  }
  identity {
    directory = %[4]q
    enrollment {
      token-file = %[5]q
    }
  }
}

partner {
  # The console's first administrators, made invites at every start: each signs in
  # at the operator's auth with this phone number or email, verified.
  admins = [%[6]s]
%[7]s}

%[8]s`, site.EdgeURL, pinList("auth", site.AuthPins), pinList("central", site.CentralPins), edgeIdentityDir, edgeConfigDir+"/token",
		strings.Join(admins, ", "), console, edgeCommonConf(site.PostgresURL, site.PostgresUser))
	fmt.Fprintf(&conf, "\ninclude file(%q)\n", edgeConfigDir+"/edge.local.conf")
	files["edge.conf"] = conf.Bytes()
	return files
}

// partnerConsoleMount is where the console's files are inside the container.
const partnerConsoleMount = "/app/console"

// partnerEdgeCompose is the compose file of a partner's edge: the
// first-party one's shape, plus the console's directory, read-only.
func partnerEdgeCompose(project, version, consoleDir string) string {
	extra := ""
	if consoleDir != "" {
		// Long syntax, the path JSON-quoted (a YAML double-quoted string): a
		// path with ':' or spaces would break the short form.
		src, _ := json.Marshal(consoleDir)
		extra = fmt.Sprintf("      # The partner console's files (only partner.html and versola-partner*.js|css are served).\n"+
			"      - type: bind\n        source: %s\n        target: %s\n        read_only: true\n", src, partnerConsoleMount)
	}
	return composeForEdge("`versola edge partner`", project, version,
		"Secrets (the edge's own keys and the database password).", extra)
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9.-]+`)

// partnerDirName is the default directory name of a partner's edge: the
// host of its URL. A first-party edge's id has no '.', so the two never
// share a directory under ~/.versola/edge.
func partnerDirName(edgeURL string) string {
	u, _ := url.Parse(edgeURL)
	name := strings.Trim(nonNameChars.ReplaceAllString(strings.ToLower(u.Hostname()), "-"), ".-")
	if name == "" {
		name = "partner"
	}
	return name
}

var nonProjectChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// partnerProject is the Compose project (and container) name of the edge in
// dir: Compose takes lowercase letters, digits, '-' and '_'.
func partnerProject(dir string) string {
	name := strings.Trim(nonProjectChars.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-"), "-_")
	if name == "" {
		name = "edge"
	}
	return "versola-partner-edge-" + name
}

// EdgePartner writes a partner's edge's directory and starts it. Everything
// the partner passed is checked before anything is written; nothing the
// partner owns is overwritten (edge.local.conf; edge.secrets.env is merged).
func EdgePartner(o EdgePartnerOptions) (EdgePartnerResult, error) {
	o.EdgeURL = strings.TrimSuffix(o.EdgeURL, "/")
	if o.EdgeURL == "" {
		return EdgePartnerResult{}, fmt.Errorf("--edge-url is required: the https origin the edge and its console are reached on (https://login.<your domain>)")
	}
	if err := checkOrigin(o.EdgeURL, "https"); err != nil {
		return EdgePartnerResult{}, fmt.Errorf("--edge-url %q %v: a partner's console runs over TLS, which your reverse proxy ends", o.EdgeURL, err)
	}
	if o.Version == "" || strings.ContainsAny(o.Version, "/:@ \"\\\n\r\t") {
		return EdgePartnerResult{}, fmt.Errorf("--version is required: the Versola release of the edge, as the operator told you (e.g. 0.8.0)")
	}
	if err := checkPostgres(o.PostgresURL, o.PostgresUser); err != nil {
		return EdgePartnerResult{}, err
	}
	if len(o.Admins) == 0 {
		return EdgePartnerResult{}, fmt.Errorf("--admin is required: the phone number or email of the console's first administrator (repeat it for more)")
	}
	var admins []string
	seen := map[string]bool{}
	for _, a := range o.Admins {
		n, err := normalizeAdmin(a)
		if err != nil {
			return EdgePartnerResult{}, err
		}
		if !seen[n] {
			seen[n] = true
			admins = append(admins, n)
		}
	}
	centralPins, err := readLeafPins("--central-pin", o.CentralPinFiles)
	if err != nil {
		return EdgePartnerResult{}, err
	}
	authPins, err := readLeafPins("--auth-pin", o.AuthPinFiles)
	if err != nil {
		return EdgePartnerResult{}, err
	}
	res := EdgePartnerResult{CentralPins: centralPins, AuthPins: authPins}
	consoleDir := ""
	if o.ConsoleDir != "" {
		if consoleDir, res.Warnings, err = checkConsoleDir(o.ConsoleDir); err != nil {
			return EdgePartnerResult{}, err
		}
	}

	dir := o.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return EdgePartnerResult{}, err
		}
		dir = filepath.Join(home, ".versola", "edge", partnerDirName(o.EdgeURL))
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return EdgePartnerResult{}, err
	}
	res.Dir, res.Project = dir, partnerProject(dir)
	// A directory that holds another edge (one `edge bootstrap` wrote) is not
	// taken over; one that holds this command's edge is a re-run.
	rerun := false
	if existing, err := os.ReadFile(filepath.Join(dir, "edge.conf")); err == nil {
		if !bytes.HasPrefix(existing, []byte(partnerConfHeader)) {
			return EdgePartnerResult{}, fmt.Errorf("%s holds the configuration of another edge (not written by `versola edge partner`): pass another --dir", dir)
		}
		rerun = true
	} else if !os.IsNotExist(err) {
		return EdgePartnerResult{}, fmt.Errorf("couldn't read %s: %w", filepath.Join(dir, "edge.conf"), err)
	}
	var token []byte
	if o.TokenFile != "" {
		data, err := os.ReadFile(o.TokenFile)
		if err != nil {
			return EdgePartnerResult{}, fmt.Errorf("couldn't read the token file: %w", err)
		}
		if res.EnrollAddress, err = ParseEnrollmentToken(string(data)); err != nil {
			return EdgePartnerResult{}, fmt.Errorf("the token file %s: %v -- it holds the enrollment token the operator gave you (vtk1.<address>.<secret>)", o.TokenFile, err)
		}
		token = []byte(strings.TrimSpace(string(data)) + "\n")
	} else if !rerun {
		return EdgePartnerResult{}, fmt.Errorf("the enrollment token file is required the first time: `versola edge partner <token file> ...` (the token the operator gave you)")
	}
	var set []envEntry
	if o.PostgresPasswordFile != "" {
		pw, err := readPasswordFile(o.PostgresPasswordFile)
		if err != nil {
			return EdgePartnerResult{}, err
		}
		set = append(set, envEntry{"POSTGRES_PASSWORD", pw, true})
	}
	for _, name := range partnerGeneratedSecrets {
		v, err := randomKey()
		if err != nil {
			return EdgePartnerResult{}, err
		}
		set = append(set, envEntry{name, v, false})
	}

	files := partnerEdgeFiles(partnerSite{
		EdgeURL: o.EdgeURL, CentralPins: centralPins, AuthPins: authPins, Admins: admins,
		PostgresURL: o.PostgresURL, PostgresUser: o.PostgresUser, Console: consoleDir != "",
	})
	files["compose.yml"] = []byte(partnerEdgeCompose(res.Project, o.Version, consoleDir))

	if err := os.MkdirAll(filepath.Join(dir, "pins"), 0o700); err != nil {
		return res, err
	}
	// pins/ is replaced as a whole: a pin no longer passed must not linger.
	old, _ := filepath.Glob(filepath.Join(dir, "pins", "*.crt"))
	for _, f := range old {
		_ = os.Remove(f)
	}
	for name, content := range files {
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, name), content, 0o644); err != nil {
			return res, fmt.Errorf("couldn't write %s: %w", name, err)
		}
	}
	tokenPath := filepath.Join(dir, "token")
	if token != nil {
		if err := fsutil.WriteFileAtomic(tokenPath, token, 0o600); err != nil {
			return res, fmt.Errorf("couldn't write the token: %w", err)
		}
	} else if err := ensureOwnerFile(tokenPath, "", 0o600); err != nil {
		// Compose mounts ./token: a missing file would become a directory,
		// which the edge cannot read. Empty, an enrolled edge does not need it.
		return res, err
	}
	if err := ensureOwnerFile(filepath.Join(dir, "edge.local.conf"), partnerLocalConf, 0o644); err != nil {
		return res, err
	}
	secretsPath := filepath.Join(dir, "edge.secrets.env")
	existing, err := os.ReadFile(secretsPath)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("couldn't read edge.secrets.env: %w", err)
	}
	if os.IsNotExist(err) {
		existing = []byte(partnerSecretsHeader)
	}
	merged := mergeEnv(string(existing), set)
	if err := fsutil.WriteFileAtomic(secretsPath, []byte(merged), 0o600); err != nil {
		return res, fmt.Errorf("couldn't write edge.secrets.env: %w", err)
	}
	if !envHasValue(merged, "POSTGRES_PASSWORD") {
		res.Missing = append(res.Missing, "POSTGRES_PASSWORD (the edge's database password: pass --postgres-password-file <file>)")
	}
	if o.NoStart || len(res.Missing) > 0 {
		return res, nil
	}
	// --force-recreate: edge.conf is replaced by a rename, which Compose does
	// not see as a change. The identity volume is kept.
	if err := docker.Run("compose", "-f", filepath.Join(dir, "compose.yml"), "up", "-d", "--force-recreate"); err != nil {
		return res, fmt.Errorf("couldn't start the edge: %w -- the files are in %s", err, dir)
	}
	res.Started = true
	return res, nil
}
