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
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
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
	if target != "local" && target != "vps" {
		return "", fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported today`, target)
	}

	fmt.Println("Checking prerequisites...")
	checksToRun := []checks.Result{
		checks.DockerDaemon(),
		checks.ComposePlugin(),
		checks.DockerMemory(),
		checks.DiskSpace(),
	}
	// Port 2821 is nginx's — local-only, checked here for the same reason
	// as the readiness URLs in up.go (a local-deployment fact still
	// hardcoded in this CLI rather than coming from the bundle
	// versola-tools generates). vps has no nginx service in its compose
	// file at all (see compose.fragment.vps.yml.template's comment) — the
	// VPS's real, native nginx already has that port, and that's expected,
	// not something to fail a prerequisite check over.
	//
	// "versola-nginx" is this deployment's own gateway from a previous
	// run, if there was one — see PortFree's own comment for why that's
	// fine, not a real conflict (the compose file's fixed `name:` means Up
	// updates/restarts it in place rather than clashing with it).
	if target == "local" {
		checksToRun = append(checksToRun, checks.PortFree(2821, "versola-nginx"))
	}
	for _, r := range checksToRun {
		fmt.Println(r.String())
		if !r.OK {
			return "", fmt.Errorf("prerequisite check failed — run `versola doctor` for details")
		}
	}

	fmt.Printf("\nPreparing Versola %s...\n", version)
	dir, err := state.Prepare()
	if err != nil {
		return "", err
	}

	fmt.Println("Generating configuration (versola-tools)...")
	if err := pullAndRunTools(dir, ToolsImage(version), target); err != nil {
		if isManifestUnknown(err) {
			return "", fmt.Errorf(`version %q of Versola doesn't exist (no "versola-tools" image published for it).

Versola releases are tagged WITHOUT a leading "v" (e.g. "0.1.2", not
"v0.1.2" — that "v" prefix is only used for versola-cli's own releases).
Check the available versions at https://github.com/orgs/versolauth/packages`, version)
		}
		return "", fmt.Errorf("versola-tools failed: %w", err)
	}

	// OpenBao has to actually be up before secrets can be resolved against
	// it — Up (which otherwise starts the whole stack, openbao included)
	// hasn't run yet at this point, so it's started here instead. Starting
	// an already-running openbao is a no-op for compose, so this is safe
	// whether or not a previous configure already left it running.
	//
	// The compose file is still named "compose.fragment.yml" at this point
	// (see the rename below) — fine here, this only needs one service out
	// of it, and up.go's own docker volume create call is idempotent, so
	// doing it again there once Up runs doesn't conflict with this one.
	fragmentPath := filepath.Join(dir, "compose.fragment.yml")
	if err := docker.Run("volume", "create", "versola-openbao-file"); err != nil {
		return "", fmt.Errorf("couldn't create the openbao-file volume: %w", err)
	}

	// A previous configure's compose project can still have openbao
	// running under container_name "versola-openbao" — every service in
	// compose.fragment.yml.template has a fixed container_name, and Docker
	// refuses to create a second container under a name that's already
	// taken, even from an unrelated compose project (a fresh bundle
	// directory, per state.Prepare, is a fresh project as far as Compose
	// is concerned). openbao is the one service here it's actually correct
	// to leave alone if that's the situation: unlike postgres/auth/
	// central/edge, it doesn't get reconfigured by this run — the whole
	// point of resolving secrets against it is that it already has the
	// values from before.
	running, err := docker.IsRunning("versola-openbao")
	if err != nil {
		return "", err
	}
	if running {
		fmt.Println("OpenBao is already running.")
	} else {
		fmt.Println("Starting OpenBao...")
		if err := docker.Run("compose", "-f", fragmentPath, "up", "-d", "openbao"); err != nil {
			return "", fmt.Errorf("couldn't start OpenBao: %w", err)
		}
	}
	if err := wait.ForReachable("http://localhost:8200/v1/sys/health", 30*time.Second); err != nil {
		return "", fmt.Errorf("OpenBao never came up: %w", err)
	}

	// versola-tools writes auth.conf/central.conf/edge.conf with each
	// secret field as a ${?VAR} placeholder rather than a literal value
	// (see gen-env.scala's secretField) — this resolves each one against
	// OpenBao (reusing what a previous configure already stored there,
	// generating and storing anything new) and writes the
	// <service>.secrets.env files the compose file's env_file: entries
	// expect to already exist by the time Up runs it.
	//
	// If OpenBao is sealed (every fresh container start comes up sealed,
	// even with its data intact on the persistent volume — see
	// openbao.hcl.template's comment), this fails with whatever error
	// OpenBao's own API returns, which already says "sealed" plainly.
	// Unsealing isn't automated: it needs the unseal key generated when
	// OpenBao was first initialized, which nothing this CLI holds — see
	// develop.md's OpenBao section for the manual `bao operator unseal`
	// step.
	fmt.Println("Resolving secrets (OpenBao)...")
	if err := resolveSecrets(dir, target); err != nil {
		return "", err
	}

	// versola-tools writes the compose file under a "fragment" name and
	// this step renames it, so a half-written or failed generation never
	// leaves behind something the later steps would happily treat as a
	// complete deployment.
	composePath := filepath.Join(dir, "compose.yml")
	if err := os.Rename(filepath.Join(dir, "compose.fragment.yml"), composePath); err != nil {
		return "", fmt.Errorf("couldn't finalize compose file: %w", err)
	}

	// Only now -- everything above has actually succeeded -- does this
	// deployment become the one status/down/uninstall/up see. See
	// state.Finalize's own comment for why that ordering matters: it's
	// what keeps a failed redeploy from costing this machine its record
	// of whatever deployment was still running before this call started.
	if err := state.Finalize(target, version, dir); err != nil {
		return "", fmt.Errorf("couldn't record this deployment: %w", err)
	}

	return dir, nil
}
