package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var adminResetYes bool

var adminCmd = &cobra.Command{
	Use:     "admin",
	GroupID: groupOperate,
	Short:   "Manage the bootstrap admin of the deployment",
}

var adminResetPasswordCmd = &cobra.Command{
	Use:   "reset-password",
	Short: "Issue a new first-login link for the bootstrap admin",
	Long: `reset-password gives the bootstrap admin a new one-time first-login link, for
when the first one was lost, opened by something else, or has expired. It
prints the link; open it in a browser and set the admin's permanent password.

It restarts auth twice (first with a reset requested, then without): a restart
alone would change nothing, because auth keeps a still-valid temporary
password and reads the reset request from its container's environment. Only
the first auth replica is restarted; sign-in is interrupted for a moment when
it is the only one. The link is shown here once and stored nowhere.

It refuses once the admin has a permanent password: a forgotten permanent
password is recovered through the product's own account recovery.

On a release without first-login links it changes nothing and says where the
temporary password is kept (the path, never the value). On local the admin
login is fixed and is only printed.`,
	Example: `  versola admin reset-password
  versola admin reset-password --yes`,
	Args: cobra.NoArgs,
	RunE: runAdminResetPassword,
}

func init() {
	adminResetPasswordCmd.Flags().BoolVarP(&adminResetYes, "yes", "y", false, "don't ask for confirmation (vps)")
	adminCmd.AddCommand(adminResetPasswordCmd)
}

func runAdminResetPassword(cmd *cobra.Command, args []string) error {
	// Held for the same reason as `up`: a concurrent `configure` finalizing
	// would delete the bundle this command runs docker compose against.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := confirmIfVpsState(adminResetYes, func(version string) string {
		return fmt.Sprintf("restart auth of Versola %s on the VPS (twice, to issue a new first-login link for the admin; sign-in is interrupted for a moment)", version)
	})
	if err != nil {
		return err
	}
	return deploy.AdminResetPassword(st, os.Stdout)
}
