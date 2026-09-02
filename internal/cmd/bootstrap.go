package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var noBrowser bool
var authURL string
var postgresHost string

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap <target> <version>",
	Short: "Deploy a specific version of Versola",
	Long: `bootstrap deploys a specific released version of Versola.

Currently supported:

  versola bootstrap local 0.1.1
  versola bootstrap vps 0.1.1 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432

vps deploys to a real server — it requires OpenBao credentials to
already be stored for it first (see "versola secrets login vps"), and
asks for confirmation before it touches the live database.

--auth-url and --postgres-host are both required for vps: the public
domain this deployment will actually be reachable at, and the host:port
its Postgres is reachable on. Both are specific to whoever's deploying
(a different server, possibly a different Postgres setup entirely) —
there's no sensible shared default across different servers/clients.
local doesn't need either (docker-local's own defaults are fine for a
throwaway dev stack).

This CLI doesn't know Versola's own topology (ports, service names,
config schema) — that lives entirely in the versioned "versola-tools"
image, which this command pulls and runs to generate everything the
compose stack needs. See the project design doc, section 3.5.

Internally this runs "configure", "migrate", and "up" in order, asking
for confirmation only once up front on vps rather than once per step.
Run those separately instead of bootstrap when a deployment (e.g. onto
a server) needs to stop between them — to review the plan before it
touches a live database, or to run migrate on its own schedule.`,
	Args: cobra.ExactArgs(2),
	RunE: runBootstrap,
}

func init() {
	bootstrapCmd.Flags().BoolVar(&noBrowser, "no-browser", false, "don't open the admin console in a browser once it's ready")
	bootstrapCmd.Flags().StringVar(&authURL, "auth-url", "", "public URL auth will be reachable at (required for vps, e.g. https://id.example.com)")
	bootstrapCmd.Flags().StringVar(&postgresHost, "postgres-host", "", "host:port Postgres is reachable on (required for vps, e.g. 127.0.0.1:5432)")
}

// runBootstrap deploys in one go, which is all this command has ever
// done and all it should keep doing: it's what the README, the install
// scripts and everyone's muscle memory point at.
//
// It calls deploy.Configure/Migrate/Up directly rather than going through
// the "versola configure"/"versola migrate"/"versola up" commands
// themselves — those each confirm on their own before touching a vps
// deployment (see cmd/migrate.go, cmd/up.go), which would mean asking
// three times here for what's really one decision. This asks once, up
// front, before Configure runs at all.
func runBootstrap(cmd *cobra.Command, args []string) error {
	target, version := args[0], args[1]

	// Checked here, not left to versola-tools' own entrypoint.sh check --
	// failing before Configure even starts pulling/running the tools
	// image gives a faster, clearer error than letting it fail deep
	// inside a container run.
	if target == "vps" && authURL == "" {
		return fmt.Errorf("--auth-url is required for vps deployments (e.g. --auth-url https://id.example.com)")
	}
	if target == "vps" && postgresHost == "" {
		return fmt.Errorf("--postgres-host is required for vps deployments (e.g. --postgres-host 127.0.0.1:5432)")
	}

	// Before Configure, not between it and Up -- Configure's own Finalize
	// commits to this being "the" deployment (overwrites state.json,
	// deletes the previous bundle) before Up would otherwise ask this,
	// which made declining, or Up failing partway, unable to actually get
	// anyone back to the old deployment record even though its containers
	// might still be what's really serving traffic (flagged in review on
	// versolauth/versola-cli#7). Asking first means nothing happens at
	// all if the answer is no.
	//
	// Same two-case split as cmd/configure.go's own check (see the long
	// comment there) -- target == "vps" alone only covers deploying TO a
	// vps; it doesn't cover "bootstrap local ..." run while the machine's
	// stored state already tracks a live vps deployment, which Configure's
	// Finalize would silently orphan (flagged in review). confirmIfVpsState
	// covers that second case by reading stored state instead of this run's
	// target; only one of the two branches ever fires, same reasoning as
	// configure.go.
	if target == "vps" {
		action := fmt.Sprintf("deploy Versola %s to the VPS, including running database migrations against the live database", version)
		if err := deploy.ConfirmVpsDeploy(action); err != nil {
			return err
		}
	} else if _, err := confirmIfVpsState(func(prevVersion string) string {
		return fmt.Sprintf("replace this machine's record of VPS deployment %s (still running) with a %s deployment -- versola status/down/uninstall won't be able to find or manage the VPS containers until you configure vps again", prevVersion, target)
	}); err != nil {
		// The state loaded here (if any) describes the record this run is
		// about to REPLACE, not the deployment Configure below is about to
		// build -- nothing to hand forward to Migrate/Up the way the state
		// loaded just below is.
		return err
	}

	if _, err := deploy.Configure(target, version, authURL, postgresHost); err != nil {
		return err
	}

	// Loaded once here and passed to both Migrate and Up, rather than
	// letting each read its own -- same reasoning as cmd/migrate.go's own
	// comment on confirmIfVpsState: Configure above just Finalized this
	// exact deployment, so this is that same record, not a re-derived
	// guess. Passing it through (instead of each step re-reading
	// state.json independently) also means Migrate and Up stay pinned to
	// the one deployment this run confirmed and configured, even if some
	// other `configure` finalizes a different one while this run's own
	// migrations are still in progress -- see deploy.Migrate's own comment
	// on why that's the correct behavior, not a bug to route around.
	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("configure succeeded but couldn't reload its own state: %w", err)
	}

	if err := deploy.Migrate(st); err != nil {
		return err
	}
	return deploy.Up(deploy.UpOptions{NoBrowser: noBrowser}, st)
}
