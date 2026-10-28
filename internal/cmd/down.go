package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var (
	removeVolumes bool
	downForce     bool
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the stack (keeps data and images for next bootstrap)",
	Long: `down stops the containers from the last "versola bootstrap local"
run. By default the Postgres data volume is kept, so a later bootstrap
picks up the same data. Pass --volumes to delete it instead.`,
	RunE: runDown,
}

func init() {
	downCmd.Flags().BoolVar(&removeVolumes, "volumes", false, "also remove the Postgres data volume (local only: on vps the volume of the deployment's own Postgres is external and is kept)")
	downCmd.Flags().BoolVar(&downForce, "force", false, "stop the stack even while a rolling upgrade is under way")
}

func runDown(cmd *cobra.Command, args []string) error {
	// Held for this whole run -- see state.Lock's own comment, specifically
	// its note on down/uninstall: without it, a concurrent `configure`
	// could finalize a different bundle directory while this is reading
	// composePath below, or while `docker compose down` is still running
	// against it (flagged in review).
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// While a rolling upgrade is under way, stopping everything is rarely
	// what is meant (it takes a vps deployment down mid-upgrade), and `up`
	// can't start it again afterwards -- only `rollout` can.
	if st, err := state.Load(); err == nil {
		if err := deploy.DownBlocked(st, downForce); err != nil {
			return err
		}
	}

	composePath, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		fmt.Println("Nothing deployed — there's nothing to stop.")
		return nil
	}

	dockerArgs := state.ComposeArgs(composePath, "down")
	if removeVolumes {
		dockerArgs = append(dockerArgs, "--volumes")
	}

	c := exec.Command("docker", dockerArgs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker compose down failed: %w", err)
	}
	fmt.Println("Stopped.")
	return nil
}
