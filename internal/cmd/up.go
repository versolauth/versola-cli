package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
)

var upNoBrowser bool

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the deployment configure/migrate prepared",
	Long: `up starts the stack "versola configure" prepared and waits until it's
actually serving traffic, not merely started.

It assumes "versola migrate" already ran — this is not a duplicate
safety net, it's the whole point of splitting them: a schema change
gets its own explicit, reviewable step, not something that happens
silently as a side effect of starting the app. If migrate was skipped,
services fail loudly (schema out of date) instead of quietly migrating.

This is the last of the three steps "versola bootstrap" runs together
in one go.

Against a vps deployment this replaces whatever's currently serving
real traffic, so it asks for confirmation first.`,
	Args: cobra.NoArgs,
	RunE: runUp,
}

func init() {
	upCmd.Flags().BoolVar(&upNoBrowser, "no-browser", false, "don't open the admin console in a browser once it's ready")
}

func runUp(cmd *cobra.Command, args []string) error {
	if err := confirmIfVpsState(func(version string) string {
		return fmt.Sprintf("start Versola %s on the VPS, replacing whatever's currently serving traffic there", version)
	}); err != nil {
		return err
	}
	return deploy.Up(deploy.UpOptions{NoBrowser: upNoBrowser})
}
