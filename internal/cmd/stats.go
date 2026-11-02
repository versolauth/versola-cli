package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// TODO(#72): GroupID once root.go has its command groups.
var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show memory and CPU per container, and whether a rollout's stand-in fits",
	Long: `stats takes one snapshot of what every container of the deployment
uses now (CPU, memory used / limit, PIDs) -- the services, the reverse
proxy, OpenBao and a bundled Postgres. For a live view, use
"docker stats" with the names it prints.

On vps it also answers "can I start a rolling upgrade with a stand-in
replica?" in the rollout's own numbers: the free memory (MemAvailable)
against what the rollout's pre-flight requires, and the Postgres
connections a stand-in would open. It has to run on the vps itself to
read the host's memory. The Postgres check only runs when its psql image
is already on the machine; stats never pulls it.

With --json it prints one JSON object on stdout instead (fields are
documented in the README and only ever added).`,
	Args: cobra.NoArgs,
	RunE: runStats,
}

var statsJSON bool

func init() {
	statsCmd.Flags().BoolVar(&statsJSON, "json", false, "print one JSON object instead of the table")
}

// collectStats is deploy.CollectStats, replaceable in tests.
var collectStats = deploy.CollectStats

func runStats(cmd *cobra.Command, args []string) error {
	return writeStats(os.Stdout, os.Stderr, statsJSON)
}

func writeStats(stdout, stderr io.Writer, asJSON bool) error {
	_, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("nothing deployed on this machine yet -- run `versola bootstrap <local|vps> <version>` first")
	}
	st, err := state.Load()
	if err != nil {
		return err
	}
	// Progress (the Postgres check's "Checking..." line) goes where it does
	// not mix with a JSON object.
	progress := stdout
	if asJSON {
		progress = stderr
	}
	r, dockerErr := collectStats(st, progress)
	if asJSON {
		if err := encodeJSON(stdout, r); err != nil {
			return err
		}
	} else {
		deploy.WriteStats(stdout, r)
	}
	return dockerErr
}
