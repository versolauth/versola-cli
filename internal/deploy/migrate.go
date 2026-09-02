package deploy

import (
	"fmt"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// Migrate runs central/auth/edge's own database migrations against
// whatever `st` describes, and only that — it doesn't start any server.
//
// Takes an already-loaded state rather than reading its own: this is
// normally the deployment a caller (cmd/migrate.go) just confirmed with the
// operator, or in bootstrap.go's case, the one Configure just prepared a
// moment ago -- re-reading state here instead of trusting what the caller
// already has in hand would leave a window, between that decision and this
// call, for a concurrent `configure` to finalize a DIFFERENT deployment in
// and have migrations silently apply to it instead (flagged in review; see
// cmd/confirm.go's own comment on the same hazard). `st == nil` means the
// caller found nothing configured (see confirmIfVpsState) -- reported here,
// not by making every caller check it themselves.
//
// Dispatches on whether the generated compose file has a `migrate` service
// (see hasMigrateService) rather than assuming it does: `bootstrap`/
// `configure` accept an arbitrary published versola-tools version (see
// internal/docker's own package comment — "one build of this CLI [deploys]
// several different releases of Versola"), and any release older than the
// one that introduced the `migrate` service generates a compose file
// without it. Assuming the new service always exists would turn every one
// of those older, previously-working versions into a hard "no such service:
// migrate" failure. migrateViaLegacyContainers below is the exact mechanism
// this CLI used before the `migrate` service existed, kept alive
// specifically so those older versions keep working.
func Migrate(st *state.State) error {
	if st == nil {
		return fmt.Errorf("nothing has been configured yet — run `versola configure <target> <version>` first")
	}

	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}

	hasMigrate, err := hasMigrateService(composePath)
	if err != nil {
		return err
	}

	if hasMigrate {
		if err := migrateViaService(composePath); err != nil {
			return err
		}
	} else {
		fmt.Println("This versola-tools release predates the standalone migrate service -- falling back to per-service migration.")
		if err := migrateViaLegacyContainers(composePath, st.Target); err != nil {
			return err
		}
	}

	if err := recordMigrated(st.BundleDir); err != nil {
		return err
	}

	fmt.Println("Migrations complete.")
	return nil
}

// recordMigrated stamps MigratedAt on the active state -- but only if it's
// still the same deployment (same BundleDir) this run actually migrated.
//
// Migrate is handed state once, at the top, before the long-running Docker
// migration itself (anywhere from seconds to however long central/auth/
// edge's own migrations take). A `configure` for a DIFFERENT deployment
// finishing and calling Finalize in that window (see state.Finalize) would
// overwrite state.json and delete this run's own bundle directory out from
// under it. Saving the stale in-memory `st` this function would otherwise
// be handed reverts state.json back to the old, now-deleted bundle and
// version -- clobbering the *other*, now-active deployment's record with
// this one's stale data, not just losing this run's MigratedAt stamp
// (flagged in review).
//
// Every caller that reaches this today (cmd/migrate.go, cmd/bootstrap.go)
// holds state.Lock() for its whole run, which already rules out a
// concurrent `configure` finalizing anything while this function runs --
// see state.Lock's own comment. This check stays anyway, reloading
// immediately before the write and comparing BundleDir, as a second,
// independent guard rather than a reason to trust the lock alone: a lock
// that's ever bypassed (a stale one removed by hand while the real holder
// is still running, say) shouldn't also mean a silent state.json
// corruption is the failure mode. If the comparison still matches, nothing
// has changed underneath this run and it's safe to stamp. If it doesn't,
// something else has taken over regardless -- this run's own migration
// still succeeded against the database it targeted, but there's no longer
// a state record for it to attach to, so skip the save rather than clobber
// the new one.
func recordMigrated(expectedBundleDir string) error {
	fresh, err := state.Load()
	if err != nil {
		return fmt.Errorf("migrations succeeded but couldn't record it: %w", err)
	}
	if fresh.BundleDir != expectedBundleDir {
		fmt.Println("Note: a different deployment was configured while this migration was running -- not recording it against that one.")
		return nil
	}

	now := time.Now().UTC()
	fresh.MigratedAt = &now
	// Stamped explicitly rather than left as whatever Load returned: a
	// pre-state.json deployment (see state.loadLegacy) comes back with
	// SchemaVersion 0, and saving that unchanged would write a state.json --
	// a format that by definition didn't exist then -- still claiming to be
	// version 0. It's this build writing the file, so it's this build's
	// layout.
	fresh.SchemaVersion = state.SchemaVersion
	if err := fresh.Save(); err != nil {
		return fmt.Errorf("migrations succeeded but couldn't record it: %w", err)
	}
	return nil
}

// hasMigrateService reports whether the compose file at composePath declares
// a service named "migrate". `compose config --services` (rather than
// grepping the YAML by hand) is Compose's own resolved service list, so this
// stays correct regardless of formatting differences across versola-tools
// releases.
func hasMigrateService(composePath string) (bool, error) {
	out, err := docker.Output("compose", "-f", composePath, "config", "--services")
	if err != nil {
		return false, fmt.Errorf("couldn't list compose services: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "migrate" {
			return true, nil
		}
	}
	return false, nil
}

// migrateViaService is the current path: one throwaway container, built
// from an image that ships all three services' migrations together (see
// versola-tools' migrate-tool), applies each against its own schema in
// sequence.
func migrateViaService(composePath string) error {
	fmt.Println("Migrating central, auth, and edge...")
	// -T: no pseudo-TTY — this never needs interactive input, and whether
	// stdin is a real terminal isn't always detected the same way across
	// Docker Desktop and an SSH session (see develop.md's own -it -> -i fix
	// for the same class of issue).
	//
	// No --no-deps here: docker-local's `migrate` service declares
	// `depends_on: postgres: condition: service_healthy` (see
	// compose.fragment.yml.template), and letting Compose start and wait on
	// that dependency itself is the whole reason that's declared there
	// instead of this CLI polling for it by hand. vps's `migrate` service
	// has no dependencies to start in the first place (see
	// compose.fragment.vps.yml.template's own comment — vps's Postgres is a
	// native install this CLI doesn't manage), so this is a no-op wait
	// there.
	//
	// No fixed container_name to collide on either (see the `migrate`
	// service's own comment in the compose templates) -- `docker compose
	// run` always creates a fresh one-off container, so unlike
	// migrateViaLegacyContainers below, there's no orphaned-container or
	// concurrent-run guard to reimplement here.
	if err := docker.Run("compose", "-f", composePath, "run", "--rm", "-T", "migrate"); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
	}
	return nil
}

// migrateViaLegacyContainers is the mechanism this CLI used before the
// `migrate` service existed, preserved as a fallback for versola-tools
// releases published before it (see hasMigrateService and Migrate's own
// comment on why this can't just be deleted).
//
// central, auth, and edge each migrate their own schema independently
// (their own Postgres connection, their own Flyway history), and each is
// run with MIGRATE_ONLY=true (see VersolaApp.migrationLayer in the versola
// repo, as published in these older releases) rather than by starting the
// service normally with RUN_MIGRATIONS=true — auth's ordinary startup pulls
// in OAuthConfigurationService, which makes a blocking call to central at
// layer-construction time, so a standalone migrate step must not go through
// that path or it inherits the exact "hangs/OOMs waiting on a service that
// isn't up yet" failure this split exists to avoid.
func migrateViaLegacyContainers(composePath, target string) error {
	if target == "local" {
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
		// actually happened or how to clear it.
		//
		// Only cleaned up if it's NOT currently running, not unconditionally:
		// this name is fixed, not scoped to this process, so an unconditional
		// `docker rm -f` here would just as happily kill a migration another
		// `versola migrate` (a second terminal, a second SSH session) has in
		// flight right now, corrupting that migration rather than this one's
		// leftover. A container that's still running is exactly the case
		// this must NOT touch -- it's either a real concurrent run or, at
		// worst, one that's about to fail on the name conflict below with a
		// clear error, either of which beats silently killing someone else's
		// migration mid-flight.
		if running, err := docker.IsRunning(name); err != nil {
			return fmt.Errorf("couldn't check whether %s is already running: %w", name, err)
		} else if running {
			return fmt.Errorf("%s is already running -- another `versola migrate` may be in progress for this deployment; wait for it to finish", name)
		}
		_ = docker.RunQuiet("rm", "-f", name)

		if err := docker.Run("compose", "-f", composePath, "run", "--rm", "--no-deps", "-T", "--name", name, "-e", "MIGRATE_ONLY=true", service); err != nil {
			return fmt.Errorf("%s migration failed: %w", service, err)
		}
	}

	return nil
}
