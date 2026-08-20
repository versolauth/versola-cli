package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for the configured deployment",
	Long: `migrate runs each service's own database migrations against whatever
"versola configure" most recently prepared. It doesn't start any server.

This is the middle of the three steps "versola bootstrap" runs together
in one go. Run it on its own when a schema change needs to be its own
explicit, reviewable step — e.g. deploying onto a server.

Against a vps deployment this applies migrations to the live database
and asks for confirmation first. Migrations cannot be rolled back
(there are no down-migrations), so take a database backup before
answering yes.`,
	Args: cobra.NoArgs,
	RunE: runMigrate,
}

func runMigrate(cmd *cobra.Command, args []string) error {
	if err := confirmIfVpsState(func(version string) string {
		return fmt.Sprintf("apply Versola %s's database migrations to the live VPS database -- they cannot be rolled back, so back it up first", version)
	}); err != nil {
		return err
	}
	return deploy.Migrate()
}
