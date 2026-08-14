// Package deploy holds the individual steps a deployment is made of.
//
// Deploying used to be one function: check the machine, generate the
// configs, start everything, in a single pass with no way to stop in
// between. That works locally, where the CLI owns the database it just
// created. It doesn't work on a server, where the database already exists
// and is someone else's responsibility, and where "change the schema" has
// to be a decision someone makes on purpose rather than a side effect of
// starting a service.
//
// So the steps live here as separate functions with the deployment
// directory (see internal/state) as the handoff between them: Configure
// writes it, the later steps read it. What the CLI exposes as commands is
// a separate question, answered in internal/cmd.
package deploy

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/state"
)

// Configure prepares a deployment without starting any of it: it checks
// the machine, clears out any previous deployment, and asks the versioned
// versola-tools image to generate this release's configs and compose file
// into ~/.versola/active.
//
// It returns the deployment directory.
//
// Nothing is running when this returns, on purpose. Configure describes a
// deployment; the steps after it act on that description.
func Configure(target, version string) (string, error) {
	if target != "local" {
		return "", fmt.Errorf(`unsupported target %q — only "local" is supported today`, target)
	}

	fmt.Println("Checking prerequisites...")
	for _, r := range []checks.Result{
		checks.DockerDaemon(),
		checks.ComposePlugin(),
		// The port numbers here are local-deployment facts, and they're
		// still hardcoded in this CLI rather than coming from the bundle
		// versola-tools generates. That's a known gap, not a decision:
		// see the note on readiness URLs in up.go.
		checks.PortFree(2821),
		checks.DockerMemory(),
		checks.DiskSpace(),
	} {
		fmt.Println(r.String())
		if !r.OK {
			return "", fmt.Errorf("prerequisite check failed — run `versola doctor` for details")
		}
	}

	fmt.Printf("\nPreparing Versola %s...\n", version)
	dir, err := state.Prepare(target, version)
	if err != nil {
		return "", err
	}

	fmt.Println("Generating configuration (versola-tools)...")
	if err := pullAndRunTools(dir, ToolsImage(version)); err != nil {
		if isManifestUnknown(err) {
			return "", fmt.Errorf(`version %q of Versola doesn't exist (no "versola-tools" image published for it).

Versola releases are tagged WITHOUT a leading "v" (e.g. "0.1.2", not
"v0.1.2" — that "v" prefix is only used for versola-cli's own releases).
Check the available versions at https://github.com/orgs/versolauth/packages`, version)
		}
		return "", fmt.Errorf("versola-tools failed: %w", err)
	}

	// versola-tools writes the compose file under a "fragment" name and
	// this step renames it, so a half-written or failed generation never
	// leaves behind something the later steps would happily treat as a
	// complete deployment.
	composePath := filepath.Join(dir, "compose.yml")
	if err := os.Rename(filepath.Join(dir, "compose.fragment.yml"), composePath); err != nil {
		return "", fmt.Errorf("couldn't finalize compose file: %w", err)
	}

	return dir, nil
}
