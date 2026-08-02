package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/state"
)

var removeVolumes bool

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the locally deployed Versola stack",
	Long: `down stops the containers from the last "versola bootstrap local"
run. By default the Postgres data volume is kept, so a later bootstrap
picks up the same data. Pass --volumes to delete it instead.`,
	RunE: runDown,
}

func init() {
	downCmd.Flags().BoolVar(&removeVolumes, "volumes", false, "also remove the Postgres data volume")
}

func runDown(cmd *cobra.Command, args []string) error {
	composePath, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		fmt.Println("Nothing deployed — there's nothing to stop.")
		return nil
	}

	dockerArgs := []string{"compose", "-f", composePath, "down"}
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
