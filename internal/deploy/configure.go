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
// authURL and postgresHost are only meaningful (and required, see
// cmd/bootstrap.go's own check) for target == "vps" -- both describe
// facts about whichever real server this is deploying to that this
// package can't default sensibly (a different domain, possibly a
// different Postgres setup entirely). Ignored for "local".
func Configure(target, version, authURL, postgresHost string) (string, error) {
	if target != "local" && target != "vps" {
		return "", fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported today`, target)
	}
	if target == "vps" && authURL == "" {
		// Belt and suspenders -- cmd/bootstrap.go already checks this
		// before calling Configure, but Configure is this package's own
		// public entry point and shouldn't rely on every future caller
		// remembering to check first.
		return "", fmt.Errorf("authURL is required for vps deployments")
	}
	if target == "vps" && postgresHost == "" {
		return "", fmt.Errorf("postgresHost is required for vps deployments")
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
	if err := pullAndRunTools(dir, ToolsImage(version), target, authURL, postgresHost); err != nil {
		if isManifestUnknown(err) {
			return "", fmt.Errorf(`version %q of Versola doesn't exist (no "versola-tools" image published for it).

Versola releases are tagged WITHOUT a leading "v" (e.g. "0.1.2", not
"v0.1.2" — that "v" prefix is only used for versola-cli's own releases).
Check the available versions at https://github.com/orgs/versolauth/packages`, version)
		}
		return "", fmt.Errorf("versola-tools failed: %w", err)
	}

	// The candidates versola-tools just wrote (*.generated-secrets.env)
	// can become real, live secret values the first time resolveSecrets
	// below runs against an empty OpenBao path -- tightened here,
	// immediately after they're written, rather than only once each is
	// successfully resolved (resolveServiceSecrets removes each file it
	// finishes with, but that's no help for whichever ones it never gets
	// to). That gap matters more than it sounds like it would: the
	// documented first-ever `configure vps` is EXPECTED to fail right
	// after this point, before OpenBao is set up at all (see develop.md's
	// OpenBao section) -- these files sit in the bundle directory for as
	// long as the one-time setup takes, on a shared machine where "just a
	// Windows dev box" isn't the threat model.
	restrictGeneratedSecretsPerms(dir)

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
	if err := docker.Run("volume", "create", OpenbaoVolumeName(target)); err != nil {
		return "", fmt.Errorf("couldn't create the openbao-file volume: %w", err)
	}

	// A previous configure's compose project can still have openbao
	// running under its container_name (see OpenbaoContainerName) — every
	// service in compose.fragment.yml.template has a fixed container_name,
	// and Docker refuses to create a second container under a name that's
	// already taken, even from an unrelated compose project (a fresh
	// bundle directory, per state.Prepare, is a fresh project as far as
	// Compose is concerned). openbao is the one service here it's
	// actually correct to leave alone if that's the situation: unlike
	// postgres/auth/central/edge, it doesn't get reconfigured by this run
	// — the whole point of resolving secrets against it is that it
	// already has the values from before. That's only true because the
	// container name is target-specific now, too -- otherwise this could
	// find the OTHER target's leftover container still running and
	// "leave it alone" straight into resolving secrets against the wrong
	// target entirely.
	running, err := docker.IsRunning(OpenbaoContainerName(target))
	if err != nil {
		return "", err
	}
	if running {
		fmt.Println("OpenBao is already running.")
	} else {
		// Both targets' compose fragments bind OpenBao to the same host
		// port (8200) -- separate container names and volumes (see
		// OpenbaoContainerName/OpenbaoVolumeName) don't change that, since
		// that's a fact about the host's own network, not about Compose
		// projects. If the OTHER target's OpenBao is still running from an
		// earlier configure, starting this one would fail on Docker's own
		// "port is already allocated" -- checked here instead, so the
		// error says what to actually do about it rather than requiring
		// that to be reverse-engineered from a raw Docker failure
		// (flagged in review on versolauth/versola-cli#7, alongside the
		// container-name mixup this same check also guards against).
		other := "vps"
		if target == "vps" {
			other = "local"
		}
		otherRunning, err := docker.IsRunning(OpenbaoContainerName(other))
		if err != nil {
			return "", err
		}
		if otherRunning {
			return "", fmt.Errorf("%s's OpenBao (%s) is still running and already holds port 8200 on this machine -- stop it first: docker rm -f %s", other, OpenbaoContainerName(other), OpenbaoContainerName(other))
		}
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
	if err := state.Finalize(target, version, dir, authURL); err != nil {
		return "", fmt.Errorf("couldn't record this deployment: %w", err)
	}

	return dir, nil
}
