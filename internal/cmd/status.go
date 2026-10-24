package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what's currently deployed and its health",
	Long: `status shows the containers from the last "versola bootstrap local"
run and their health, straight from Docker Compose. This command
doesn't know anything about Versola's own services — it just asks
Docker what it knows about the compose project bootstrap created, the
same way "doctor" doesn't know Versola's topology either.

It also lists every mutual-TLS certificate with its notAfter (auth's and
central's listeners, the edge's identity, and the public names' own),
marking the ones a renewal is due for -- see "versola certs renew".`,
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

	// Read the whole state record rather than just the version: a
	// deployment now has a target, and on a server it will matter which
	// one this is. Errors are deliberately ignored -- a compose file
	// exists (checked above), so docker still has something useful to say
	// about it even if the record next to it is missing or unreadable,
	// and refusing to print any status at all over that would be worse
	// than printing it without a header.
	if st, err := state.Load(); err == nil {
		if st.RolloutUnderway() {
			// Version is where the rollout is going, not what runs.
			fmt.Printf("Configured version: %s (target: %s) -- not all of it runs yet:\n", st.Version, st.Target)
			fmt.Println(deploy.RolloutSummary(st))
		} else {
			fmt.Printf("Deployed version: %s (target: %s)\n", st.Version, st.Target)
		}
		if st.ImageRegistry != "" {
			fmt.Printf("Images from: %s (not Versola's own registry)\n", st.ImageRegistry)
		}
		// Printed rather than kept purely internal: with configure/migrate/up
		// available separately, "did the migrate step actually run for what's
		// deployed here" is a real question someone asks when something looks
		// wrong -- and the only place that answers it.
		if st.MigratedAt != nil {
			fmt.Printf("Migrations applied: %s\n", st.MigratedAt.Local().Format("2006-01-02 15:04:05"))
		} else {
			fmt.Println("Migrations applied: never (run `versola migrate`)")
		}
		fmt.Println(deploy.ReplicaSummary(st))
		fmt.Println()
		// On a deployment of this machine's own (not a record of one that is
		// gone: its volume could not be asked): when each certificate runs out.
		if st.Target == "vps" || st.Target == "local" {
			deploy.PrintCertificates(os.Stdout, st, time.Now())
			printCertsWarnings(os.Stdout, st)
			fmt.Println()
		}
	}

	c := exec.Command("docker", state.ComposeArgs(composePath, "ps")...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker compose ps failed: %w", err)
	}
	return nil
}
