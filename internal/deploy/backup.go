package deploy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// `versola backup create` writes one archive that holds what a deployment
// cannot get back by running `configure` again: the database, OpenBao's
// data, the keys that open OpenBao, and the CLI's record of the deployment.
// The generated bundle (compose file, configs) is left out on purpose:
// `configure` regenerates it from the same inputs.
//
// The archive is built in a staging directory next to its destination
// (every member is written to a file there first, because a tar header
// needs the member's size before its content, and a dump's size is not
// known until it is done), then packed into a temp file that is renamed into
// place at the very end. Whatever stops it halfway leaves no archive with a
// final name -- only the temp names, which are removed on the way out.

const (
	// BackupManifestName is the first member of every archive.
	BackupManifestName = "manifest.json"
	// backupFormat is the layout of the archive, for `backup restore` later.
	backupFormat = 1
	// backupPrefix starts every archive name; `backup list` reads only these.
	backupPrefix = "versola-backup-"
	backupSuffix = ".tar.gz"

	backupDumpMember    = "database.dump"
	backupOpenbaoMember = "openbao-volume.tar"
)

// BackupMember is one file in the archive, as the manifest lists it.
type BackupMember struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BackupSkipped is something the archive does not hold, and why.
type BackupSkipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// BackupManifest is manifest.json. No secret value is in it: names, sizes
// and checksums only.
type BackupManifest struct {
	Format         int       `json:"format"`
	CLIVersion     string    `json:"cliVersion"`
	VersolaVersion string    `json:"versolaVersion"`
	Target         string    `json:"target"`
	CreatedAt      time.Time `json:"createdAt"`
	// PostgresMode is "bundled" when the database is a container of the
	// deployment (vps with its own Postgres, and local), "external" when it
	// is the customer's own: a restore puts the dump back into the container
	// only in the first case.
	PostgresMode string `json:"postgresMode"`
	// PostgresDB and PostgresUser are where the dump came from.
	PostgresDB   string `json:"postgresDb,omitempty"`
	PostgresUser string `json:"postgresUser,omitempty"`
	// ProxyGeneration is the vps proxy's configuration in use, for
	// reference only: `configure` builds a new one.
	ProxyGeneration string `json:"proxyGeneration,omitempty"`
	// UnfinishedRollout says why the deployment was not one plain version
	// when it was backed up (a rolling upgrade under way or cut off): the
	// dump may already carry VersolaVersion's schema while older replicas
	// still ran. Empty for a plain deployment.
	UnfinishedRollout string          `json:"unfinishedRollout,omitempty"`
	Members           []BackupMember  `json:"members"`
	Skipped           []BackupSkipped `json:"skipped,omitempty"`
}

// BackupOptions are the flags of `backup create`.
type BackupOptions struct {
	// OutDir is where the archive goes; "" is DefaultBackupDir.
	OutDir string
	// SkipDatabase leaves the dump out (a managed database with snapshots
	// of its own).
	SkipDatabase bool
	// CLIVersion is recorded in the manifest.
	CLIVersion string
}

// backupOps is what CreateBackup does outside its own files: Docker, the
// home directory, the clock. A variable so the tests can stand in for it.
type backupOps struct {
	now func() time.Time
	// dumpDatabase writes a pg_dump -Fc of the deployment's database to w.
	dumpDatabase func(st *state.State, bundleDir string, pg pgTarget, w io.Writer) error
	// copyOpenbao writes a tar of OpenBao's data volume to w. skip is
	// non-empty (and nothing was written) when there is nothing this CLI
	// can copy safely.
	copyOpenbao func(target string, w io.Writer, out io.Writer) (skip string, err error)
	// openbaoDir is ~/.versola/openbao; stateDir is ~/.versola/active.
	openbaoDir func() (string, error)
	stateDir   func() (string, error)
	// proxyGeneration is the name `current` points at, "" when unknown.
	proxyGeneration func() string
}

var defaultBackupOps = backupOps{
	now:             time.Now,
	dumpDatabase:    dockerDumpDatabase,
	copyOpenbao:     dockerCopyOpenbao,
	openbaoDir:      openbao.Dir,
	stateDir:        state.Dir,
	proxyGeneration: currentProxyGeneration,
}

// DefaultBackupDir is ~/versola-backups: outside ~/.versola, so that
// `uninstall` and a hand-cleaned ~/.versola leave the backups alone.
func DefaultBackupDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't find home directory: %w", err)
	}
	return filepath.Join(home, "versola-backups"), nil
}

// CreateBackup writes the archive of st's deployment and returns its path.
// What it holds, and what it does not, is printed to out.
func CreateBackup(st *state.State, o BackupOptions, out io.Writer) (string, error) {
	return createBackup(st, o, out, defaultBackupOps)
}

func createBackup(st *state.State, o BackupOptions, out io.Writer, ops backupOps) (path string, err error) {
	if st.Target != "vps" && st.Target != "local" {
		return "", fmt.Errorf("backup works on a vps or local deployment, and this one is %q", st.Target)
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("the deployment's files are missing (%s) -- run `versola configure %s %s` again first", composePath, st.Target, st.Version)
	}
	bundleDir := filepath.Dir(composePath)

	outDir := o.OutDir
	if outDir == "" {
		if outDir, err = DefaultBackupDir(); err != nil {
			return "", err
		}
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", fmt.Errorf("couldn't create %s: %w", outDir, err)
	}
	sweepBackupLeftovers(outDir)

	now := ops.now().UTC()
	m := BackupManifest{
		Format:         backupFormat,
		CLIVersion:     o.CLIVersion,
		VersolaVersion: st.Version,
		Target:         st.Target,
		CreatedAt:      now,
		PostgresMode:   BackupPostgresMode(st),
	}
	if st.Target == "vps" {
		m.ProxyGeneration = ops.proxyGeneration()
	}
	if why, schema := unfinishedRollout(st); why != "" {
		m.UnfinishedRollout = why
		fmt.Fprintf(out, "Note: a rolling upgrade is not finished (%s); the manifest records it, and the dump may already have %s's schema.\n", why, schema)
	}
	name := backupFileName(st.Target, st.Version, now)
	path = filepath.Join(outDir, name)
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists -- run the command again in a second", path)
	}

	staging, err := os.MkdirTemp(outDir, backupTempPrefix(".staging-")+"*")
	if err != nil {
		return "", fmt.Errorf("couldn't create a staging directory in %s: %w", outDir, err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", err
	}

	var members []BackupMember
	add := func(member string, write func(w io.Writer) error) error {
		bm, err := stageMember(staging, member, write)
		if err != nil {
			return err
		}
		members = append(members, bm)
		fmt.Fprintf(out, "  %-40s %s\n", member, HumanSize(bm.Size))
		return nil
	}

	fmt.Fprintf(out, "Backing up the %s deployment (Versola %s) into %s\n", st.Target, st.Version, path)

	// The database first: it is the part that can fail for reasons outside
	// this machine (a managed database refusing the login), and the cheapest
	// to find out about before OpenBao is stopped.
	if o.SkipDatabase {
		m.Skipped = append(m.Skipped, BackupSkipped{backupDumpMember, "--skip-database"})
	} else {
		pg, err := backupPgTarget(bundleDir)
		if err != nil {
			return "", err
		}
		m.PostgresDB, m.PostgresUser = pg.db, pg.user
		fmt.Fprintf(out, "Dumping the database %s (pg_dump -Fc)...\n", pg.db)
		if err := add(backupDumpMember, func(w io.Writer) error { return ops.dumpDatabase(st, bundleDir, pg, w) }); err != nil {
			return "", err
		}
	}

	var skip string
	bm, err := stageMember(staging, backupOpenbaoMember, func(w io.Writer) error {
		var err error
		skip, err = ops.copyOpenbao(st.Target, w, out)
		return err
	})
	if err != nil {
		return "", err
	}
	if skip != "" {
		// Nothing was written: drop the empty member.
		_ = os.Remove(filepath.Join(staging, backupOpenbaoMember))
		m.Skipped = append(m.Skipped, BackupSkipped{backupOpenbaoMember, skip})
	} else {
		members = append(members, bm)
		fmt.Fprintf(out, "  %-40s %s\n", bm.Name, HumanSize(bm.Size))
	}

	// The files this CLI keeps of its own. Missing ones are not an error:
	// <target>-admin.json is absent when OpenBao was set up by hand, the
	// role script when the database is not the customer's own.
	baoDir, err := ops.openbaoDir()
	if err != nil {
		return "", err
	}
	stDir, err := ops.stateDir()
	if err != nil {
		return "", err
	}
	files := []struct{ member, src string }{
		{"openbao/" + st.Target + ".json", filepath.Join(baoDir, st.Target+".json")},
		{"openbao/" + st.Target + "-admin.json", filepath.Join(baoDir, st.Target+"-admin.json")},
		{"openbao/" + st.Target + "-postgres-role.sql", filepath.Join(baoDir, st.Target+"-postgres-role.sql")},
		{"state/state.json", filepath.Join(stDir, "state.json")},
	}
	if st.PostgresCA != "" {
		files = append(files, struct{ member, src string }{"state/" + state.PostgresCAFileName, filepath.Join(stDir, st.PostgresCA)})
	}
	for _, f := range files {
		src, err := os.Open(f.src)
		if errors.Is(err, os.ErrNotExist) {
			m.Skipped = append(m.Skipped, BackupSkipped{f.member, "not on this machine"})
			continue
		}
		if err != nil {
			return "", fmt.Errorf("couldn't read %s: %w", f.src, err)
		}
		err = add(f.member, func(w io.Writer) error { _, err := io.Copy(w, src); return err })
		src.Close()
		if err != nil {
			return "", err
		}
	}
	m.Members = members

	if err := packBackup(path, staging, m); err != nil {
		return "", err
	}
	printBackupSummary(out, path, m)
	if skip != "" {
		// Written, because the rest of it is still worth having, but not a
		// success: a dump without OpenBao's data can't be read after a
		// restore, and a backup run from cron must not look fine.
		return path, fmt.Errorf("%s was written WITHOUT OpenBao's data (%s) -- the database dump in it can't be read after a restore on its own; back OpenBao's data up as well", path, skip)
	}
	return path, nil
}

// unfinishedRollout is why st is not one plain deployment, "" when it is.
// schema is the version whose migrations the database may already carry:
// the newer of the two, which is PreviousVersion during an abort (Version is
// then the version it goes back to).
func unfinishedRollout(st *state.State) (why, schema string) {
	var parts []string
	schema = st.Version
	switch {
	case st.PreviousVersion != "" && st.RollingBack:
		parts = append(parts, "an abort back to "+st.Version+" from "+st.PreviousVersion+" is under way")
		schema = st.PreviousVersion
	case st.PreviousVersion != "":
		parts = append(parts, "a rolling upgrade from "+st.PreviousVersion+" to "+st.Version+" is under way")
	}
	if mixed := st.MixedVersions(); mixed != "" {
		parts = append(parts, mixed)
	}
	return strings.Join(parts, "; "), schema
}

// sweepBackupLeftovers removes the staging directories and temp archives a
// backup that was killed outright (a signal outside the window OpenBao is
// stopped in, a power cut) left in dir: a staging directory can hold a whole
// database dump. Only this machine's names are swept (they carry its host
// name, for an --out shared between servers): only backup create writes
// them, and it holds this machine's deployment lock, so none belongs to a
// run still going.
func sweepBackupLeftovers(dir string) {
	for _, pattern := range []string{backupTempPrefix(".staging-") + "*", backupTempPrefix(".tmp-") + "*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			_ = os.RemoveAll(m)
		}
	}
}

// backupTempPrefix is kind + versola-backup- + this host's name + "-".
func backupTempPrefix(kind string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "host"
	}
	// Glob metacharacters and separators out of the name.
	host = strings.Map(func(r rune) rune {
		if r == '-' || r == '.' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, host)
	return kind + backupPrefix + host + "-"
}

// BackupPostgresMode is the manifest's postgresMode: "bundled" when the
// database is a container of the deployment (vps with its own, and local).
func BackupPostgresMode(st *state.State) string {
	if st.Target == "local" || st.PostgresMode == PostgresModeBundled {
		return PostgresModeBundled
	}
	return "external"
}

// backupFileName is versola-backup-<target>-<version>-<UTC timestamp>.tar.gz.
func backupFileName(target, version string, t time.Time) string {
	return backupPrefix + target + "-" + version + "-" + t.UTC().Format("20060102T150405Z") + backupSuffix
}

// backupPgTarget reads where auth connects, from the bundle: the database
// and the role every service uses.
func backupPgTarget(bundleDir string) (pgTarget, error) {
	b, err := os.ReadFile(filepath.Join(bundleDir, AuthService+".conf"))
	if err != nil {
		return pgTarget{}, fmt.Errorf("couldn't read %s.conf to find the database: %w", AuthService, err)
	}
	t, err := parsePgTarget(string(b))
	if err != nil {
		return pgTarget{}, fmt.Errorf("couldn't read the Postgres settings in %s.conf: %w", AuthService, err)
	}
	return t, nil
}

// stageMember writes one member into the staging directory (0600) and
// returns its size and SHA-256.
func stageMember(staging, member string, write func(w io.Writer) error) (BackupMember, error) {
	dst := filepath.Join(staging, filepath.FromSlash(member))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return BackupMember{}, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return BackupMember{}, fmt.Errorf("couldn't create %s: %w", dst, err)
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h)}
	werr := write(cw)
	cerr := f.Close()
	if werr != nil {
		return BackupMember{}, werr
	}
	if cerr != nil {
		return BackupMember{}, fmt.Errorf("couldn't write %s: %w", dst, cerr)
	}
	return BackupMember{Name: member, Size: cw.n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// packBackup writes manifest.json and every staged member into a tar.gz
// at path, through a 0600 temp file in the same directory renamed into
// place at the end.
func packBackup(path, staging string, m BackupManifest) (err error) {
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode the manifest: %w", err)
	}
	mb = append(mb, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), backupTempPrefix(".tmp-")+"*")
	if err != nil {
		return fmt.Errorf("couldn't create a temp file next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("couldn't set permissions on %s: %w", tmpName, err)
	}

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	hdr := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: m.CreatedAt, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	}
	if err = tw.WriteHeader(hdr(BackupManifestName, int64(len(mb)))); err != nil {
		return err
	}
	if _, err = tw.Write(mb); err != nil {
		return err
	}
	for _, bm := range m.Members {
		if err = appendStaged(tw, filepath.Join(staging, filepath.FromSlash(bm.Name)), hdr(bm.Name, bm.Size)); err != nil {
			return fmt.Errorf("couldn't add %s to the archive: %w", bm.Name, err)
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("couldn't flush %s to disk: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("couldn't finish writing %s: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("couldn't move %s into place at %s: %w", tmpName, path, err)
	}
	return nil
}

func appendStaged(tw *tar.Writer, src string, h *tar.Header) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != h.Size {
		return fmt.Errorf("its size changed while it was packed (%d, then %d bytes)", h.Size, n)
	}
	return nil
}

// backupNotIncluded is what a restore has to get some other way. Printed
// after every backup and kept in one place so the tests can hold it.
func backupNotIncluded(target string) []string {
	var lines []string
	if target == "vps" {
		lines = append(lines, fmt.Sprintf("the ACME certificate volume (%s) is not included -- a redeploy requests a new certificate", proxy.ACMEVolume))
	}
	lines = append(lines,
		"the mutual-TLS volume is not included -- versola up reissues the certificates",
		"the deployment's generated files are not included -- versola configure writes them again")
	return lines
}

func printBackupSummary(out io.Writer, path string, m BackupManifest) {
	fmt.Fprintf(out, "\nWrote %s (mode 0600).\n", path)
	if len(m.Skipped) > 0 {
		fmt.Fprintln(out, "Left out:")
		for _, s := range m.Skipped {
			fmt.Fprintf(out, "  - %s: %s\n", s.Name, s.Reason)
		}
	}
	fmt.Fprintln(out, "Not in any backup:")
	for _, l := range backupNotIncluded(m.Target) {
		fmt.Fprintf(out, "  - %s\n", l)
	}
	fmt.Fprintln(out, "\nThe archive holds OpenBao's data together with the key that unseals it: whoever has the file has every")
	fmt.Fprintln(out, "secret of the deployment. Keep it like a password, and copy it off this server -- a backup that lives")
	fmt.Fprintln(out, "only on the server is lost with it.")
}

// HumanSize is n bytes in KiB, MiB or GiB.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- The database ------------------------------------------------------

// pgDumpScript runs pg_dump with the password taken from the container's
// environment, never from a command line: in the deployment's own Postgres
// container it is the variable the image was created with
// (postgres.secrets.env on vps), in the throw-away client container the one
// docker reads from auth.secrets.env (--env-file).
const pgDumpScript = `PGPASSWORD="$POSTGRES_PASSWORD" exec pg_dump --format=custom --no-password`

// pgDumpInContainerArgs dumps through `docker exec` into the deployment's
// own Postgres container: the server's own pg_dump (so its version always
// matches), over the container's local socket, nothing published.
func pgDumpInContainerArgs(container string, t pgTarget) []string {
	return []string{"exec", "-e", "PGUSER=" + t.user, "-e", "PGDATABASE=" + t.db, container, "sh", "-c", pgDumpScript}
}

// pgDumpExternalArgs dumps a Postgres that is not the deployment's own from
// the throw-away client container the rollout pre-flight uses: on the host's
// network, connecting and verifying TLS as the services do.
func pgDumpExternalArgs(t pgTarget, secretsFile string) []string {
	args := []string{"run", "--rm", "--pull", "missing", "--network", "host", "--env-file", secretsFile,
		"-e", "PGHOST=" + t.host, "-e", "PGPORT=" + t.port, "-e", "PGDATABASE=" + t.db, "-e", "PGUSER=" + t.user,
		"-e", "PGCONNECT_TIMEOUT=10", "-e", "PGAPPNAME=versola-backup"}
	args = append(args, pgTLSArgs(t.sslmode, t.caFile)...)
	return append(args, pgCheckImage, "sh", "-c", pgDumpScript)
}

func dockerDumpDatabase(st *state.State, bundleDir string, t pgTarget, w io.Writer) error {
	var args []string
	if BackupPostgresMode(st) == PostgresModeBundled {
		composePath, _, err := st.ComposeFilePath()
		if err != nil {
			return err
		}
		out, err := docker.Output(state.ComposeArgs(composePath, "ps", "-q", "postgres")...)
		id := strings.TrimSpace(string(out))
		if err != nil || id == "" {
			return fmt.Errorf("the deployment's Postgres container isn't running -- `versola up` starts it")
		}
		args = pgDumpInContainerArgs(strings.Fields(id)[0], t)
	} else {
		if t.rootCert != "" && pgVerifies(t.sslmode) {
			ca, err := pgCAOnHost(bundleDir, t.rootCert)
			if err != nil {
				return err
			}
			t.caFile = ca
		}
		args = pgDumpExternalArgs(t, filepath.Join(bundleDir, AuthService+".secrets.env"))
	}
	stderr, err := docker.RunTo(w, args...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 125 {
			// docker itself failed (pulling the image, or reading the
			// secrets file -- whose parse errors can quote the file): its
			// message is not repeated.
			return fmt.Errorf("docker couldn't run pg_dump (pulling %s, or starting it, failed) -- nothing was written", pgCheckImage)
		}
		// pg_dump's own errors name the host, the user and the database,
		// never the password.
		return fmt.Errorf("pg_dump failed%s -- nothing was written", firstLine(stderr))
	}
	return nil
}

// ---- OpenBao -----------------------------------------------------------

// openbaoTarArgs reads OpenBao's data volume, mounted read-only, as a tar
// on stdout. --pull never: the image is made present before OpenBao is
// stopped (openbaoCtl.prepare), so nothing is downloaded while it is down. The client image of the Postgres checks is reused: it is
// already on a server that ran them, and has tar.
func openbaoTarArgs(volume string) []string {
	return []string{"run", "--rm", "--pull", "never", "--network", "none", "-v", volume + ":/src:ro", pgCheckImage, "tar", "-C", "/src", "-cf", "-", "."}
}

// openbaoCtl is what copying OpenBao's volume does with Docker and OpenBao:
// a variable so the tests can stand in for each step.
type openbaoCtl struct {
	volumeExists func(volume string) bool
	// inspect: whether the container exists, runs, and runs on an
	// openbao.hcl that is gone (see inspectOpenbao).
	inspect func(target string) (exists, running, configMissing bool, err error)
	// canUnseal: whether this machine can unseal it after a restart (a saved
	// key, and a root token the running instance accepts).
	canUnseal func(target, address string) (ok bool, why string, err error)
	stop      func(container string) error
	start     func(container string) error
	unseal    func(target, address string, out io.Writer) error
	tar       func(volume string, w io.Writer) error
	// prepare makes sure the image tar runs from is on this machine, before
	// OpenBao is stopped: a pull that hangs or fails must not happen while
	// it is down.
	prepare func() error
	// signals receives the signals held while OpenBao is stopped; stop
	// releases them.
	signals func() (<-chan os.Signal, func())
}

var dockerOpenbaoCtl = openbaoCtl{
	volumeExists: func(volume string) bool {
		_, err := docker.Output("volume", "inspect", "--format", "{{.Name}}", volume)
		return err == nil
	},
	inspect:   inspectOpenbao,
	canUnseal: func(target, address string) (bool, string, error) { return canUnsealOpenbao(target, address, false) },
	stop:      func(c string) error { return docker.RunQuiet("stop", c) },
	start:     func(c string) error { return docker.RunQuiet("start", c) },
	unseal: func(target, address string, out io.Writer) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return openbao.EnsureUnsealed(ctx, target, address, out)
	},
	tar: tarVolume,
	prepare: func() error {
		if _, err := docker.Output("image", "inspect", "--format", "{{.Id}}", pgCheckImage); err == nil {
			return nil
		}
		if _, stderr, err := docker.OutputWithin(5*time.Minute, "pull", pgCheckImage); err != nil {
			return fmt.Errorf("couldn't pull %s, which copies OpenBao's data: %v%s", pgCheckImage, err, firstLine(stderr))
		}
		return nil
	},
	signals: func() (<-chan os.Signal, func()) {
		// The signals a vps session ends with, not only Ctrl-C: a dropped SSH
		// connection (SIGHUP) or a kill (SIGTERM) between the stop and the
		// start would leave OpenBao stopped (the same set a rollout holds).
		ch := make(chan os.Signal, 3)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		// And SIGPIPE, on a channel of its own that nobody reads: with output
		// piped (`| tee`) over a session that drops, the next line written to
		// the dead pipe would otherwise kill the process. Notified, a write
		// just fails with EPIPE, and it is not an interruption.
		pipe := make(chan os.Signal, 1)
		signal.Notify(pipe, syscall.SIGPIPE)
		return ch, func() { signal.Stop(ch); signal.Stop(pipe) }
	},
}

func dockerCopyOpenbao(target string, w io.Writer, out io.Writer) (string, error) {
	return copyOpenbaoWith(dockerOpenbaoCtl, target, w, out)
}

// openbaoContainerAddress is where the container this stops and starts
// listens (configure checks and unseals it there too): not the address in
// the stored credentials, which can name another OpenBao.
const openbaoContainerAddress = "http://localhost:8200"

// copyOpenbaoWith copies OpenBao's data volume. OpenBao's file storage has
// no online snapshot (`bao operator raft snapshot` is for raft storage only),
// and copying its files while it writes them could capture keys from two
// different moments. So a running OpenBao is stopped for the copy -- a few
// seconds; the services keep running, they read their secrets from their own
// *.secrets.env, not from OpenBao -- and started and unsealed again with the
// key this CLI saved.
//
// It is stopped only when it is sure to come back: it runs on a
// configuration that still exists (a container on a pruned openbao.hcl
// would not start again), and this machine holds an unseal key and a root
// token the running instance accepts. Otherwise it is not stopped, the copy
// is left out, and the reason is returned as skip -- no question is needed
// for a stop that both checks have cleared.
func copyOpenbaoWith(c openbaoCtl, target string, w io.Writer, out io.Writer) (skip string, err error) {
	volume := OpenbaoVolumeName(target)
	if !c.volumeExists(volume) {
		return fmt.Sprintf("there is no Docker volume %s on this machine -- back OpenBao's data up where it lives", volume), nil
	}
	_, running, configMissing, err := c.inspect(target)
	if err != nil {
		return "", fmt.Errorf("couldn't check OpenBao's container: %w", err)
	}
	if err := c.prepare(); err != nil {
		return "", err
	}
	if !running {
		// Nothing writes to it: the copy is consistent as it is.
		return "", c.tar(volume, w)
	}
	if configMissing {
		return fmt.Sprintf("OpenBao runs on a configuration file that is gone, so it would not start again after a stop -- `versola configure %s <version>` recreates it; back up after that", target), nil
	}
	address := openbaoContainerAddress
	// Sealed is its normal state after a reboot of the server, and the token
	// check below can't pass against a sealed OpenBao: unseal it first (a
	// no-op when it already is). If that fails, it can't come back either.
	if err := c.unseal(target, address, out); err != nil {
		return "OpenBao is not stopped for a consistent copy, because it is sealed and couldn't be unsealed: " + err.Error(), nil
	}
	ok, why, err := c.canUnseal(target, address)
	if err != nil {
		return "", fmt.Errorf("couldn't check that OpenBao can be unsealed after a restart: %w", err)
	}
	if !ok {
		return "OpenBao is not stopped for a consistent copy, because this machine couldn't unseal it again: " + why, nil
	}
	return copyWithOpenbaoStopped(c, target, OpenbaoContainerName(target), address, out, func() error { return c.tar(volume, w) })
}

func tarVolume(volume string, w io.Writer) error {
	stderr, err := docker.RunTo(w, openbaoTarArgs(volume)...)
	if err != nil {
		return fmt.Errorf("couldn't copy OpenBao's data volume %s: %v%s", volume, err, firstLine(stderr))
	}
	return nil
}

// copyWithOpenbaoStopped stops OpenBao, runs copy, and starts and unseals it
// again whatever copy returned -- and also when the stop itself failed, which
// may have stopped it anyway. The signals that end a session are held until
// OpenBao runs again: leaving it stopped is the one thing this must not do.
func copyWithOpenbaoStopped(c openbaoCtl, target, container, address string, out io.Writer, copy func() error) (skip string, err error) {
	sigs, release := c.signals()
	defer release()
	// Say at once that the signal was heard: a silent Ctrl-C is pressed
	// again, and then comes kill -9, which would leave OpenBao stopped.
	var heard atomic.Bool
	done, stopped := make(chan struct{}), make(chan struct{})
	// Ends the watcher and waits for it, so that a signal it already took is
	// counted; then one still in the channel is taken too.
	stopWatching := func() bool {
		select {
		case <-done:
		default:
			close(done)
			<-stopped
		}
		select {
		case <-sigs:
			heard.Store(true)
		default:
		}
		return heard.Load()
	}
	defer stopWatching()
	go func() {
		defer close(stopped)
		for {
			select {
			case <-sigs:
				if !heard.Swap(true) {
					fmt.Fprintln(out, "Interrupted -- stopping once OpenBao runs again and is unsealed.")
				}
			case <-done:
				return
			}
		}
	}()
	docker.SetShielded(true)
	defer docker.SetShielded(false)

	fmt.Fprintf(out, "Stopping OpenBao for the copy (a few seconds; the services keep running)...\n")
	var copyErr error
	if err := c.stop(container); err != nil {
		copyErr = fmt.Errorf("couldn't stop OpenBao for the copy: %w -- nothing was copied", err)
	} else {
		copyErr = copy()
	}

	fmt.Fprintf(out, "Starting OpenBao again...\n")
	if err := c.start(container); err != nil {
		return "", fmt.Errorf("OpenBao was stopped for the copy and couldn't be started again (%v) -- the services keep running; start it with `docker start %s`, then `versola secrets test %s` unseals it", err, container, target)
	}
	if err := c.unseal(target, address, out); err != nil {
		return "", fmt.Errorf("OpenBao is running again but couldn't be unsealed: %w", err)
	}
	if stopWatching() {
		return "", errors.New("interrupted -- OpenBao runs again; no archive was written")
	}
	return "", copyErr
}

// currentProxyGeneration is the generation the vps proxy's `current` link
// names, "" when there is none.
func currentProxyGeneration() string {
	dir, err := proxyGenerationsDir()
	if err != nil {
		return ""
	}
	target, err := os.Readlink(filepath.Join(dir, proxy.CurrentGeneration))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// ---- backup list -------------------------------------------------------

// BackupEntry is one archive in a directory, for `backup list`.
type BackupEntry struct {
	Path     string
	Size     int64
	Manifest *BackupManifest
	// Err is why the manifest couldn't be read.
	Err error
}

// ListBackups returns the archives in dir ("" is DefaultBackupDir), newest
// first by name (the name ends with the UTC time it was made).
func ListBackups(dir string) ([]BackupEntry, string, error) {
	if dir == "" {
		var err error
		if dir, err = DefaultBackupDir(); err != nil {
			return nil, "", err
		}
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, dir, nil
	}
	if err != nil {
		return nil, dir, err
	}
	var list []BackupEntry
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, backupPrefix) || !strings.HasSuffix(n, backupSuffix) {
			continue
		}
		p := filepath.Join(dir, n)
		be := BackupEntry{Path: p}
		if fi, err := e.Info(); err == nil {
			be.Size = fi.Size()
		}
		be.Manifest, be.Err = ReadBackupManifest(p)
		list = append(list, be)
	}
	sort.Slice(list, func(i, j int) bool {
		ti, tj := backupTime(list[i]), backupTime(list[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return list[i].Path > list[j].Path
	})
	return list, dir, nil
}

func backupTime(e BackupEntry) time.Time {
	if e.Manifest != nil {
		return e.Manifest.CreatedAt
	}
	return time.Time{}
}

// ReadBackupManifest reads manifest.json, the first member of an archive.
func ReadBackupManifest(path string) (*BackupManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("not a tar archive: %w", err)
	}
	if h.Name != BackupManifestName {
		return nil, fmt.Errorf("its first member is %s, not %s", h.Name, BackupManifestName)
	}
	var m BackupManifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("couldn't read %s: %w", BackupManifestName, err)
	}
	return &m, nil
}
