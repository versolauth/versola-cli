package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archiveFile is one member a test archive is built from.
type archiveFile struct {
	name     string
	data     string
	typeflag byte // 0 is a regular file
	// listAs / sha / size override what the manifest says about it.
	noList bool
	sha    string
	size   *int64
}

// buildArchive writes a tar.gz at path whose manifest.json comes first, the
// way packBackup writes it, from m (with the members' lines filled in from
// files unless a file overrides them) and the files.
func buildArchive(t *testing.T, path string, m BackupManifest, files []archiveFile) {
	t.Helper()
	if m.Format == 0 {
		m.Format = backupFormat
	}
	if m.Target == "" {
		m.Target = "vps"
	}
	if m.VersolaVersion == "" {
		m.VersolaVersion = "0.7.6"
	}
	if m.PostgresMode == "" {
		m.PostgresMode = PostgresModeBundled
	}
	m.CreatedAt = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	m.Members = nil
	for _, f := range files {
		if f.noList {
			continue
		}
		sum := sha256.Sum256([]byte(f.data))
		bm := BackupMember{Name: f.name, Size: int64(len(f.data)), SHA256: hex.EncodeToString(sum[:])}
		if f.sha != "" {
			bm.SHA256 = f.sha
		}
		if f.size != nil {
			bm.Size = *f.size
		}
		m.Members = append(m.Members, bm)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(m)
	if err := tw.WriteHeader(&tar.Header{Name: BackupManifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(mb)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), Typeflag: tar.TypeReg}
		if f.typeflag != 0 {
			h.Typeflag = f.typeflag
			h.Linkname = "/etc/passwd"
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if f.typeflag == 0 {
			tw.Write([]byte(f.data))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// vpsFiles is the member set `backup create` writes for a vps deployment on
// its own Postgres.
func vpsFiles() []archiveFile {
	return []archiveFile{
		{name: backupDumpMember, data: "PGDMP-fake-dump"},
		{name: backupOpenbaoMember, data: "tar-of-openbao"},
		{name: "openbao/vps.json", data: `{"roleId":"r","secretId":"SECRET-ID"}`},
		{name: "openbao/vps-admin.json", data: `{"rootToken":"ROOT","unsealKey":"UNSEAL"}`},
		{name: "state/state.json", data: `{"schemaVersion":4,"target":"vps","version":"0.7.6","authUrl":"https://id.example.com","proxyMode":"nginx","postgresMode":"bundled","postgresHost":"127.0.0.1:5432"}`},
	}
}

func TestVerifyBackupAcceptsAnIntactArchive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	buildArchive(t, p, BackupManifest{}, vpsFiles())
	m, err := VerifyBackup(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.VersolaVersion != "0.7.6" || len(m.Members) != 5 || !m.hasMember(backupDumpMember) {
		t.Errorf("manifest = %+v", m)
	}
}

// What `backup create` writes is what `backup restore` reads: the same archive
// goes through createBackup and then VerifyBackup, and its members come out.
func TestVerifyBackupReadsWhatBackupCreateWrites(t *testing.T) {
	st, ops, out := backupFixture(t, "vps", PostgresModeBundled)
	path, err := createBackup(st, BackupOptions{OutDir: out, CLIVersion: "test"}, io.Discard, ops)
	if err != nil {
		t.Fatal(err)
	}
	m, err := VerifyBackup(path)
	if err != nil {
		t.Fatalf("an archive of backup create is refused: %v", err)
	}
	for _, name := range []string{backupDumpMember, backupOpenbaoMember, "openbao/vps.json", "openbao/vps-admin.json", "state/state.json"} {
		if !m.hasMember(name) {
			t.Errorf("%s is not among the members: %+v", name, m.Members)
		}
	}
	if m.PostgresMode != PostgresModeBundled || m.VersolaVersion != st.Version {
		t.Errorf("manifest = %+v", m)
	}
	b, err := readBackupMember(path, "state/state.json")
	if err != nil || !strings.Contains(string(b), "schemaVersion") {
		t.Errorf("state.json = %q, %v", b, err)
	}
	r, err := openBackupMember(path, backupDumpMember)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if dump, _ := io.ReadAll(r); string(dump) != "PGDMP-fake" {
		t.Errorf("the dump member reads %q", dump)
	}
}

func TestVerifyBackupRefusesWhatIsNotRight(t *testing.T) {
	short := int64(3)
	cases := []struct {
		name  string
		m     BackupManifest
		files func() []archiveFile
		want  string
	}{
		{"a changed member", BackupManifest{}, func() []archiveFile {
			f := vpsFiles()
			f[0].sha = strings.Repeat("0", 64)
			return f
		}, "checksum of database.dump does not match"},
		{"a size that does not match", BackupManifest{}, func() []archiveFile {
			f := vpsFiles()
			f[0].size = &short
			return f
		}, "its manifest says 3"},
		{"a member the manifest does not list", BackupManifest{}, func() []archiveFile {
			return append(vpsFiles(), archiveFile{name: "state/extra", data: "x", noList: true})
		}, "which its manifest does not list"},
		{"a member this CLI does not write", BackupManifest{}, func() []archiveFile {
			return append(vpsFiles(), archiveFile{name: "../../etc/cron.d/x", data: "x"})
		}, "a member this versola does not write"},
		{"a symlink", BackupManifest{}, func() []archiveFile {
			return append(vpsFiles()[:1], archiveFile{name: backupOpenbaoMember, typeflag: tar.TypeSymlink, noList: true})
		}, "not a plain file"},
		{"a member twice", BackupManifest{}, func() []archiveFile {
			return append(vpsFiles(), archiveFile{name: backupDumpMember, data: "PGDMP-fake-dump"})
		}, "lists database.dump twice"},
		{"a member the manifest lists and the archive lacks", BackupManifest{}, func() []archiveFile {
			f := vpsFiles()
			// listed, but cut off the end of the tar below
			return append(f, archiveFile{name: "openbao/vps-postgres-role.sql", data: "x"})
		}, ""},
		{"a newer format", BackupManifest{Format: 2}, vpsFiles, "written by a newer versola"},
		{"a local archive", BackupManifest{Target: "local"}, vpsFiles, "vps deployments only"},
		{"a version with a control character", BackupManifest{VersolaVersion: "0.7.6\x1b[2J"}, vpsFiles, "no usable Versola version"},
		{"a version with a space", BackupManifest{VersolaVersion: "0.7.6 --yes"}, vpsFiles, "no usable Versola version"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "a.tar.gz")
		buildArchive(t, p, c.m, c.files())
		if c.name == "a member the manifest lists and the archive lacks" {
			// Rewrite the archive without its last member but with the
			// manifest that lists it.
			files := c.files()
			buildArchiveManifestOf(t, p, files, files[:len(files)-1])
			c.want = "is listed in the manifest but is not in the archive"
		}
		_, err := VerifyBackup(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// buildArchiveManifestOf writes an archive whose manifest lists listed and
// whose tar holds present.
func buildArchiveManifestOf(t *testing.T, path string, listed, present []archiveFile) {
	t.Helper()
	buildArchive(t, path, BackupManifest{}, listed)
	// Read the manifest back and write the shorter tar around it.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	tr.Next()
	mb, _ := io.ReadAll(tr)
	f.Close()
	out, _ := os.Create(path)
	defer out.Close()
	gw := gzip.NewWriter(out)
	tw := tar.NewWriter(gw)
	tw.WriteHeader(&tar.Header{Name: BackupManifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg})
	tw.Write(mb)
	for _, p := range present {
		tw.WriteHeader(&tar.Header{Name: p.name, Mode: 0o600, Size: int64(len(p.data)), Typeflag: tar.TypeReg})
		tw.Write([]byte(p.data))
	}
	tw.Close()
	gw.Close()
}

func TestVerifyBackupRefusesATruncatedOrForeignFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tar.gz")
	buildArchive(t, p, BackupManifest{}, vpsFiles())
	b, _ := os.ReadFile(p)

	cut := filepath.Join(dir, "cut.tar.gz")
	os.WriteFile(cut, b[:len(b)-20], 0o600)
	if _, err := VerifyBackup(cut); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("truncated: %v", err)
	}
	text := filepath.Join(dir, "text.tar.gz")
	os.WriteFile(text, []byte("not an archive"), 0o600)
	if _, err := VerifyBackup(text); err == nil || !strings.Contains(err.Error(), "not an archive made by `versola backup create`") {
		t.Errorf("text file: %v", err)
	}
	if _, err := VerifyBackup(dir); err == nil {
		t.Errorf("a directory was accepted")
	}
	if _, err := VerifyBackup(filepath.Join(dir, "missing.tar.gz")); err == nil {
		t.Errorf("a missing file was accepted")
	}
}

func TestSanitizeForMessage(t *testing.T) {
	got := sanitizeForMessage("ok\x1b[31mred\nline")
	if strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("control characters survive: %q", got)
	}
	if len(sanitizeForMessage(strings.Repeat("a", 500))) > 48 {
		t.Errorf("not cut short")
	}
}

func TestReadBackupMemberIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	buildArchive(t, p, BackupManifest{}, vpsFiles())
	b, err := readBackupMember(p, "openbao/vps.json")
	if err != nil || !strings.Contains(string(b), "roleId") {
		t.Fatalf("= %q, %v", b, err)
	}
	if _, err := readBackupMember(p, "openbao/vps-postgres-role.sql"); err == nil {
		t.Errorf("a member that is not there was read")
	}
}
