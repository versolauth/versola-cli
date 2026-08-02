package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/state"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what's currently deployed and its health",
	Long: `status shows the containers from the last "versola bootstrap local"
run and their health, straight from Docker Compose. This command
doesn't know anything about Versola's own services — it just asks
Docker what it knows about the compose project bootstrap created, the
same way "doctor" doesn't know Versola's topology either.`,
	RunE: runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	composePath, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		fmt.Println("Nothing deployed yet — run `versola bootstrap local <version>` first.")
		return nil
	}

	if v := state.Version(); v != "" {
		fmt.Printf("Deployed version: %s\n\n", v)
	}

	c := exec.Command("docker", "compose", "-f", composePath, "ps")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker compose ps failed: %w", err)
	}
	return nil
}
