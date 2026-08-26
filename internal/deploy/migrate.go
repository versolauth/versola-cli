package deploy

import (
	"errors"
	"fmt"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// Migrate runs central/auth/edge's own database migrations against whatever
// Configure most recently prepared, and only that — it doesn't start any
// server. It reads which deployment to migrate from state, the same way Up
// does, rather than taking a target argument: there's only ever one active
// deployment (see state's own package comment), and re-stating its target
// here would make it possible to ask for one and migrate another.
//
// This runs a single one-off container from the `migrate` service compose
// itself generated (see compose.fragment.yml.template /
// compose.fragment.vps.yml.template), which ships all three services'
// Flyway migrations together (see versola/migrate-tool's MigrateTool) and
// applies each against its own schema independently — its own Postgres
// connection, its own Flyway history, in sequence within one process. This
// used to loop over three separate MIGRATE_ONLY=true invocations of
// central/auth/edge's own images (one per service), each needing its own
// container-name collision guard, since a fixed `container_name` on those
// services would otherwise collide with an already-running instance. The
// `migrate` service deliberately has no `container_name` at all (see its
// own comment in the compose templates), so `docker compose run` — which
// always creates a fresh one-off container regardless — has nothing to
// collide with in the first place; there's no leftover-container cleanup
// or concurrent-run guard to reimplement here.
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

	fmt.Println("Migrating central, auth, and edge...")
	// -T: no pseudo-TTY — this never needs interactive input, and whether
	// stdin is a real terminal isn't always detected the same way across
	// Docker Desktop and an SSH session (see develop.md's own -it -> -i fix
	// for the same class of issue).
	//
	// No --no-deps here, unlike the old per-service loop: docker-local's
	// `migrate` service declares `depends_on: postgres: condition:
	// service_healthy` (see compose.fragment.yml.template), and letting
	// Compose start and wait on that dependency itself is the whole reason
	// that's declared there instead of this CLI polling for it by hand, the
	// way the old implementation had to (`docker compose up -d --wait
	// postgres` before the loop). vps's `migrate` service has no
	// dependencies to start in the first place (see
	// compose.fragment.vps.yml.template's own comment — vps's Postgres is a
	// native install this CLI doesn't manage), so this is a no-op wait
	// there.
	if err := docker.Run("compose", "-f", composePath, "run", "--rm", "-T", "migrate"); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
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
