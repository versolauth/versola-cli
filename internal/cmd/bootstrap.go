package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

var noBrowser bool

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

func init() {
	bootstrapCmd.Flags().BoolVar(&noBrowser, "no-browser", false, "don't open the admin console in a browser once it's ready")
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
		checks.PortFree(2821),
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
	if err := pullAndRunTools(dir, toolsImage); err != nil {
		if isManifestUnknown(err) {
			return fmt.Errorf(`version %q of Versola doesn't exist (no "versola-tools" image published for it).

Versola releases are tagged WITHOUT a leading "v" (e.g. "0.1.2", not
"v0.1.2" — that "v" prefix is only used for versola-cli's own releases).
Check the available versions at https://github.com/orgs/versolauth/packages`, version)
		}
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

	adminURL := "http://localhost:2821/central/admin/"
	fmt.Printf("\nVersola %s is running at http://localhost:2821\n", version)
	fmt.Println("Login: admin / Admin1234!")

	if !noBrowser {
		// Best-effort only: bootstrap having succeeded shouldn't hinge on a
		// desktop environment being around to pop a browser window in
		// (e.g. running over SSH) — failure here is a note, not an error.
		if err := browser.Open(adminURL); err != nil {
			fmt.Printf("(couldn't open a browser automatically: %v — open %s yourself)\n", err, adminURL)
		}
	}
	return nil
}

// dockerCmd builds a docker exec.Cmd with stdout already wired to the
// user's terminal. Stderr is left for the caller to set, since runDocker
// and pullAndRunTools each need it to go somewhere different.
func dockerCmd(args ...string) *exec.Cmd {
	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	return c
}

func runDocker(args ...string) error {
	c := dockerCmd(args...)
	c.Stderr = os.Stderr
	return c.Run()
}

// manifestUnknownErr wraps a runDocker failure whose stderr contained
// Docker's "manifest unknown" text -- i.e. the image (or this tag of it)
// was never published, as opposed to some other failure (network, daemon
// down, etc.) that happens to also come back as a non-zero exit.
type manifestUnknownErr struct{ inner error }

func (e *manifestUnknownErr) Error() string { return e.inner.Error() }
func (e *manifestUnknownErr) Unwrap() error { return e.inner }

func isManifestUnknown(err error) bool {
	var m *manifestUnknownErr
	return errors.As(err, &m)
}

// pullAndRunTools runs the versola-tools image the same way runDocker does
// (stdout/stderr still streamed live to the user), but also tees stderr
// into a buffer so it can be inspected afterward -- specifically to
// recognize Docker's "manifest unknown" text and turn it into a clearer
// error in runBootstrap, without changing behavior for every other
// runDocker call site (compose up/down, uninstall's rmi, etc.) that has no
// need for this.
func pullAndRunTools(dir, image string) error {
	// "manifest unknown" (when it happens at all) comes from Docker
	// failing to resolve the image before any pull output follows, so a
	// few KB is more than enough to catch it -- capped rather than a plain
	// bytes.Buffer so a normal, successful pull's progress output (which
	// can run to tens of KB across an image's layers) can't grow this
	// unbounded in memory. os.Stderr (the other leg of the MultiWriter
	// below) still gets every byte, same as a real terminal would.
	stderrBuf := newCappedBuffer(8 * 1024)
	// --platform linux/amd64: versola-tools, like the rest of the stack
	// (see compose.fragment.yml.template's platform pins on central/auth/
	// edge/nginx in the versola repo), is only published for amd64. Without
	// this, Docker has to guess whether to emulate on non-amd64 hosts (e.g.
	// Apple Silicon Macs) -- it doesn't always guess right, and the failure
	// mode when it doesn't is a raw "no matching manifest" error instead of
	// a working, if slower, emulated container. Being explicit here makes
	// bootstrap work the same way on arm64 as on amd64, without requiring
	// DOCKER_DEFAULT_PLATFORM to be set in the environment first.
	c := dockerCmd("run", "--rm", "--platform", "linux/amd64", "-v", dir+":/out", image)
	c.Stderr = io.MultiWriter(os.Stderr, stderrBuf)

	if err := c.Run(); err != nil {
		if strings.Contains(stderrBuf.String(), "manifest unknown") {
			return &manifestUnknownErr{inner: err}
		}
		return err
	}
	return nil
}

// cappedBuffer is a bytes.Buffer that silently stops accumulating past a
// fixed size. It still reports every byte as written (never a short
// write) so it's safe to use as one leg of an io.MultiWriter -- the other
// leg (os.Stderr here) keeps receiving the full, uncapped output.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		c.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
