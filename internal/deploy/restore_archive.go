package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/versolauth/versola-cli/internal/state"
)

// The archive `backup restore` reads is whatever file it is given: it may have
// been damaged on the way to this server, or be something else altogether. So
// it is read in two passes, and nothing on the machine changes before the
// first one has finished:
//
//  1. VerifyBackup reads every member once, against the size and SHA-256 its
//     manifest gives, and refuses members the manifest does not list, names
//     this CLI does not write, and anything that is not a plain file.
//  2. openBackupMember then reads the one member a step needs (the dump, the
//     OpenBao volume, a credentials file) as a stream.
//
// What the manifest says is checked before it is shown or used: it is data
// from a file, not from this CLI.

// restoreMembers are the members `backup create` writes for a vps deployment,
// and so the only names a restore accepts. Fixed names: a file in the archive
// can never choose where on this machine it is written.
var restoreMembers = map[string]bool{
	backupDumpMember:                    true,
	backupOpenbaoMember:                 true,
	"openbao/vps.json":                  true,
	"openbao/vps-admin.json":            true,
	"openbao/vps-postgres-role.sql":     true,
	"state/state.json":                  true,
	"state/" + state.PostgresCAFileName: true,
}

// manifestWord is what a manifest may carry where this CLI prints it or puts
// it into a command: a version, a database or role name. Anything with a
// control character, a space or a quote is refused, so that an archive cannot
// put escape sequences on the terminal or arguments into a command line.
var manifestWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxSmallMember bounds the members read into memory (credentials, the
// record): they are a few hundred bytes.
const maxSmallMember = 1 << 20

// maxTrailing is how much may follow the last member (tar's padding, a few
// KiB) before the archive is refused: a file that expands into much more is not
// one `backup create` wrote.
const maxTrailing = 1 << 20

// hasSparseRecord says whether a member is a sparse file: its size on the
// manifest is then not what is stored, and reading it can run for a long time on
// a small archive. `backup create` writes none.
func hasSparseRecord(h *tar.Header) bool {
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse") {
			return true
		}
	}
	return false
}

// checkBackupManifest refuses a manifest this CLI cannot restore from, with
// the reason.
func checkBackupManifest(m *BackupManifest) error {
	switch {
	case m.Format > backupFormat:
		return fmt.Errorf("the archive was written by a newer versola (archive format %d, this one reads %d) -- update versola (`versola upgrade`)", m.Format, backupFormat)
	case m.Format != backupFormat:
		return fmt.Errorf("the archive has format %d, which this versola does not read", m.Format)
	}
	if m.Target == "" {
		return errors.New("the archive's manifest names no deployment -- the archive is damaged; copy it again")
	}
	if m.Target != "vps" {
		return fmt.Errorf("the archive is of a %q deployment: `backup restore` puts back vps deployments only", sanitizeForMessage(m.Target))
	}
	if !manifestWord.MatchString(m.VersolaVersion) {
		return errors.New("the archive's manifest has no usable Versola version")
	}
	if m.PostgresDB != "" && !manifestWord.MatchString(m.PostgresDB) {
		return errors.New("the archive's manifest names a database that is not a plain name")
	}
	if m.PostgresUser != "" && !manifestWord.MatchString(m.PostgresUser) {
		return errors.New("the archive's manifest names a database role that is not a plain name")
	}
	seen := map[string]bool{}
	for _, bm := range m.Members {
		if !restoreMembers[bm.Name] {
			return fmt.Errorf("the archive lists a member this versola does not write (%s)", sanitizeForMessage(bm.Name))
		}
		if seen[bm.Name] {
			return fmt.Errorf("the archive lists %s twice", bm.Name)
		}
		seen[bm.Name] = true
		if bm.Size < 0 || !hexSHA256.MatchString(bm.SHA256) {
			return fmt.Errorf("the archive's manifest has no usable size or checksum for %s", bm.Name)
		}
	}
	return nil
}

// sanitizeForMessage keeps a string from a file out of the terminal's control
// sequences: anything outside printable ASCII becomes '?', and it is cut short.
func sanitizeForMessage(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && i < 48; i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			c = '?'
		}
		out = append(out, c)
	}
	return string(out)
}

// hasMember says whether the manifest lists a member.
func (m *BackupManifest) hasMember(name string) bool {
	for _, bm := range m.Members {
		if bm.Name == name {
			return true
		}
	}
	return false
}

// skippedReason is why `backup create` left a member out, "" when it did not
// say (or did not leave it out).
func (m *BackupManifest) skippedReason(name string) string {
	for _, s := range m.Skipped {
		if s.Name == name {
			return sanitizeForMessage(s.Reason)
		}
	}
	return ""
}

// VerifyBackup reads the archive at path from end to end and returns its
// manifest when every member is there and matches its size and SHA-256. It
// changes nothing. A truncated or damaged file (a copy that stopped half way,
// a bad gzip trailer) is an error here, before anything on this machine is
// touched.
func VerifyBackup(path string) (*BackupManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not an archive made by `versola backup create`", path)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s is not an archive made by `versola backup create` (not gzip: %w)", path, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("%s is not an archive made by `versola backup create` (not tar: %w)", path, err)
	}
	if h.Name != BackupManifestName {
		return nil, fmt.Errorf("%s is not an archive made by `versola backup create`: its first member is not %s", path, BackupManifestName)
	}
	var m BackupManifest
	if err := json.NewDecoder(io.LimitReader(tr, maxSmallMember)).Decode(&m); err != nil {
		return nil, fmt.Errorf("couldn't read %s (%w) -- the archive is damaged; copy it again", BackupManifestName, err)
	}
	if err := checkBackupManifest(&m); err != nil {
		return nil, err
	}
	want := map[string]BackupMember{}
	for _, bm := range m.Members {
		want[bm.Name] = bm
	}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the archive is damaged (%w) -- copy it again", err)
		}
		if h.Typeflag != tar.TypeReg || hasSparseRecord(h) {
			return nil, fmt.Errorf("the archive holds %s, which is not a plain file", sanitizeForMessage(h.Name))
		}
		bm, ok := want[h.Name]
		if !ok {
			return nil, fmt.Errorf("the archive holds %s, which its manifest does not list", sanitizeForMessage(h.Name))
		}
		if seen[h.Name] {
			return nil, fmt.Errorf("the archive holds %s twice", h.Name)
		}
		seen[h.Name] = true
		sum := sha256.New()
		n, err := io.Copy(sum, io.LimitReader(tr, bm.Size+1))
		if err != nil {
			return nil, fmt.Errorf("the archive is damaged while reading %s (%w) -- copy it again", h.Name, err)
		}
		if n != bm.Size {
			return nil, fmt.Errorf("%s is %d bytes in the archive, and its manifest says %d -- the archive is damaged; copy it again", h.Name, n, bm.Size)
		}
		if hex.EncodeToString(sum.Sum(nil)) != bm.SHA256 {
			return nil, fmt.Errorf("the checksum of %s does not match its manifest -- the archive is damaged; copy it again", h.Name)
		}
	}
	// The end of the gzip stream carries its own checksum, read only when
	// everything before it has been.
	if n, err := io.Copy(io.Discard, io.LimitReader(gz, maxTrailing+1)); err != nil {
		return nil, fmt.Errorf("the archive is damaged (%w) -- copy it again", err)
	} else if n > maxTrailing {
		return nil, errors.New("the archive has data after its last member, which `backup create` never writes")
	}
	for _, bm := range m.Members {
		if !seen[bm.Name] {
			return nil, fmt.Errorf("%s is listed in the manifest but is not in the archive -- it is cut short; copy it again", bm.Name)
		}
	}
	return &m, nil
}

// backupMemberReader reads one member of an archive as a stream.
type backupMemberReader struct {
	io.Reader
	closers []io.Closer
}

func (r *backupMemberReader) Close() error {
	var first error
	for _, c := range r.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// openBackupMember opens the archive at path and positions on member name. The
// caller closes it. VerifyBackup has already checked the archive.
func openBackupMember(path, name string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			gz.Close()
			f.Close()
			return nil, fmt.Errorf("%s is not in the archive", name)
		}
		if err != nil {
			gz.Close()
			f.Close()
			return nil, err
		}
		if h.Name == name {
			return &backupMemberReader{Reader: tr, closers: []io.Closer{gz, f}}, nil
		}
	}
}

// readBackupMember reads a small member (a credentials file, the record) into
// memory.
func readBackupMember(path, name string) ([]byte, error) {
	r, err := openBackupMember(path, name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, maxSmallMember+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSmallMember {
		return nil, fmt.Errorf("%s is larger than a credentials file can be", name)
	}
	return b, nil
}
