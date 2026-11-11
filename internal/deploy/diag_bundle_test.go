package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func diagNow() time.Time { return time.Date(2026, 11, 11, 3, 4, 5, 0, time.UTC) }

func readDiag(t *testing.T, path string) ([]string, map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	content := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, content
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		content[h.Name] = string(b)
	}
}

func TestWriteDiagBundleLayoutAndManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out", "d.tar.gz")
	sections := []DiagSection{
		{Name: "a.txt", Run: func(w io.Writer) error { _, err := io.WriteString(w, "alpha"); return err }},
		{Name: "files/b.yml", Scrub: true, Run: func(w io.Writer) error { _, err := io.WriteString(w, "PASSWORD: hunter2\nimage: x\n"); return err }},
	}
	final, m, err := WriteDiagBundle(path, DiagMeta{CLIVersion: "1.2.3", OS: "linux", Arch: "amd64", Target: "vps", VersolaVersion: "0.7.6"}, sections, diagNow)
	if err != nil {
		t.Fatal(err)
	}
	if final != path {
		t.Errorf("path %s, want %s", final, path)
	}
	names, content := readDiag(t, path)
	if len(names) != 3 || names[0] != DiagManifestName || names[1] != "a.txt" || names[2] != "files/b.yml" {
		t.Fatalf("members %v", names)
	}
	if content["files/b.yml"] != "PASSWORD: <redacted>\nimage: x\n" {
		t.Errorf("scrubbed content %q", content["files/b.yml"])
	}
	var got DiagManifest
	if err := json.Unmarshal([]byte(content[DiagManifestName]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Format != DiagFormat || got.CLIVersion != "1.2.3" || got.Target != "vps" || got.VersolaVersion != "0.7.6" || !got.CreatedAt.Equal(diagNow()) || len(got.NotIncluded) == 0 {
		t.Errorf("manifest %+v", got)
	}
	// Sizes and checksums are those of what is in the archive (after the scrub).
	for _, mem := range got.Members {
		sum := sha256.Sum256([]byte(content[mem.Name]))
		if mem.Size != int64(len(content[mem.Name])) || mem.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: %+v", mem.Name, mem)
		}
	}
	if len(m.Members) != 2 {
		t.Errorf("returned manifest lists %d members", len(m.Members))
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v (%v), want 0600", fi.Mode().Perm(), err)
		}
	}
	// No temp file is left next to it.
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("leftovers: %v", ents)
	}
}

func TestWriteDiagBundleNeverReplacesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.tar.gz")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := WriteDiagBundle(path, DiagMeta{}, nil, diagNow)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Errorf("the file was replaced: %q", b)
	}
}

func TestWriteDiagBundleIntoADirectory(t *testing.T) {
	dir := t.TempDir()
	final, _, err := WriteDiagBundle(dir, DiagMeta{Target: "vps"}, nil, diagNow)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(final) != dir || filepath.Base(final) != "versola-diagnostics-vps-20261111T030405Z.tar.gz" {
		t.Errorf("name %s", final)
	}
	// Twice in the same second: the second is refused, not written over the first.
	if _, _, err := WriteDiagBundle(dir, DiagMeta{Target: "vps"}, nil, diagNow); err == nil {
		t.Errorf("a second archive replaced the first")
	}
}

// A section that cannot be made does not stop the others; what it wrote before
// failing stays, and the reason is in the manifest in one printable line.
func TestWriteDiagBundleSectionsFailAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.tar.gz")
	sections := []DiagSection{
		{Name: "ok.txt", Run: func(w io.Writer) error { _, err := io.WriteString(w, "fine"); return err }},
		{Name: "partial.json", Run: func(w io.Writer) error {
			io.WriteString(w, `{"x":1}`)
			return errors.New("docker says no\x1b[31m and a second line\nsecond")
		}},
		{Name: "none.txt", Run: func(io.Writer) error { return DiagNone("no deployment") }},
	}
	_, m, err := WriteDiagBundle(path, DiagMeta{}, sections, diagNow)
	if err != nil {
		t.Fatal(err)
	}
	names, content := readDiag(t, path)
	if strings.Join(names, ",") != "manifest.json,ok.txt,partial.json" || content["partial.json"] != `{"x":1}` {
		t.Errorf("members %v, partial %q", names, content["partial.json"])
	}
	if len(m.Problems) != 2 || m.Problems[0].Name != "partial.json" || m.Problems[1].Name != "none.txt" {
		t.Fatalf("problems %+v", m.Problems)
	}
	if strings.ContainsAny(m.Problems[0].Reason, "\x1b\n") || !strings.HasPrefix(m.Problems[0].Reason, "docker says no") {
		t.Errorf("reason %q", m.Problems[0].Reason)
	}
}

func TestWriteDiagBundleCapsASection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.tar.gz")
	big := strings.Repeat("x", 1<<20)
	_, _, err := WriteDiagBundle(path, DiagMeta{}, []DiagSection{{Name: "big.log", Run: func(w io.Writer) error {
		for i := 0; i < 12; i++ {
			if _, err := io.WriteString(w, big); err != nil {
				return err
			}
		}
		return nil
	}}}, diagNow)
	if err != nil {
		t.Fatal(err)
	}
	_, content := readDiag(t, path)
	if n := len(content["big.log"]); n > maxDiagSection+64 || !strings.Contains(content["big.log"], "[cut: more than 8 MiB]") {
		t.Errorf("the section is %d bytes: not cut", n)
	}
}

func TestScrubSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"POSTGRES_PASSWORD: hunter2":                       "POSTGRES_PASSWORD: <redacted>",
		"POSTGRES_PASSWORD=hunter2":                        "POSTGRES_PASSWORD=<redacted>",
		`password = "hunter2"`:                             "password = <redacted>",
		"  AUTH_CODES_SECRET: abc123":                      "  AUTH_CODES_SECRET: <redacted>",
		"api-key: abc":                                     "api-key: <redacted>",
		"- ./auth.secrets.env":                             "- ./auth.secrets.env",
		"password = ${POSTGRES_PASSWORD}":                  "password = ${POSTGRES_PASSWORD}",
		"password = ${?AUTH_PASSWORD}":                     "password = ${?AUTH_PASSWORD}",
		"image: ghcr.io/versolauth/versola-auth:0.7.6":     "image: ghcr.io/versolauth/versola-auth:0.7.6",
		"Authorization: Bearer abcdef0123456789":           "Authorization: <redacted>",
		"curl -H 'X: Bearer abcdef0123456789'":             "curl -H 'X: Bearer <redacted>'",
		"url: postgresql://versola:hunter2@db.example.com": "url: postgresql://versola:<redacted>@db.example.com",
		"ports:\n  - 8080:8080":                            "ports:\n  - 8080:8080",
	} {
		if got := ScrubSecrets(in); got != want {
			t.Errorf("ScrubSecrets(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	pem := "before\n-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY-----\nafter"
	if got := ScrubSecrets(pem); strings.Contains(got, "MIIE") || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("a private key block survived: %q", got)
	}
	cert := "-----BEGIN CERTIFICATE-----\nMIIDdzCCAl\n-----END CERTIFICATE-----"
	if got := ScrubSecrets(cert); got != cert {
		t.Errorf("a certificate was changed: %q", got)
	}
}

func TestDiagNotIncludedNamesTheLogsOnlyWithoutThem(t *testing.T) {
	with := strings.Join(DiagNotIncluded(true), "|")
	without := strings.Join(DiagNotIncluded(false), "|")
	for _, want := range []string{"*.secrets.env", "OpenBao", "environment variables", "database"} {
		if !strings.Contains(with, want) || !strings.Contains(without, want) {
			t.Errorf("%q is not said to be left out", want)
		}
	}
	if strings.Contains(with, "logs") || !strings.Contains(without, "logs") {
		t.Errorf("logs: with %q, without %q", with, without)
	}
}

func TestCheckDiagTarget(t *testing.T) {
	dir := t.TempDir()
	if err := CheckDiagTarget(filepath.Join(dir, "new.tar.gz")); err != nil {
		t.Errorf("a new name: %v", err)
	}
	if err := CheckDiagTarget(dir); err != nil {
		t.Errorf("a directory: %v", err)
	}
	taken := filepath.Join(dir, "taken.tar.gz")
	if err := os.WriteFile(taken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDiagTarget(taken); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("a taken name: %v", err)
	}
}

// What the first review round found passing through: a quoted key (JSON), a
// value with a comma, a semicolon or spaces, an unterminated or PGP key block,
// and user:password@host with no scheme.
func TestScrubSecretsKnownBypasses(t *testing.T) {
	for in, want := range map[string]string{
		`{"password": "hunter2", "token":"abc123"}`: `{"password": <redacted>`,
		"password: a1;b2":              "password: <redacted>",
		"password: ab,cd":              "password: <redacted>",
		"password: my long passphrase": "password: <redacted>",
		"PSK=abcdef":                   "PSK=<redacted>",
	} {
		got := ScrubSecrets(in)
		if !strings.HasPrefix(got, strings.SplitN(want, "<", 2)[0]) || !strings.Contains(got, "<redacted>") || strings.Contains(got, "hunter2") || strings.Contains(got, "a1;b2") || strings.Contains(got, "ab,cd") || strings.Contains(got, "passphrase") || strings.Contains(got, "abcdef") {
			t.Errorf("ScrubSecrets(%q) = %q", in, got)
		}
	}
	for _, in := range []string{
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF\n-----END PGP PRIVATE KEY BLOCK-----",
		"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n(cut by the size cap)",
	} {
		if got := ScrubSecrets(in); strings.Contains(got, "lQOYBF") || strings.Contains(got, "MIIE") {
			t.Errorf("a key block survived: %q", got)
		}
	}
	if got := ScrubSecrets("host: versola:hunter2@db.example.com:5432"); strings.Contains(got, "hunter2") {
		t.Errorf("user:password@host survived: %q", got)
	}
}

// The light scrub, for JSON and text the customer's flags can reach, keeps JSON
// valid (no value rule) and still blanks what a pasted URL carries.
func TestScrubLightKeepsJSONAndBlanksURLs(t *testing.T) {
	in := `{"secretsRevision": 2, "database": {"host": "postgres://versola:hunter2@db.example.com:5432/auth"}}`
	got := ScrubLight(in)
	if strings.Contains(got, "hunter2") || !strings.Contains(got, `"secretsRevision": 2,`) {
		t.Errorf("= %q", got)
	}
	if got := ScrubLight("Authorization: Bearer abcdef0123456789"); strings.Contains(got, "abcdef") {
		t.Errorf("= %q", got)
	}
}

// Postgres logs the text of a failed role statement: the verifier, or the password itself.
func TestScrubLogsBlanksWhatPostgresLogs(t *testing.T) {
	in := "ERROR:  syntax error\nSTATEMENT:  CREATE ROLE app LOGIN PASSWORD 'plain-pw-9' \nDETAIL: verifier SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy\n"
	got := ScrubLogs(in)
	if strings.Contains(got, "plain-pw-9") || strings.Contains(got, "c2FsdA") || !strings.Contains(got, "syntax error") {
		t.Errorf("= %q", got)
	}
}

// A name taken while the sections ran is not replaced at the end.
func TestWriteDiagBundleDoesNotReplaceAFileThatAppeared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.tar.gz")
	sections := []DiagSection{{Name: "a.txt", Run: func(w io.Writer) error {
		os.WriteFile(path, []byte("someone else's"), 0o600) // appears while the archive is gathered
		_, err := io.WriteString(w, "x")
		return err
	}}}
	if _, _, err := WriteDiagBundle(path, DiagMeta{}, sections, diagNow); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "someone else's" {
		t.Errorf("the file was replaced: %q", b)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("temp file left: %v", ents)
	}
}

// An empty member that was made stays (a service with no log lines).
func TestWriteDiagBundleKeepsAnEmptyMember(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.tar.gz")
	_, m, err := WriteDiagBundle(path, DiagMeta{}, []DiagSection{{Name: "logs/mtls-init.log", Run: func(io.Writer) error { return nil }}}, diagNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 1 || m.Members[0].Size != 0 {
		t.Errorf("members %+v", m.Members)
	}
}
