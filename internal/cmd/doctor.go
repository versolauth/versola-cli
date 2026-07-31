package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/checks"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that this machine has everything Versola needs",
	Long: `doctor checks this machine for the dependencies Versola needs to run
locally: a reachable Docker daemon, the Docker Compose plugin, and a
free port for the gateway. It only reads state — it never installs or
changes anything.

Run this before "versola bootstrap local <version>" to see ahead of
time what's missing.`,
	RunE: runDoctor,
}

func runDoctor(cmd *cobra.Command, args []string) error {
	results := []checks.Result{
		checks.DockerDaemon(),
		checks.ComposePlugin(),
		checks.PortFree(8080),
	}

	failed := 0
	for _, r := range results {
		fmt.Println(r.String())
		if !r.OK {
			failed++
		}
	}

	fmt.Println()
	if failed == 0 {
		fmt.Println("All checks passed — ready for `versola bootstrap local <version>`.")
		return nil
	}

	fmt.Printf("%d check(s) failed. Fix the issue(s) above before running bootstrap.\n", failed)
	return fmt.Errorf("doctor found %d problem(s)", failed)
}
