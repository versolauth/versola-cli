package deploy

import (
	"errors"
	"fmt"
	"time"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

// UpOptions are the choices Up leaves to the caller. They're a struct
// rather than plain parameters because the list only grows from here (a
// deployment that skips the browser today will also want to skip parts of
// the stack, pick a target, and so on), and a growing list of bare
// booleans at a call site stops being readable very quickly.
type UpOptions struct {
	// NoBrowser suppresses opening the admin console once it's ready.
	NoBrowser bool
}

// Up starts the stack that Configure prepared and waits until it's
// actually serving traffic, not merely started.
//
// It reads what to start from the deployment directory rather than taking
// a version argument: by this point the version is a property of what's
// on disk, and re-stating it here would make it possible to ask for one
// version and get another.
func Up(opts UpOptions) error {
	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return fmt.Errorf("nothing has been configured yet — run `versola bootstrap local <version>` first")
		}
		return err
	}

	composePath, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		// State exists but the compose file doesn't: configure didn't
		// finish. Say so plainly rather than letting docker compose fail
		// on a missing file, which reads like a bug in the CLI.
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}

	// The compose file declares this volume `external: true` (see the
	// comment on it in compose.fragment.yml.template) so OpenBao's storage
	// survives being reconfigured into a fresh bundle directory each run —
	// but `external: true` also means compose refuses to start anything
	// that mounts it until it already exists. `docker volume create` is a
	// no-op if it's already there, so this is safe to run on every `up`,
	// not just the first one.
	if err := docker.Run("volume", "create", "versola-openbao-file"); err != nil {
		return fmt.Errorf("couldn't create the openbao-file volume: %w", err)
	}

	// Postgres and central go up first, on their own — auth/edge's own
	// startup fails fatally if central isn't reachable yet, and confirmed
	// by hand that Docker's restart policy does NOT recover from that
	// failure mode (the process hangs rather than exiting). See the
	// comment on auth's depends_on in
	// versola-tools/compose.fragment.yml.template.
	fmt.Println("\nStarting Postgres and central...")
	if err := docker.Run("compose", "-f", composePath, "up", "-d", "postgres", "central"); err != nil {
		return fmt.Errorf("couldn't start postgres/central: %w", err)
	}

	// These URLs, like the service names above and the port checked in
	// Configure, are still hardcoded here. They're local-deployment facts
	// that by rights belong in the bundle versola-tools generates -- this
	// CLI is meant not to know Versola's topology, so that one build of it
	// can deploy any release (design doc §3.5). Hardcoding them was
	// tolerable while "local" was the only target; a second target with
	// different ports, no Postgres container and no gateway is what will
	// force the bundle to describe this instead.
	fmt.Println("Waiting for central to be ready...")
	if err := wait.ForReady("http://localhost:8091/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("central never became ready: %w", err)
	}

	fmt.Println("Starting auth, edge, and the gateway...")
	// Named explicitly, not a bare "up -d" ("start everything not already
	// running under this project") -- openbao is deliberately left out of
	// that: Configure starts it (or leaves an already-running one alone —
	// see the comment there) under whatever compose project happened to
	// start it, which may not be this bundle's. A bare "up -d" here would
	// see openbao defined in this project's compose file but not part of
	// this project, and try to create a second container under its fixed
	// container_name, which Docker refuses.
	if err := docker.Run("compose", "-f", composePath, "up", "-d", "auth", "edge", "nginx"); err != nil {
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
	fmt.Printf("\nVersola %s is running at http://localhost:2821\n", st.Version)
	fmt.Println("Login: admin / Admin1234!")

	if !opts.NoBrowser {
		// Best-effort only: the deployment having succeeded shouldn't
		// hinge on a desktop environment being around to pop a browser
		// window in (e.g. running over SSH) — failure here is a note, not
		// an error.
		if err := browser.Open(adminURL); err != nil {
			fmt.Printf("(couldn't open a browser automatically: %v — open %s yourself)\n", err, adminURL)
		}
	}
	return nil
}
