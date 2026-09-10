package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var migrateDryRun bool
var migrateService string

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for the configured deployment",
	Long: `migrate runs each service's own database migrations against whatever
"versola configure" most recently prepared. It doesn't start any server.

This is the middle of the three steps "versola bootstrap" runs together
in one go. Run it on its own when a schema change needs to be its own
explicit, reviewable step — e.g. deploying onto a server.

Against a vps deployment this applies migrations to the live database
and asks for confirmation first. Migrations cannot be rolled back
(there are no down-migrations), so take a database backup before
answering yes.

Exits non-zero if any migration fails (e.g. a pipeline step calling this
should fail its build the same way) -- this was already true before
--dry-run/--service existed, since Go's own os.Exit(1) on a returned
error doesn't change with either flag.

--dry-run connects to the real database and reports which migrations
would be applied, without applying any of them or asking for
confirmation first (there's nothing here that could hurt a live
deployment). --service limits either mode to one of "auth", "central",
"edge" instead of all three.`,
	Args: cobra.NoArgs,
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "report which migrations would be applied, without applying any of them")
	migrateCmd.Flags().StringVar(&migrateService, "service", "", `limit to one service ("auth", "central", or "edge") -- default is all three`)
}

func runMigrate(cmd *cobra.Command, args []string) error {
	switch migrateService {
	case "", "auth", "central", "edge":
		// ok
	default:
		return fmt.Errorf(`--service must be "auth", "central", or "edge", got %q`, migrateService)
	}

	// Held from before the confirmation read through deploy.Migrate's own
	// recordMigrated -- see state.Lock's own comment. Without it, a
	// concurrent `configure` could finalize a different deployment between
	// this confirmation and deploy.Migrate acting on it (flagged in
	// review), or between recordMigrated's own read-compare-write. Held
	// for --dry-run too, even though it never writes state itself: without
	// it, a `configure` finishing mid-dry-run could make this report
	// against a compose file a moment away from being deleted (see
	// state.Finalize) -- a stale answer with nothing to show for it isn't
	// as bad as a corrupted write, but it's still not the deployment the
	// operator thinks they just checked.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	var st *state.State
	if migrateDryRun {
		// No confirmation prompt: confirmIfVpsState exists specifically to
		// warn about migrations that can't be rolled back (see this
		// command's own Long text) -- a dry run never applies anything, so
		// that warning would be actively misleading here.
		st, err = state.Load()
		if err != nil {
			if errors.Is(err, state.ErrNotConfigured) {
				return fmt.Errorf("nothing has been configured yet — run `versola configure <target> <version>` first")
			}
			return err
		}
	} else {
		scope := "every service"
		if migrateService != "" {
			scope = migrateService
		}
		st, err = confirmIfVpsState(func(version string) string {
			return fmt.Sprintf("apply Versola %s's migrations (%s) to the live VPS database -- they cannot be rolled back, so back it up first", version, scope)
		})
		if err != nil {
			return err
		}
	}

	return deploy.Migrate(st, deploy.MigrateOptions{DryRun: migrateDryRun, Service: migrateService})
}
