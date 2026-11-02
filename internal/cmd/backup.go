package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	backupCmd.AddCommand(backupCreateCmd, backupListCmd)
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
