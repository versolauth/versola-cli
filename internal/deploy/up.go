package deploy

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Up starts the stack described by `st` and waits until it's actually
// serving traffic, not merely started.
//
// Takes an already-loaded state rather than reading its own -- same
// reasoning as deploy.Migrate's own comment: this is normally the
// deployment a caller (cmd/up.go) just confirmed with the operator, or the
// one bootstrap.go's own Configure/Migrate just acted on, and re-reading
// state here instead would leave a window for a concurrent `configure` to
// finalize a different deployment into the gap (flagged in review). `st ==
// nil` means the caller found nothing configured (see confirmIfVpsState).
func Up(opts UpOptions, st *state.State) error {
	if st == nil {
		return fmt.Errorf("nothing has been configured yet — run `versola bootstrap local <version>` first")
	}

	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		// State exists but the compose file doesn't: configure didn't
		// finish. Say so plainly rather than letting docker compose fail
		// on a missing file, which reads like a bug in the CLI.
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}
	dir := filepath.Dir(composePath)
	isVps := st.Target == "vps"

	// A warning, not a hard failure: the services themselves are the real
	// check (they validate their schema at startup now rather than silently
	// running against whatever's there -- see RUN_MIGRATIONS in
	// compose.fragment.yml.template), and this record can legitimately be
	// absent for a deployment made before it existed. But when it IS absent,
	// saying so here turns "central won't start and I don't know why" into
	// one line read before the failure rather than after it.
	if st.MigratedAt == nil {
		fmt.Println("\nNote: no migration has been recorded for this deployment — if `versola migrate` hasn't run, the services will refuse to start against an out-of-date schema.")
	}

	// The vps confirmation doesn't live here: Configure's own Finalize
	// (which overwrites state.json to point at the new deployment and
	// deletes the previous bundle -- see its comment) runs to completion
	// before Up is ever called, so by this point that's already
	// irreversible -- declining here, or Up failing partway, couldn't
	// actually get anyone back to the old deployment record even though
	// the old containers might still be the ones really serving traffic
	// (flagged in review on versolauth/versola-cli#7). cmd/bootstrap.go
	// asks once, before Configure runs at all; cmd/up.go asks again on its
	// own when Up is run standalone (see ConfirmVpsDeploy's own comment on
	// why one fixed prompt can't cover both callers).

	// The compose file declares this volume `external: true` (see the
	// comment on it in compose.fragment.yml.template) so OpenBao's storage
	// survives being reconfigured into a fresh bundle directory each run —
	// but `external: true` also means compose refuses to start anything
	// that mounts it until it already exists. `docker volume create` is a
	// no-op if it's already there, so this is safe to run on every `up`,
	// not just the first one.
	if err := docker.Run("volume", "create", OpenbaoVolumeName(st.Target)); err != nil {
		return fmt.Errorf("couldn't create the openbao-file volume: %w", err)
	}

	// Postgres and central go up first, on their own — auth/edge's own
	// startup fails fatally if central isn't reachable yet, and confirmed
	// by hand that Docker's restart policy does NOT recover from that
	// failure mode (the process hangs rather than exiting). See the
	// comment on auth's depends_on in
	// versola-tools/compose.fragment.yml.template. vps has no postgres
	// service at all — its Postgres is native on the host (see
	// compose.fragment.vps.yml.template's comment), not something this
	// compose file starts.
	if isVps {
		fmt.Println("\nStarting central...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "central"); err != nil {
			return fmt.Errorf("couldn't start central: %w", err)
		}
	} else {
		fmt.Println("\nStarting Postgres and central...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "postgres", "central"); err != nil {
			return fmt.Errorf("couldn't start postgres/central: %w", err)
		}
	}

	// These URLs, like the service names above and the port checked in
	// Configure, are still hardcoded here. They're deployment facts that
	// by rights belong in the bundle versola-tools generates -- this CLI
	// is meant not to know Versola's topology, so that one build of it can
	// deploy any release (design doc §3.5). Hardcoding them was tolerable
	// while "local" was the only target; vps happens to use the exact
	// same ports (see deploy.md's table), which is the only reason this
	// hasn't forced the issue yet.
	fmt.Println("Waiting for central to be ready...")
	if err := wait.ForReady("http://localhost:8091/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("central never became ready: %w", err)
	}

	// Named explicitly, not a bare "up -d" ("start everything not already
	// running under this project") -- openbao is deliberately left out of
	// that: Configure starts it (or leaves an already-running one alone —
	// see the comment there) under whatever compose project happened to
	// start it, which may not be this bundle's. A bare "up -d" here would
	// see openbao defined in this project's compose file but not part of
	// this project, and try to create a second container under its fixed
	// container_name, which Docker refuses. vps has no nginx/gateway
	// service — its public nginx is native on the VPS, deployed by a
	// separate pipeline (see compose.fragment.vps.yml.template's comment).
	if isVps {
		fmt.Println("Starting auth and edge...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "auth", "edge"); err != nil {
			return fmt.Errorf("couldn't start auth/edge: %w", err)
		}
	} else {
		fmt.Println("Starting auth, edge, and the gateway...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "auth", "edge", "nginx"); err != nil {
			return fmt.Errorf("couldn't start the rest of the stack: %w", err)
		}
	}

	fmt.Println("Waiting for auth and edge to be ready...")
	if err := wait.ForReady("http://localhost:8081/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("auth never became ready: %w", err)
	}
	if err := wait.ForReady("http://localhost:8096/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("edge never became ready: %w", err)
	}

	if isVps {
		// st.AuthURL is whatever --auth-url Configure was given (see
		// state.Finalize) -- not hardcoded here anymore, since that broke
		// for any deployment other than the one original VPS this used to
		// assume (flagged in review on versolauth/versola-cli#7). Older
		// deployments recorded before AuthURL existed will have it empty;
		// falling back to the compose-generated auth.conf's own issuer
		// would need reading and parsing that file just for a print
		// statement, so this just says so plainly instead.
		authURL := st.AuthURL
		if authURL == "" {
			authURL = "(unknown -- reconfigure to record it)"
		}
		fmt.Printf("\nVersola %s is running at %s\n", st.Version, authURL)
		// vps doesn't use a fixed literal password the way local's
		// "Admin1234!" is — it's a real, standing admin credential Configure
		// resolved against OpenBao (see gen-env.scala's
		// bootstrapPasswordDefault), not a throwaway dev one. Deliberately
		// NOT echoed to stdout here the way local's fixed password is below:
		// unlike local, this runs on every redeploy of the same target, not
		// just the first, and a real production credential printed to a
		// terminal on every run (often over SSH, sometimes logged or
		// recorded) is needless repeated exposure for something that's
		// already sitting in auth.secrets.env for whoever's actually
		// authorized to read it.
		fmt.Printf("Login: admin / (see ADMIN_BOOTSTRAP_PASSWORD in %s)\n", filepath.Join(dir, "auth.secrets.env"))
		// No browser.Open here, unlike local below: this CLI runs on the
		// VPS itself (typically over SSH), not on the operator's own
		// desktop -- popping a browser window on the server wouldn't
		// reach anyone.
		return nil
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

// ConfirmVpsDeploy asks for an explicit go-ahead before anything touches
// a real VPS deployment. action is the already-formatted sentence
// describing what this particular call is actually about to do --
// configure/migrate/up are separate commands now (see cmd/configure.go,
// cmd/migrate.go, cmd/up.go), each with a different real effect on a
// live deployment, so one fixed message here would be wrong for at least
// two of the three callers. bootstrap.go calls this once, before
// Configure runs at all (see its own comment on why not later), covering
// the whole chain in one prompt; cmd/migrate.go and cmd/up.go each call
// it again themselves when run standalone, since nothing then guarantees
// the same process just showed one moments ago.
func ConfirmVpsDeploy(action string) error {
	fmt.Printf("\nThis will %s.\n", action)
	fmt.Print("Continue? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}
