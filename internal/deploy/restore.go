package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/fsutil"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/state"
)

// `versola backup restore` puts back what `backup create` took: the data that
// `configure` cannot make again. It is one command that does one of two things,
// by what this machine already has, because the database cannot be put back
// until the deployment's own Postgres container exists, and that container
// comes from the bundle `configure` writes:
//
//   - On a machine with nothing deployed (a new server, or one after
//     `uninstall --purge`): OpenBao's data and keys, and an empty Postgres data
//     volume (so that `configure` does not take the machine for one that lost
//     its database). It prints the `configure` command that follows, with the
//     flags the archive's record holds.
//   - On a machine whose deployment `configure` has set up, on the Versola
//     version the archive was taken on, with its services stopped: the
//     database, with the deployment's own Postgres started for it alone.
//
// A deployment that is already there is never overwritten by the first and
// never by the second unless its database is empty or --replace-database
// says so and "vps" is typed. Only a vps deployment with its own Postgres is
// restored; a Postgres of the customer's own is theirs to restore.

// RestoreOptions are the flags of `backup restore`.
type RestoreOptions struct {
	// Archive is the path of the archive.
	Archive string
	// ReplaceDatabase is --replace-database: the deployment's database has
	// data in it, and the restore may replace it.
	ReplaceDatabase bool
	// AssumeYes is --yes: answers the plain confirmation, never the typed one.
	AssumeYes bool
	// In is where the answers are read from (os.Stdin).
	In io.Reader
}

// restoreWord is what replacing a database that has data in it asks to be
// typed, like `uninstall --purge`: --yes does not answer it.
const restoreWord = "vps"

// restoreOps is what a restore does outside its own files: Docker, the home
// directory. A variable so the tests can stand in for it.
type restoreOps struct {
	loadState    func() (*state.State, error)
	openbaoDir   func() (string, error)
	dockerDaemon func() error
	// exists asks Docker whether a "volume" or a "container" of that name is
	// there; an error when it could not be asked.
	exists func(kind, name string) (bool, error)
	// prepareImage makes sure the image the volume is filled from is here.
	prepareImage  func() error
	createVolume  func(name string) error
	removeVolume  func(name string) error
	extractVolume func(volume string, r io.Reader) error
	// runningServices are the compose services running now, by name.
	runningServices func(composePath string) ([]string, error)
	bundledPg       bundledPgCtl
	// tableCount is how many tables the database holds, in any schema.
	tableCount func(container string, t pgTarget) (int, error)
	pgRestore  func(container string, t pgTarget, r io.Reader) error
	// signals receives the signals that end a session; release stops it.
	signals func() (<-chan os.Signal, func())
}

var defaultRestoreOps = restoreOps{
	loadState:  state.Load,
	openbaoDir: openbao.Dir,
	dockerDaemon: func() error {
		if r := checks.DockerDaemon(); !r.OK {
			return errors.New(r.String())
		}
		return nil
	},
	exists: func(kind, name string) (bool, error) {
		_, stderr, err := docker.OutputWithin(time.Minute, kind, "inspect", "--format", "{{.Name}}", name)
		if err == nil {
			return true, nil
		}
		if strings.Contains(stderr, "No such") || strings.Contains(stderr, "no such") {
			return false, nil
		}
		return false, fmt.Errorf("couldn't ask Docker about the %s %s%s", kind, name, firstLine(stderr))
	},
	prepareImage: dockerOpenbaoCtl.prepare,
	createVolume: func(name string) error { return docker.RunQuiet("volume", "create", name) },
	removeVolume: func(name string) error { return docker.RunQuiet("volume", "rm", name) },
	extractVolume: func(volume string, r io.Reader) error {
		stderr, err := docker.RunWithStdin(r, extractVolumeArgs(volume)...)
		if err != nil {
			return fmt.Errorf("couldn't fill the volume %s from the archive%s", volume, firstLine(stderr))
		}
		return nil
	},
	runningServices: func(composePath string) ([]string, error) {
		out, stderr, err := docker.OutputWithin(time.Minute, state.ComposeArgs(composePath, runningServicesArgs()...)...)
		if err != nil {
			return nil, fmt.Errorf("couldn't ask Docker which services run%s: %w", firstLine(stderr), err)
		}
		return strings.Fields(string(out)), nil
	},
	bundledPg: dockerBundledPg,
	tableCount: func(container string, t pgTarget) (int, error) {
		stderr, out, err := runPsqlCount(container, t)
		if err != nil {
			return 0, fmt.Errorf("couldn't count the tables of the database%s", firstLine(stderr))
		}
		n, perr := strconv.Atoi(strings.TrimSpace(out))
		if perr != nil {
			return 0, errors.New("couldn't read how many tables the database holds")
		}
		return n, nil
	},
	pgRestore: func(container string, t pgTarget, r io.Reader) error {
		stderr, err := docker.RunWithStdinTo(r, io.Discard, pgRestoreArgs(container, t)...)
		if err != nil {
			// The first line of the message only: a failing row can quote data
			// in the lines after it. `versola logs postgres` has the rest.
			return fmt.Errorf("the restore failed%s -- it ran in one transaction that was not committed, so the database is as it was (`versola logs postgres` has more)", firstStderrLine(stderr))
		}
		return nil
	},
	signals: dockerOpenbaoCtl.signals,
}

// extractVolumeArgs fills a data volume from a tar on stdin: no network, no
// other mount, so that a member of the archive can only land in the volume.
// --pull never: restoreIdentity makes the image present first.
func extractVolumeArgs(volume string) []string {
	return []string{"run", "--rm", "-i", "--pull", "never", "--network", "none", "--security-opt", "no-new-privileges", "-v", volume + ":/dst", pgCheckImage, "tar", "-C", "/dst", "-xpf", "-"}
}

// runningServicesArgs lists the compose services that run, by name.
func runningServicesArgs() []string {
	return []string{"ps", "--services", "--status", "running", "--status", "restarting", "--status", "paused"}
}

// The SQL and the scripts below read the password from the container's own
// environment, like the dump does (pgDumpScript): never from a command line.
const (
	pgCountScript = `PGPASSWORD="$POSTGRES_PASSWORD" exec psql --no-password --tuples-only --no-align --quiet`
	pgCountSQL    = "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog', 'information_schema');\n"

	// pgDropSQL empties the database before the dump is applied: every schema
	// but the system ones, with everything in it, and a new public. A plain
	// `pg_restore --clean` drops only what the dump has, so a table a newer
	// version made (the very thing a rollback after a failed upgrade has to get
	// rid of) would stay, and the old version's migrations would then fail on it.
	pgDropSQL = `DO $drop$
DECLARE s text;
BEGIN
  FOR s IN SELECT nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema' LOOP
    EXECUTE format('DROP SCHEMA %I CASCADE', s);
  END LOOP;
  CREATE SCHEMA public;
END
$drop$;`

	// pgRestoreScript runs inside the Postgres container. pg_restore turns the
	// dump on stdin into SQL, and psql runs that SQL after the drop above, all
	// of it between one BEGIN and one COMMIT. The COMMIT is sent only when
	// pg_restore itself finished: psql alone would commit whatever reached it
	// before a broken dump ended. Anything that goes wrong leaves the
	// connection without a COMMIT, so the database is as it was. The status of
	// pg_restore is carried in a file because a pipeline reports only psql's
	// (this runs in dash, which has no pipefail). The password is read from the
	// container's own environment, never from a command line.
	pgRestoreScript = `st=/tmp/.versola-restore-status
rm -f "$st"
PGPASSWORD="$POSTGRES_PASSWORD"
export PGPASSWORD
{
  echo 'BEGIN;'
  echo "SET LOCAL client_min_messages = warning;"
  echo "SET LOCAL lock_timeout = '30s';"
  cat <<'SQL'
` + pgDropSQL + `
SQL
  if pg_restore --no-password --no-owner -f -; then echo 0 > "$st" && echo 'COMMIT;'; else echo 1 > "$st"; fi
} | psql --no-password --quiet --set ON_ERROR_STOP=on --dbname "$PGDATABASE"
rc=$?
done=$(cat "$st" 2>/dev/null)
rm -f "$st"
[ "$rc" = 0 ] && [ "$done" = 0 ]`
)

// pgRestoreArgs restores a custom-format dump on stdin into the deployment's
// own container: everything in the database replaced by the dump, in one
// transaction (see pgRestoreScript), owned by the role it connects as
// (--no-owner).
func pgRestoreArgs(container string, t pgTarget) []string {
	return []string{"exec", "-i", "-e", "PGUSER=" + t.user, "-e", "PGDATABASE=" + t.db, container, "sh", "-c", pgRestoreScript}
}

func pgCountArgs(container string, t pgTarget) []string {
	return []string{"exec", "-i", "-e", "PGUSER=" + t.user, "-e", "PGDATABASE=" + t.db, container, "sh", "-c", pgCountScript}
}

func runPsqlCount(container string, t pgTarget) (stderr, stdout string, err error) {
	var out strings.Builder
	stderr, err = docker.RunWithStdinTo(strings.NewReader(pgCountSQL), &out, pgCountArgs(container, t)...)
	return stderr, out.String(), err
}

// firstStderrLine is ": <the first line of stderr>", cut short and printable.
//
// The line that says what went wrong, when there is one: the first one with
// "ERROR" or "error:" in it (psql's and pg_restore's own words), else the first
// line. A NOTICE can come before the cause.
func firstStderrLine(s string) string {
	var first string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if first == "" {
				first = l
			}
			if strings.Contains(l, "ERROR") || strings.Contains(l, "error:") {
				first = l
				break
			}
		}
	}
	if first == "" {
		return ""
	}
	if len(first) > 160 {
		first = first[:160]
	}
	return ": " + quoteSafe(first)
}

// quoteSafe keeps text that came from a program's output off the terminal's
// control sequences.
func quoteSafe(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// RestoreBackup restores the archive at o.Archive: see the comment at the top
// of this file for which of the two things it does.
func RestoreBackup(o RestoreOptions, out io.Writer) error {
	return restoreBackup(o, out, defaultRestoreOps)
}

func restoreBackup(o RestoreOptions, out io.Writer, ops restoreOps) error {
	m, err := VerifyBackup(o.Archive)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "The archive is complete: Versola %s, taken %s, every member matches its checksum (that shows damage, not who made it).\n", m.VersolaVersion, m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if m.UnfinishedRollout != "" {
		fmt.Fprintf(out, "Note: it was taken during a rolling upgrade that was not finished (%s): its database may already have a newer schema than %s.\n", sanitizeForMessage(m.UnfinishedRollout), m.VersolaVersion)
	}
	if m.PostgresMode != PostgresModeBundled {
		return fmt.Errorf("the archive's database is a Postgres of your own, and `backup restore` puts back the deployment's own Postgres only. The dump is in the archive as %s: `tar -xzf %s %s`, then `pg_restore --clean --if-exists --no-owner --single-transaction --dbname <database> %s` against your server (it replaces what is in that database; and OpenBao's data, in %s, is restored with `docker run` and tar)", backupDumpMember, quotePath(o.Archive), backupDumpMember, backupDumpMember, backupOpenbaoMember)
	}
	if !m.hasMember(backupDumpMember) {
		why := m.skippedReason(backupDumpMember)
		if why != "" {
			why = " (" + why + ")"
		}
		return fmt.Errorf("the archive has no database dump%s: there is nothing to restore the deployment's database from", why)
	}
	if err := ops.dockerDaemon(); err != nil {
		return err
	}

	st, err := ops.loadState()
	switch {
	case err == nil && st.Target == "vps":
		return restoreDatabase(st, m, o, out, ops)
	case err == nil:
		return fmt.Errorf("this machine tracks a %s deployment, and the archive is of a vps one -- `backup restore` puts back a vps deployment on a machine with none, or its database into the one `configure vps` has set up", st.Target)
	case errors.Is(err, state.ErrNotConfigured):
		return restoreIdentity(m, o, out, ops)
	case errors.Is(err, state.ErrOldInstall):
		return fmt.Errorf("this machine has a deployment of an older versola-cli: %w", err)
	default:
		return err
	}
}

// ---- A machine with nothing deployed -----------------------------------

// restoreCreated is what a restore made on a machine that had none of it, so
// that a failure undoes exactly that.
type restoreCreated struct {
	files   []string
	volumes []string
}

func (c *restoreCreated) undo(ops restoreOps, out io.Writer) {
	for i := len(c.files) - 1; i >= 0; i-- {
		if err := os.Remove(c.files[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "Warning: couldn't remove %s: %v\n", c.files[i], err)
		}
	}
	for i := len(c.volumes) - 1; i >= 0; i-- {
		if err := ops.removeVolume(c.volumes[i]); err != nil {
			fmt.Fprintf(out, "Warning: couldn't remove the Docker volume %s: %v -- `docker volume rm %s` does.\n", c.volumes[i], err, c.volumes[i])
		}
	}
	if len(c.files)+len(c.volumes) > 0 {
		fmt.Fprintln(out, "What this run had put back was removed again.")
	}
}

var restoredCredentialFiles = []string{"openbao/vps.json", "openbao/vps-admin.json", "openbao/vps-postgres-role.sql"}

func restoreIdentity(m *BackupManifest, o RestoreOptions, out io.Writer, ops restoreOps) error {
	for _, need := range []string{backupOpenbaoMember, "openbao/vps.json", "state/state.json"} {
		if !m.hasMember(need) {
			why := m.skippedReason(need)
			if why != "" {
				why = " (" + why + ")"
			}
			return fmt.Errorf("the archive has no %s%s: without it the deployment cannot be brought back, and the database in the archive would not be readable", need, why)
		}
	}
	baoDir, err := ops.openbaoDir()
	if err != nil {
		return err
	}
	// The machine must have none of what this puts back: nothing is replaced
	// by a restore of this kind, so nothing is lost by it either.
	var found []string
	for _, f := range restoredCredentialFiles {
		p := filepath.Join(baoDir, filepath.Base(f))
		if _, err := os.Lstat(p); err == nil {
			found = append(found, "the file "+p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("couldn't look at %s: %w", p, err)
		}
	}
	for _, what := range []struct{ kind, name string }{
		{"volume", OpenbaoVolumeName("vps")}, {"volume", bundledPostgresVolume},
		{"container", OpenbaoContainerName("vps")}, {"container", bundledPostgresContainer},
	} {
		ok, err := ops.exists(what.kind, what.name)
		if err != nil {
			return err
		}
		if ok {
			found = append(found, "the Docker "+what.kind+" "+what.name)
		}
	}
	if p, perr := state.LoadPending(); perr != nil {
		found = append(found, "an unreadable note of an unfinished first install")
	} else if p != nil {
		found = append(found, "the note of an unfinished first install")
	}
	if len(found) > 0 {
		return fmt.Errorf("this machine already has parts of a Versola deployment:\n  - %s\nA restore puts an archive back on a machine that has none, and replaces nothing. To replace what is here with the archive's (this removes it for good, and asks first): `versola uninstall --purge`, then run this command again", strings.Join(found, "\n  - "))
	}
	if err := ops.prepareImage(); err != nil {
		return err
	}

	rec, err := archivedRecord(o.Archive)
	if err != nil {
		return err
	}
	byHand := !m.hasMember("openbao/vps-admin.json")

	action := fmt.Sprintf("put OpenBao's data and keys of the archive (Versola %s) back on this machine, and create an empty Postgres data volume for the database that follows. Nothing is started, and nothing that is here is replaced", m.VersolaVersion)
	if err := confirmVpsFrom(o.In, o.AssumeYes, action); err != nil {
		return err
	}

	// A Ctrl-C or a dropped session must not leave half a volume behind: the
	// docker calls are shielded from it, and it is handled between the steps.
	sigs, release := ops.signals()
	defer release()
	docker.SetShielded(true)
	defer docker.SetShielded(false)
	heard := false
	interrupted := func() bool {
		if heard {
			return true
		}
		select {
		case <-sigs:
			heard = true
		default:
		}
		return heard
	}

	var made restoreCreated
	fail := func(err error) error {
		made.undo(ops, out)
		if interrupted() {
			return exitcode.With(exitcode.Interrupted, errors.New("interrupted -- nothing is left of this run"))
		}
		return err
	}

	if err := os.MkdirAll(baoDir, 0o700); err != nil {
		return fail(fmt.Errorf("couldn't create %s: %w", baoDir, err))
	}
	var restored []string
	for _, f := range restoredCredentialFiles {
		if !m.hasMember(f) {
			continue
		}
		b, err := readBackupMember(o.Archive, f)
		if err != nil {
			return fail(fmt.Errorf("couldn't read %s from the archive: %w", f, err))
		}
		dst := filepath.Join(baoDir, filepath.Base(f))
		if err := fsutil.WriteFileAtomic(dst, b, 0o600); err != nil {
			return fail(err)
		}
		made.files = append(made.files, dst)
		restored = append(restored, filepath.Base(f))
	}
	if interrupted() {
		return fail(nil)
	}

	vol := OpenbaoVolumeName("vps")
	fmt.Fprintf(out, "Filling OpenBao's data volume %s...\n", vol)
	if err := createFreshVolume(ops, vol); err != nil {
		return fail(err)
	}
	made.volumes = append(made.volumes, vol)
	r, err := openBackupMember(o.Archive, backupOpenbaoMember)
	if err != nil {
		return fail(err)
	}
	err = ops.extractVolume(vol, r)
	r.Close()
	if err != nil {
		return fail(err)
	}
	if interrupted() {
		return fail(nil)
	}
	if err := createFreshVolume(ops, bundledPostgresVolume); err != nil {
		return fail(err)
	}
	made.volumes = append(made.volumes, bundledPostgresVolume)
	if interrupted() {
		return fail(nil)
	}

	sort.Strings(restored)
	fmt.Fprintf(out, "\nPut back: OpenBao's data (volume %s), its credentials in %s (%s), and an empty Postgres data volume (%s).\n", vol, baoDir, strings.Join(restored, ", "), bundledPostgresVolume)
	fmt.Fprintf(out, "\nThe database is next. It needs the deployment's Postgres, which `configure` creates, so:\n\n")
	fmt.Fprintf(out, "  1. %s     (built from the archive's record: read its values before you run it)\n", RestoredConfigureCommand(m.VersolaVersion, rec, byHand))
	fmt.Fprintf(out, "  2. versola backup restore %s     (the same command: now it puts the database back)\n", quotePath(o.Archive))
	fmt.Fprintf(out, "  3. versola up\n")
	fmt.Fprintf(out, "\nThe domain in --auth-url has to point at this server before `up`; `versola doctor --target vps` checks it.\n")
	return nil
}

// archivedRecord reads the archive's copy of state.json, only for the values
// it prints: nothing from it is written to this machine.
func archivedRecord(path string) (*state.State, error) {
	b, err := readBackupMember(path, "state/state.json")
	if err != nil {
		return nil, fmt.Errorf("couldn't read the deployment's record from the archive: %w", err)
	}
	var rec state.State
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("the deployment's record in the archive is not readable: %w", err)
	}
	// A record of a layout newer than this build reads may hold settings the
	// command it prints would silently lack.
	if rec.SchemaVersion > state.SchemaVersion {
		return nil, fmt.Errorf("the archive's record was written by a newer versola (state layout %d, this one reads up to %d) -- update versola (`versola upgrade`)", rec.SchemaVersion, state.SchemaVersion)
	}
	if rec.Target != "vps" {
		return nil, errors.New("the archive's record is not of a vps deployment")
	}
	return &rec, nil
}

// RestoredConfigureCommand is the `configure` that follows a restore, with the
// flags the archive's record holds, so that the deployment comes back as it
// was. What it prints comes from a file: each value is quoted, and printable
// ASCII only.
func RestoredConfigureCommand(version string, rec *state.State, openbaoByHand bool) string {
	parts := []string{"versola", "configure", "vps", version}
	add := func(flag, val string) {
		if val != "" {
			parts = append(parts, flag, quoteArg(val))
		}
	}
	add("--auth-url", rec.AuthURL)
	if rec.ProxyMode != "" {
		add("--proxy", rec.ProxyMode)
	}
	add("--central-url", rec.CentralURL)
	add("--mtls-domain", rec.MTLSDomain)
	add("--central-mtls-domain", rec.CentralMTLSDomain)
	add("--central-domain", rec.CentralDomain)
	if rec.CertLifetimeDays != nil {
		add("--cert-lifetime-days", strconv.Itoa(*rec.CertLifetimeDays))
	}
	if rec.EstEnabled {
		parts = append(parts, "--est")
	}
	add("--registry", rec.ImageRegistry)
	if rec.PostgresDB != "" && rec.PostgresDB != "auth" {
		add("--postgres-db", rec.PostgresDB)
	}
	if openbaoByHand {
		parts = append(parts, "--setup-openbao")
	}
	return strings.Join(parts, " ")
}

// quoteArg makes s one word of a shell command: as it is when it is made of
// the characters a path or a URL uses, in single quotes otherwise. Anything
// outside printable ASCII becomes '?': the value comes from a file.
func quoteArg(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '\'' {
			c = '?'
		}
		b = append(b, c)
	}
	return quoteWord(string(b))
}

// quotePath is quoteArg for the path the customer typed: theirs, not the
// archive's, so it is kept as it is (a name with letters outside ASCII is
// fine), and only quoted for the shell.
func quotePath(s string) string {
	return quoteWord(strings.ReplaceAll(s, "'", `'\''`))
}

// quoteWord leaves s as it is when it is made of the characters a path or a
// URL uses, and puts it in single quotes otherwise.
func quoteWord(s string) string {
	if s != "" && strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._:/@%+=,-") == "" {
		return s
	}
	return "'" + s + "'"
}

// ---- A machine whose deployment configure has set up -------------------

func restoreDatabase(st *state.State, m *BackupManifest, o RestoreOptions, out io.Writer, ops restoreOps) error {
	if st.PostgresMode != PostgresModeBundled {
		return errors.New("this deployment uses a Postgres of your own, which `backup restore` does not touch: restore the dump (database.dump in the archive) into it with pg_restore")
	}
	if st.Version != m.VersolaVersion {
		return fmt.Errorf("the archive was taken on Versola %s, and this deployment is on %s: a dump is only put back on the version that wrote it (a newer one would need migrations, an older one cannot read the schema). Run `versola configure vps %s` first (then `versola backup restore` again); the database is not touched until the versions match", m.VersolaVersion, st.Version, m.VersolaVersion)
	}
	if why := st.MixedVersions(); why != "" || st.RolloutUnderway() {
		return errors.New("a rolling upgrade of this deployment is not finished: finish it with `versola rollout`, or go back with `versola rollout --abort`, before restoring a database")
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment's files are missing (%s) -- run `versola configure vps %s` first", composePath, st.Version)
	}
	bundleDir := filepath.Dir(composePath)
	pg, err := backupPgTarget(bundleDir)
	if err != nil {
		return err
	}
	if m.PostgresDB != "" && m.PostgresDB != pg.db {
		return fmt.Errorf("the archive's database is %q and this deployment's is %q: configure it with `--postgres-db %s` first", m.PostgresDB, pg.db, m.PostgresDB)
	}
	if m.PostgresUser != "" && m.PostgresUser != pg.user {
		return fmt.Errorf("the archive's database role is %q and this deployment's is %q: it is not the deployment this archive came from", m.PostgresUser, pg.user)
	}
	if err := sameOpenbaoKeys(o.Archive, m, ops, out); err != nil {
		return err
	}
	running, err := ops.runningServices(composePath)
	if err != nil {
		return err
	}
	var busy []string
	for _, s := range running {
		if s != "postgres" && s != "openbao" {
			busy = append(busy, s)
		}
	}
	if len(busy) > 0 {
		sort.Strings(busy)
		return fmt.Errorf("the services are running (%s) and would write to the database while it is replaced: run `versola down` first (it keeps the data), then this command again, then `versola up`", strings.Join(busy, ", "))
	}

	use := pgUse{purpose: "the restore", interrupted: "the database was not touched", interruptedDone: "the restore had finished, so the database WAS replaced by the archive's", holding: "Interrupted -- a restore that has begun is not stopped half way: it finishes (or fails, and nothing is changed), then Postgres is stopped."}
	return withBundledPostgresFor(use, ops.bundledPg, composePath, out, func(container string, sess pgSession) error {
		n, err := ops.tableCount(container, pg)
		if err != nil {
			return err
		}
		if n > 0 {
			if !o.ReplaceDatabase {
				return fmt.Errorf("this deployment's database %s is not empty (%d tables): the restore replaces everything in it with the archive's. If that is what you want, run the same command with --replace-database (it asks you to type %q)", pg.db, n, restoreWord)
			}
			fmt.Fprintf(out, "\nThe database %s holds %d tables. Restoring replaces all of it with the archive's (Versola %s, taken %s). It cannot be brought back; `versola backup create` first if you want a copy.\n", pg.db, n, m.VersolaVersion, m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
			if err := askInterruptibly(sess, func() error { return confirmTyped(o.In, o.AssumeYes, out) }); err != nil {
				return err
			}
		} else {
			action := fmt.Sprintf("put the database of the archive (Versola %s, taken %s) into this deployment's empty database %s", m.VersolaVersion, m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"), pg.db)
			if err := askInterruptibly(sess, func() error { return confirmVpsFrom(o.In, o.AssumeYes, action) }); err != nil {
				return err
			}
		}
		// A Ctrl-C that came while a question waited, or between the answer
		// and here, ends it: nothing has been touched yet.
		if sess.Heard() {
			return exitcode.With(exitcode.Interrupted, errors.New("interrupted -- the database was not touched"))
		}
		r, err := openBackupMember(o.Archive, backupDumpMember)
		if err != nil {
			return err
		}
		defer r.Close()
		fmt.Fprintf(out, "Restoring the database %s (pg_restore, one transaction)...\n", pg.db)
		if err := ops.pgRestore(container, pg, r); err != nil {
			return err
		}
		fmt.Fprintf(out, "The database is restored.\n\nNext: `versola up` starts the services on it.\n")
		return nil
	})
}

// sameOpenbaoKeys refuses a database that this machine's OpenBao could not
// read: the dump holds values that only the keys of the OpenBao it was taken
// with can open, and a deployment made fresh on this machine has its own. They
// are compared by the unseal key, which does not change; neither value is
// shown. When there is no key to compare (OpenBao was set up by hand) it says
// it could not tell, and goes on.
func sameOpenbaoKeys(archive string, m *BackupManifest, ops restoreOps, out io.Writer) error {
	couldNotTell := func(why string) error {
		fmt.Fprintf(out, "Note: couldn't compare this machine's OpenBao keys with the archive's (%s): if this is not the OpenBao the archive was taken with, its database will not be readable here.\n", why)
		return nil
	}
	if !m.hasMember("openbao/vps-admin.json") {
		return couldNotTell("the archive has no admin credentials: OpenBao was set up by hand")
	}
	baoDir, err := ops.openbaoDir()
	if err != nil {
		return err
	}
	local, err := os.ReadFile(filepath.Join(baoDir, "vps-admin.json"))
	if errors.Is(err, os.ErrNotExist) {
		return couldNotTell("this machine has no admin credentials: OpenBao was set up by hand")
	}
	if err != nil {
		return fmt.Errorf("couldn't read this machine's OpenBao credentials: %w", err)
	}
	b, err := readBackupMember(archive, "openbao/vps-admin.json")
	if err != nil {
		return err
	}
	var a, l openbao.AdminCreds
	if json.Unmarshal(b, &a) != nil || json.Unmarshal(local, &l) != nil || a.UnsealKey == "" || l.UnsealKey == "" {
		return couldNotTell("an admin file has no readable unseal key")
	}
	if a.UnsealKey != l.UnsealKey {
		return errors.New("this machine's OpenBao is not the one the archive was taken with, so the database in the archive would not be readable here (its secrets are encrypted with the archive's keys). Put the archive's OpenBao back first, on a machine with nothing deployed: `versola uninstall --purge` (it asks first and removes this deployment), then `versola backup restore` on the empty machine")
	}
	return nil
}

// askInterruptibly runs a question, and gives up on it when a signal that ends
// the session arrives (the signals are held while Postgres is started, so a
// Ctrl-C would otherwise only be noted and the question go on waiting): the
// customer who pressed it does not then have to answer. Nothing has been touched
// when a question is being asked.
func askInterruptibly(sess pgSession, ask func() error) error {
	if sess.HeardCh == nil {
		return ask()
	}
	res := make(chan error, 1)
	go func() { res <- ask() }()
	select {
	case err := <-res:
		return err
	case <-sess.HeardCh:
		return exitcode.With(exitcode.Interrupted, errors.New("interrupted -- the database was not touched"))
	}
}

// confirmTyped asks for restoreWord to be typed. assumeYes does not answer it:
// a restore over data replaces what nothing can bring back.
func confirmTyped(in io.Reader, assumeYes bool, out io.Writer) error {
	if assumeYes {
		fmt.Fprintln(out, "(--yes does not answer this question.)")
	}
	fmt.Fprintf(out, "Type %q to replace the database: ", restoreWord)
	line, err := ReadLine(in)
	if err != nil && !errors.Is(err, io.EOF) {
		return exitcode.With(exitcode.Aborted, fmt.Errorf("aborted: the database was not touched: %w", err))
	}
	if strings.TrimSpace(line) != restoreWord {
		return exitcode.With(exitcode.Aborted, fmt.Errorf("aborted: the database was not touched (it needs %q typed)", restoreWord))
	}
	return nil
}

// createFreshVolume creates a volume that must not be there: asked again right
// before, because the machine was checked a prompt ago, and `docker volume
// create` succeeds on one that has appeared since, which the undo of a later
// failure would then remove.
func createFreshVolume(ops restoreOps, name string) error {
	if ok, err := ops.exists("volume", name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("the Docker volume %s appeared while this was asking: nothing is replaced, so it is left alone -- run the command again", name)
	}
	if err := ops.createVolume(name); err != nil {
		return fmt.Errorf("couldn't create the volume %s: %w", name, err)
	}
	return nil
}
