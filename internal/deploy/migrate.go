package deploy

import (
	"errors"
	"fmt"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// Migrate runs each service's own database migrations against whatever
// Configure most recently prepared, and only that — it doesn't start any
// server. It reads which deployment to migrate from state, the same way
// Up does, rather than taking a target argument: there's only ever one
// active deployment (see state's own package comment), and re-stating
// its target here would make it possible to ask for one and migrate
// another.
//
// central, auth, and edge each migrate their own schema independently
// (their own Postgres connection, their own Flyway history), and each is
// run with MIGRATE_ONLY=true (see VersolaApp.migrationLayer in the
// versola repo) rather than by starting the service normally with
// RUN_MIGRATIONS=true — auth's ordinary startup pulls in
// OAuthConfigurationService, which makes a blocking call to central at
// layer-construction time, so a standalone migrate step must not go
// through that path or it inherits the exact "hangs/OOMs waiting on a
// service that isn't up yet" failure this split exists to avoid.
func Migrate() error {
	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return fmt.Errorf("nothing has been configured yet — run `versola configure <target> <version>` first")
		}
		return err
	}

	composePath, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}

	if st.Target == "local" {
		// docker-local's Postgres is this compose project's own container
		// (vps's is native — see compose.fragment.vps.yml.template's
		// comment); central, auth, AND edge connect to it directly for
		// their own schema, not just central, so it has to be up before any
		// of them can migrate. It has no published port (see the comment on
		// its own compose service), so this can't be waited for from the
		// host the way central/auth/edge's own readiness is — "--wait"
		// instead asks Compose itself to block on the healthcheck already
		// defined for it.
		fmt.Println("Starting Postgres...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "--wait", "postgres"); err != nil {
			return fmt.Errorf("postgres never became healthy: %w", err)
		}
	}

	for _, service := range []string{"central", "auth", "edge"} {
		fmt.Printf("Migrating %s...\n", service)
		// --no-deps: without it, compose would start central as a full
		// server as a side effect of migrating auth/edge (both declare
		// `depends_on: central` in the compose templates) — exactly the
		// coupling this whole standalone step exists to avoid (see the
		// comment above).
		//
		// --name, not the service's own fixed container_name that `run`
		// would otherwise try to reuse: during a redeploy, the PREVIOUS
		// version's container is still up under that exact name at the
		// point migrate runs (Up hasn't touched it yet — configure/migrate/
		// up run in that order), and Docker refuses to create a second
		// container under a name already in use. A dedicated name for this
		// one-off container sidesteps that entirely.
		//
		// -T: no pseudo-TTY — this never needs interactive input, and
		// whether stdin is a real terminal isn't always detected the same
		// way across Docker Desktop and an SSH session (see develop.md's
		// own -it -> -i fix for the same class of issue).
		name := fmt.Sprintf("versola-%s-migrate", service)

		// --rm only removes the container when the run finishes normally. A
		// migrate interrupted partway (Ctrl+C, a dropped SSH session, the
		// Docker daemon restarting) leaves it behind, and the next migrate
		// would then fail on a name conflict that says nothing about what
		// actually happened or how to clear it. Removing it up front is
		// safe: this name belongs to nothing but this one-shot step, and
		// there is never a second migrate running concurrently against the
		// same deployment. Errors are ignored on purpose -- the overwhelmingly
		// common case is "no such container", which is not a problem, and a
		// removal that genuinely fails will resurface as a much clearer error
		// from the run below.
		_ = docker.RunQuiet("rm", "-f", name)

		if err := docker.Run("compose", "-f", composePath, "run", "--rm", "--no-deps", "-T", "--name", name, "-e", "MIGRATE_ONLY=true", service); err != nil {
			return fmt.Errorf("%s migration failed: %w", service, err)
		}
	}

	now := time.Now().UTC()
	st.MigratedAt = &now
	// Stamped explicitly rather than left as whatever Load returned: a
	// pre-state.json deployment (see state.loadLegacy) comes back with
	// SchemaVersion 0, and saving that unchanged would write a state.json --
	// a format that by definition didn't exist then -- still claiming to be
	// version 0. It's this build writing the file, so it's this build's
	// layout.
	st.SchemaVersion = state.SchemaVersion
	if err := st.Save(); err != nil {
		return fmt.Errorf("migrations succeeded but couldn't record it: %w", err)
	}

	fmt.Println("Migrations complete.")
	return nil
}
