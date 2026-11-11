package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var (
	backupOut          string
	backupSkipDatabase bool
)

var backupCmd = &cobra.Command{
	Use:     "backup",
	GroupID: groupOperate,
	Short:   "Back the deployment up into one archive, and list the archives",
}

var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Write one archive with the database, OpenBao's data and keys, and the deployment record",
	Long: `create writes versola-backup-<target>-<version>-<time>.tar.gz (mode 0600)
into --out (default ~/versola-backups) for the deployment this machine
tracks, vps or local. It holds:

  database.dump        pg_dump -Fc of the deployment's database
  openbao-volume.tar   OpenBao's data volume (OpenBao is stopped for a few
                       seconds for a consistent copy, then started and
                       unsealed again; the services keep running)
  openbao/             the AppRole and admin credentials (root token and
                       unseal key) and the Postgres role script, if present
  state/               state.json and the Postgres CA copy, if present
  manifest.json        versions, target, Postgres mode, sizes and SHA-256
                       of every member, and what was left out

The generated bundle is not included (configure writes it again), nor the
ACME certificate and mutual-TLS volumes (reissued on a redeploy).

Whoever has the archive has every secret of the deployment: keep it like a
password, and copy it off the server.`,
	Args: cobra.NoArgs,
	RunE: runBackupCreate,
}

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the archives in --out (default ~/versola-backups), newest first",
	Args:  cobra.NoArgs,
	RunE:  runBackupList,
}

func init() {
	backupCreateCmd.Flags().StringVar(&backupOut, "out", "", "directory to write the archive to (default ~/versola-backups)")
	backupCreateCmd.Flags().BoolVar(&backupSkipDatabase, "skip-database", false, "leave the database dump out (a managed database with snapshots of its own)")
	backupListCmd.Flags().StringVar(&backupOut, "out", "", "directory to list (default ~/versola-backups)")
	backupRestoreCmd.Flags().BoolVar(&backupReplaceDatabase, "replace-database", false, "the deployment's database has data in it and the archive's may replace all of it (asks you to type vps; --yes does not answer that)")
	backupRestoreCmd.Flags().BoolVarP(&backupRestoreYes, "yes", "y", false, "don't ask for the plain confirmation (it does not answer the question that --replace-database brings)")
	backupCmd.AddCommand(backupCreateCmd, backupListCmd, backupRestoreCmd)
}

var (
	backupReplaceDatabase bool
	backupRestoreYes      bool
)

var backupRestoreCmd = &cobra.Command{
	Use:   "restore <archive>",
	Short: "Put an archive of `backup create` back: OpenBao and its keys on an empty machine, then the database",
	Long: `restore reads an archive that "versola backup create" wrote and puts back what
"versola configure" cannot make again. It checks every member against the
checksums in the archive's manifest first, and changes nothing if one is wrong.
Only a vps deployment with its own Postgres is restored (a Postgres of your own
is restored with pg_restore from database.dump). The archive is a file path, or
the name of one in ~/versola-backups.

The same command does what the machine needs, in two steps, because the
database cannot be put back before the deployment's Postgres container exists:

  On a machine with nothing deployed (a new server, or after
  "versola uninstall --purge"): OpenBao's data and keys, and an empty Postgres
  data volume. It prints the "versola configure vps ..." that follows, with the
  flags the archive's record holds. Nothing that is there is replaced.

  After that "configure" (the same Versola version as the archive; the
  services stopped, "versola down" does it): the database, with the
  deployment's own Postgres started for it alone. A database that already has
  data is replaced only with --replace-database and the word vps typed; the
  restore runs in one transaction, so a failure leaves the database as it was.

Then "versola up". Whoever has the archive has every secret of the deployment.
Restore only archives you made or trust: the checksums show damage, not who made it.`,
	Example: `  versola backup restore versola-backup-vps-0.7.6-20261110T090000Z.tar.gz
  versola backup restore ~/versola-backups/versola-backup-vps-0.7.6-20261110T090000Z.tar.gz --replace-database`,
	Args: cobra.ExactArgs(1),
	RunE: runBackupRestore,
}

func runBackupRestore(cmd *cobra.Command, args []string) error {
	// Held for the whole run, like backup create: no configure, up or rollout
	// may run against what is being put back.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return deploy.RestoreBackup(deploy.RestoreOptions{
		Archive:         resolveArchive(args[0]),
		ReplaceDatabase: backupReplaceDatabase,
		AssumeYes:       backupRestoreYes,
		In:              os.Stdin,
	}, os.Stdout)
}

// resolveArchive is arg when a file is there, or, for a bare name, the file of
// that name in the default backup directory (what `backup list` shows).
func resolveArchive(arg string) string {
	if _, err := os.Stat(arg); err == nil {
		return arg
	}
	if !strings.ContainsAny(arg, `/\`) {
		if dir, err := deploy.DefaultBackupDir(); err == nil {
			if p := filepath.Join(dir, arg); fileExists(p) {
				return p
			}
		}
	}
	return arg
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func runBackupCreate(cmd *cobra.Command, args []string) error {
	// Held for the whole run: OpenBao is stopped for the copy, and a
	// configure or rollout must not run against it meanwhile.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return fmt.Errorf("nothing deployed on this machine -- there is nothing to back up")
		}
		return err
	}
	if err := st.RequireCurrent(); err != nil {
		return err
	}
	if backupSkipDatabase && deploy.BackupPostgresMode(st) == deploy.PostgresModeBundled {
		fmt.Println("Warning: --skip-database on a deployment whose database is a container on this server: nothing else backs that database up.")
	}
	_, err = deploy.CreateBackup(st, deploy.BackupOptions{OutDir: backupOut, SkipDatabase: backupSkipDatabase, CLIVersion: version}, os.Stdout)
	return err
}

func runBackupList(cmd *cobra.Command, args []string) error {
	list, dir, err := deploy.ListBackups(backupOut)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Printf("No backups in %s -- `versola backup create` writes one.\n", dir)
		return nil
	}
	fmt.Printf("Backups in %s:\n", dir)
	for _, e := range list {
		name := filepath.Base(e.Path)
		if e.Err != nil {
			fmt.Printf("  %s  (unreadable: %v)\n", name, e.Err)
			continue
		}
		m := e.Manifest
		fmt.Printf("  %s\n    %s, Versola %s (%s), Postgres %s, created %s, %d files", name, deploy.HumanSize(e.Size), m.VersolaVersion, m.Target, m.PostgresMode, m.CreatedAt.Local().Format("2006-01-02 15:04:05"), len(m.Members))
		if len(m.Skipped) > 0 {
			fmt.Printf(", left out:")
			for _, s := range m.Skipped {
				fmt.Printf(" %s", s.Name)
			}
		}
		fmt.Println()
	}
	return nil
}
