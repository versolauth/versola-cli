package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap <target> <version>",
	Short: "Deploy a specific version of Versola",
	Long: `bootstrap deploys a specific released version of Versola.

Currently supported:

  versola bootstrap local 0.1.1

This CLI doesn't know Versola's own topology (ports, service names,
config schema) — that lives entirely in the versioned "versola-tools"
image, which this command pulls and runs to generate everything the
compose stack needs. See the project design doc, section 3.5.`,
	Args: cobra.ExactArgs(2),
	RunE: runBootstrap,
}

func runBootstrap(cmd *cobra.Command, args []string) error {
	target, version := args[0], args[1]
	if target != "local" {
		return fmt.Errorf(`unsupported target %q — only "local" is supported today`, target)
	}

	fmt.Println("Checking prerequisites...")
	for _, r := range []checks.Result{
		checks.DockerDaemon(),
		checks.ComposePlugin(),
		checks.PortFree(8080),
		checks.DockerMemory(),
		checks.DiskSpace(),
	} {
		fmt.Println(r.String())
		if !r.OK {
			return fmt.Errorf("prerequisite check failed — run `versola doctor` for details")
		}
	}

	fmt.Printf("\nPreparing Versola %s...\n", version)
	dir, err := state.Prepare(version)
	if err != nil {
		return err
	}

	fmt.Println("Generating configuration (versola-tools)...")
	toolsImage := fmt.Sprintf("ghcr.io/versolauth/versola-tools:%s", version)
	if err := runDocker("run", "--rm", "-v", dir+":/out", toolsImage); err != nil {
		return fmt.Errorf("versola-tools failed: %w", err)
	}

	composePath := filepath.Join(dir, "compose.yml")
	if err := os.Rename(filepath.Join(dir, "compose.fragment.yml"), composePath); err != nil {
		return fmt.Errorf("couldn't finalize compose file: %w", err)
	}

	// Postgres and central go up first, on their own — auth/edge's own
	// startup fails fatally if central isn't reachable yet, and confirmed
	// by hand that Docker's restart policy does NOT recover from that
	// failure mode (the process hangs rather than exiting). See the
	// comment on auth's depends_on in
	// versola-tools/compose.fragment.yml.template.
	fmt.Println("\nStarting Postgres and central...")
	if err := runDocker("compose", "-f", composePath, "up", "-d", "postgres", "central"); err != nil {
		return fmt.Errorf("couldn't start postgres/central: %w", err)
	}

	fmt.Println("Waiting for central to be ready...")
	if err := wait.ForReady("http://localhost:8091/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("central never became ready: %w", err)
	}

	fmt.Println("Starting auth, edge, and the gateway...")
	if err := runDocker("compose", "-f", composePath, "up", "-d"); err != nil {
		return fmt.Errorf("couldn't start the rest of the stack: %w", err)
	}

	fmt.Println("Waiting for auth and edge to be ready...")
	if err := wait.ForReady("http://localhost:8081/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("auth never became ready: %w", err)
	}
	if err := wait.ForReady("http://localhost:8096/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("edge never became ready: %w", err)
	}

	fmt.Printf("\nVersola %s is running at http://localhost:8080\n", version)
	fmt.Println("Login: admin / Admin1234!")
	return nil
}

func runDocker(args ...string) error {
	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
