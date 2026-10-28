package cmd

import (
	"errors"
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
	downYes       bool
)

// downCompose runs `docker compose` for down; a variable so the tests can
// stand in for Docker.
var downCompose = func(args []string) error {
	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the stack (keeps data and images for next bootstrap)",
	Long: `down stops the containers of the deployment this machine tracks, local
or vps. By default the data is kept: on local the Postgres volume, so a later
bootstrap picks up the same data; on vps the Postgres, OpenBao and TLS
certificate volumes are external and always kept. Pass --volumes to delete
the rest (on local that includes the Postgres volume).

Against a vps deployment the domain stops answering until "versola up", so
down asks for confirmation first (-y/--yes skips the question). local never
asks.`,
	Args: cobra.NoArgs,
	RunE: runDown,
}

func init() {
	downCmd.Flags().BoolVar(&removeVolumes, "volumes", false, "also remove the stack's volumes: the Postgres data on local; on vps the mutual-TLS CA and certificates and the diagnostics volumes (the Postgres, OpenBao and TLS certificate volumes are external and are kept)")
	downCmd.Flags().BoolVarP(&downYes, "yes", "y", false, "don't ask for confirmation (vps)")
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

	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			fmt.Println("Nothing deployed — there's nothing to stop.")
			return nil
		}
		return err
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		fmt.Println("Nothing deployed — there's nothing to stop.")
		return nil
	}

	// Asked only once there is something to stop, and under the lock above,
	// so the answer applies to the deployment that was just described.
	if st.Target == "vps" {
		if err := deploy.ConfirmVpsDeployYes(downYes, deploy.DownAction(st, removeVolumes)); err != nil {
			return err
		}
	}

	dockerArgs := state.ComposeArgs(composePath, "down")
	if removeVolumes {
		dockerArgs = append(dockerArgs, "--volumes")
	}
	if err := downCompose(dockerArgs); err != nil {
		return fmt.Errorf("docker compose down failed: %w", err)
	}
	fmt.Println("Stopped.")
	return nil
}
