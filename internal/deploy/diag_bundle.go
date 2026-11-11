package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// `versola doctor --bundle` writes one archive that someone helping with a
// failed or ailing deployment needs, and that its owner can send without
// reading it: what doctor says, the versions in play, the CLI's record of the
// deployment, what `status --json` and `stats --json` say, and the generated
// files of the bundle that are known not to hold a secret. Nothing else:
// never the secrets files, OpenBao's data and credentials, the containers'
// environment or the database. The services' logs are in it only when asked
// for (--logs), because nothing can promise they hold no personal data or
// secret (`versola logs` says the same).
//
// A section that cannot be made (Docker is down: the very reason the bundle
// is wanted) is not an error of the bundle: it is listed in the manifest with
// the reason, and the rest is written. The archive is mode 0600, packed under
// a temp name and renamed into place at the end, and never replaces a file.

const (
	// DiagFormat is the layout of the archive.
	DiagFormat = 1
	// DiagManifestName is the first member.
	DiagManifestName = "manifest.json"
	diagPrefix       = "versola-diagnostics-"
	diagSuffix       = ".tar.gz"
	// maxDiagSection bounds what one section may put in the archive.
	maxDiagSection = 8 << 20
)

// DiagMember is one file in the archive, as the manifest lists it.
type DiagMember struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// DiagProblem is a section that could not be made, or ended with an error, and why.
type DiagProblem struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// DiagManifest is manifest.json. No secret value is in it: names, sizes and
// checksums, and what was left out on purpose.
type DiagManifest struct {
	Format         int       `json:"format"`
	CLIVersion     string    `json:"cliVersion"`
	OS             string    `json:"os"`
	Arch           string    `json:"arch"`
	CreatedAt      time.Time `json:"createdAt"`
	Target         string    `json:"target,omitempty"`
	VersolaVersion string    `json:"versolaVersion,omitempty"`
	// LogsIncluded is whether the services' logs (not redacted) are in it.
	LogsIncluded bool          `json:"logsIncluded"`
	Members      []DiagMember  `json:"members"`
	Problems     []DiagProblem `json:"problems,omitempty"`
	NotIncluded  []string      `json:"notIncluded"`
}

// DiagMeta is what the manifest says about who made the archive, for what.
type DiagMeta struct {
	CLIVersion     string
	OS, Arch       string
	Target         string
	VersolaVersion string
	LogsIncluded   bool
}

// DiagSection is one member of the archive and how it is made.
type DiagSection struct {
	// Name is the member's path in the archive.
	Name string
	// Scrub blanks what looks like a secret value in the content (a defence for
	// generated files that are not meant to hold one; never promised for logs,
	// which are not scrubbed).
	Scrub bool
	// ScrubLog blanks only what Postgres is known to log of a failed role
	// statement (ScrubLogs): for service logs, which are otherwise not touched.
	ScrubLog bool
	// ScrubLight applies ScrubLight: for doctor's output, the record, status and
	// stats, where the customer's own flags can end up.
	ScrubLight bool
	// Run writes the content. Whatever it wrote before an error is kept.
	Run func(w io.Writer) error
}

// DiagNotIncluded is what the archive never holds, said in the manifest and
// after the command. withLogs is whether --logs was given.
func DiagNotIncluded(withLogs bool) []string {
	out := []string{
		"the secrets files of the deployment (*.secrets.env) and their values",
		"OpenBao's data, its credentials and its keys (~/.versola/openbao)",
		"the environment variables of the containers",
		"the database",
	}
	if !withLogs {
		out = append(out, "the services' logs (--logs adds them; they are not redacted)")
	}
	return out
}

// DiagFileName is versola-diagnostics-<target>-<UTC time>.tar.gz, for a --bundle
// that names a directory.
func DiagFileName(target string, t time.Time) string {
	if target == "" {
		target = "machine"
	}
	return diagPrefix + target + "-" + t.UTC().Format("20060102T150405Z") + diagSuffix
}

// capBuffer holds at most max bytes of what is written to it and says so
// where it cut: a section that ran away must not fill the disk or the mailbox.
type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.truncated = true
	} else {
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *capBuffer) bytes() []byte {
	if !c.truncated {
		return c.buf.Bytes()
	}
	return append(append([]byte{}, c.buf.Bytes()...), []byte(fmt.Sprintf("\n[cut: more than %d MiB]\n", c.max>>20))...)
}

// CheckDiagTarget says whether an archive can be made at path (a name that does
// not exist, or a directory), before doctor runs its checks: a customer should
// not wait for them to be told the name is taken. WriteDiagBundle checks again.
func CheckDiagTarget(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		abs, _ := filepath.Abs(path)
		return fmt.Errorf("%s already exists -- it is not replaced; give another name, or a directory", abs)
	}
	return nil
}

// WriteDiagBundle makes the archive at path from sections, in order, and
// returns its manifest. path is a file that does not exist yet, or a
// directory the file is made in; it is never replaced. now stamps the manifest.
func WriteDiagBundle(path string, meta DiagMeta, sections []DiagSection, now func() time.Time) (final string, m *DiagManifest, err error) {
	t := now().UTC()
	if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
		path = filepath.Join(path, DiagFileName(meta.Target, t))
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", nil, err
	}
	if _, serr := os.Lstat(path); serr == nil {
		return "", nil, fmt.Errorf("%s already exists -- it is not replaced; give another name, or a directory", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("couldn't create %s: %w", dir, err)
	}

	// The temp file first, before anything is gathered: a directory that cannot
	// be written is found now, not after minutes of Docker calls (0600, empty
	// until the end).
	tmp, err := os.CreateTemp(dir, ".tmp-"+diagPrefix+"*")
	if err != nil {
		return "", nil, fmt.Errorf("couldn't create a temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return "", nil, fmt.Errorf("couldn't set permissions on %s: %w", tmpName, err)
	}

	m = &DiagManifest{Format: DiagFormat, CLIVersion: meta.CLIVersion, OS: meta.OS, Arch: meta.Arch, CreatedAt: t,
		Target: meta.Target, VersolaVersion: meta.VersolaVersion, LogsIncluded: meta.LogsIncluded, NotIncluded: DiagNotIncluded(meta.LogsIncluded)}
	type member struct {
		name string
		data []byte
	}
	var members []member
	for _, s := range sections {
		cb := &capBuffer{max: maxDiagSection}
		runErr := s.Run(cb)
		data := cb.bytes()
		if s.Scrub {
			data = []byte(ScrubSecrets(string(data)))
		} else if s.ScrubLight {
			data = []byte(ScrubLight(string(data)))
		} else if s.ScrubLog {
			data = []byte(ScrubLogs(string(data)))
		}
		if runErr != nil {
			m.Problems = append(m.Problems, DiagProblem{Name: s.Name, Reason: diagReason(runErr)})
		}
		// An empty member that was made (a service with no log lines) stays, so
		// "empty" is not mistaken for "not collected"; one that failed and wrote
		// nothing is only in the problems.
		if len(data) == 0 && runErr != nil {
			continue
		}
		sum := sha256.Sum256(data)
		m.Members = append(m.Members, DiagMember{Name: s.Name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		members = append(members, member{s.Name, data})
	}

	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", nil, err
	}
	mb = append(mb, '\n')

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: t.Truncate(time.Second), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err = put(DiagManifestName, mb); err != nil {
		return "", nil, err
	}
	for _, mem := range members {
		if err = put(mem.name, mem.data); err != nil {
			return "", nil, fmt.Errorf("couldn't add %s: %w", mem.name, err)
		}
	}
	if err = tw.Close(); err != nil {
		return "", nil, err
	}
	if err = gz.Close(); err != nil {
		return "", nil, err
	}
	if err = tmp.Sync(); err != nil {
		return "", nil, fmt.Errorf("couldn't flush %s to disk: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return "", nil, fmt.Errorf("couldn't finish writing %s: %w", tmpName, err)
	}
	// A hard link, not a rename: it fails when the name has been taken since
	// the start (a rename would replace the file, on Linux and on Windows).
	// Where links are not supported the check and the rename stand in.
	if lerr := os.Link(tmpName, path); lerr == nil {
		os.Remove(tmpName)
	} else if errors.Is(lerr, os.ErrExist) {
		err = fmt.Errorf("%s already exists -- it is not replaced; give another name, or a directory", path)
		return "", nil, err
	} else if _, serr := os.Lstat(path); serr == nil {
		err = fmt.Errorf("%s already exists -- it is not replaced; give another name, or a directory", path)
		return "", nil, err
	} else if err = os.Rename(tmpName, path); err != nil {
		return "", nil, fmt.Errorf("couldn't move %s into place at %s: %w", tmpName, path, err)
	}
	return path, m, nil
}

// diagReason is why a section failed, in one line: errors may carry the first
// line of a command's own output, never more.
func diagReason(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return diagPrintable(s)
}

// PrintDiagSummary says what was written, what is in it and what is not, names
// and sizes only.
func PrintDiagSummary(w io.Writer, path string, m *DiagManifest) {
	var total int64
	for _, mem := range m.Members {
		total += mem.Size
	}
	fmt.Fprintf(w, "\nWrote %s (mode 0600): %d files, %s before compression.\n", path, len(m.Members), HumanSize(total))
	for _, mem := range m.Members {
		fmt.Fprintf(w, "  %-34s %s\n", mem.Name, HumanSize(mem.Size))
	}
	if len(m.Problems) > 0 {
		fmt.Fprintln(w, "\nCould not be made (the manifest says why):")
		for _, p := range m.Problems {
			fmt.Fprintf(w, "  %-34s %s\n", p.Name, p.Reason)
		}
	}
	fmt.Fprintln(w, "\nNot in it:")
	for _, n := range m.NotIncluded {
		fmt.Fprintf(w, "  - %s\n", n)
	}
	if m.LogsIncluded {
		fmt.Fprintln(w, "\nThe logs in it are the services' own output, not redacted: they can hold addresses, names and ids of your users, and credentials (Postgres logs failed statements). Read them before you send the file.")
	} else {
		fmt.Fprintln(w, "\nIt holds no password or key, but it identifies the server: your domain, the addresses it resolves to, your database host and user name, paths under your home directory. For the services' last log lines as well: `versola doctor --bundle <file> --logs` (read those before sending).")
	}
}

// ---- Blanking secret-looking values ----------------------------------------

var (
	// A private key block, also an unterminated one (a key cut by the size cap).
	scrubPEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----.*?(-----END [A-Z ]*PRIVATE KEY( BLOCK)?-----|\z)`)
	// The rest of the line after a name that says password, secret, token, key,
	// credential or salt/pepper/psk, with an optional quote before the
	// separator (JSON): not one word of it, so a value with spaces, commas or
	// semicolons does not leak its tail.
	scrubAssign       = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:pass(?:word|wd)?|pwd|secret|token|key|credential|authorization|salt|pepper|psk)[A-Za-z0-9_.-]*)(["']?[ \t]*[:=][ \t]*)([^\r\n]+)`)
	scrubUserNoScheme = regexp.MustCompile(`\b([A-Za-z0-9_.-]+):[^\s/@:]+@`)
	// What Postgres logs of a failed role statement: a SCRAM verifier, or the
	// password itself in PASSWORD '...'.
	scrubScram    = regexp.MustCompile(`SCRAM-SHA-256\$[0-9]+:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+`)
	scrubPwdQuote = regexp.MustCompile(`(?i)\bpassword[ \t]+'[^'\n]*'`)
	scrubAuthLine = regexp.MustCompile(`(?i)\b(authorization)([ \t]*[:=][ \t]*)[^\r\n]+`)
	scrubBearer   = regexp.MustCompile(`(?i)\b(bearer|basic)[ \t]+[A-Za-z0-9._~+/=-]{8,}`)
	scrubUserinfo = regexp.MustCompile(`(://[^/\s:@]+):[^/\s@]+@`)
)

// ScrubSecrets blanks what looks like a secret value in generated text: a
// private key block, the value after a name that says password, secret, token,
// key or credential, a bearer token, the password of a URL. A placeholder
// (`${VAR}`, `${?VAR}`) stays: it is where a value would go, not a value. It is
// a second line of defence for files that are written without secrets in the
// first place; it is not applied to logs, and it promises nothing there.
func ScrubSecrets(s string) string {
	s = scrubPEM.ReplaceAllString(s, "<private key removed>")
	// A whole header line: its value is a scheme and a token, not one word.
	s = scrubAuthLine.ReplaceAllString(s, "$1$2<redacted>")
	s = scrubAssign.ReplaceAllStringFunc(s, func(m string) string {
		sub := scrubAssign.FindStringSubmatch(m)
		v := strings.Trim(sub[3], `"'`)
		if strings.HasPrefix(v, "${") || strings.HasPrefix(v, "$(") || v == "<redacted>" || v == "" {
			return m
		}
		return sub[1] + sub[2] + "<redacted>"
	})
	s = scrubBearer.ReplaceAllString(s, "$1 <redacted>")
	s = scrubUserinfo.ReplaceAllString(s, "$1:<redacted>@")
	s = scrubUserNoScheme.ReplaceAllString(s, "$1:<redacted>@")
	return s
}

// ScrubLogs blanks the two things Postgres is known to log of a failed role
// statement: a SCRAM verifier and PASSWORD '...', and the password of a URL. It
// is a best effort on logs, which can hold anything, and promises nothing more.
func ScrubLogs(s string) string {
	s = scrubScram.ReplaceAllString(s, "SCRAM-SHA-256$<redacted>")
	s = scrubPwdQuote.ReplaceAllString(s, "PASSWORD '<redacted>'")
	s = scrubUserinfo.ReplaceAllString(s, "$1:<redacted>@")
	return s
}

// errDiagNone is what a section returns when there is nothing to put in it on
// this machine (no deployment yet): a note in the manifest, not a failure.
var errDiagNone = errors.New("nothing on this machine")

// DiagNone is the error a section returns for "nothing to put here".
func DiagNone(what string) error { return fmt.Errorf("%w: %s", errDiagNone, what) }

// diagPrintable keeps text that came from a program's output off the
// terminal's control sequences: anything outside printable ASCII becomes '?'.
func diagPrintable(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// ScrubLight is the scrub for members that are not generated files and are
// JSON or text the customer's own input can reach (doctor's output, the record,
// status and stats): a private key, an Authorization line, a bearer token, the
// password of a URL or of user:password@host. Not the name-and-value rule of
// ScrubSecrets, which would cut the commas out of JSON.
func ScrubLight(s string) string {
	s = scrubPEM.ReplaceAllString(s, "<private key removed>")
	s = scrubAuthLine.ReplaceAllString(s, "$1$2<redacted>")
	s = scrubBearer.ReplaceAllString(s, "$1 <redacted>")
	s = scrubUserinfo.ReplaceAllString(s, "$1:<redacted>@")
	s = scrubUserNoScheme.ReplaceAllString(s, "$1:<redacted>@")
	return s
}
