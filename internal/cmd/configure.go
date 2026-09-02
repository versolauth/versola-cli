package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
)

var configureAuthURL string
var configurePostgresHost string

var configureCmd = &cobra.Command{
	Use:   "configure <target> <version>",
	Short: "Prepare a deployment without starting it",
	Long: `configure checks the machine, generates this release's configs, and
resolves its secrets against OpenBao (starting OpenBao itself first if
it isn't already running). It doesn't start any of Versola's own
services and doesn't touch the database — see "versola migrate" and
"versola up" for the steps after this one.

  versola configure local 0.1.1
  versola configure vps 0.1.1 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432

vps requires OpenBao credentials to already be stored for it (see
"versola secrets login vps"). --auth-url and --postgres-host are both
required for vps — see "versola bootstrap --help" for why.

Against a vps deployment this asks for confirmation first: it doesn't
start or stop any of Versola's own services, but it does replace this
machine's record of which deployment is the current one, which is what
"versola status", "down" and "uninstall" all act on.

This is the first of three steps "versola bootstrap" runs together in
one go, in order: configure, migrate, up. Use these separately when a
database migration needs to be its own explicit, reviewable step rather
than something that happens silently — e.g. deploying onto a server.`,
	Args: cobra.ExactArgs(2),
	RunE: runConfigure,
}

func init() {
	configureCmd.Flags().StringVar(&configureAuthURL, "auth-url", "", "public URL auth will be reachable at (required for vps, e.g. https://id.example.com)")
	configureCmd.Flags().StringVar(&configurePostgresHost, "postgres-host", "", "host:port Postgres is reachable on (required for vps, e.g. 127.0.0.1:5432)")
}

func runConfigure(cmd *cobra.Command, args []string) error {
	target, version := args[0], args[1]

	// Checked here too, not just inside deploy.Configure — see
	// bootstrap.go's identical check for why: failing before Configure
	// even starts pulling/running the tools image gives a faster, clearer
	// error than letting it fail deep inside a container run.
	if target == "vps" && configureAuthURL == "" {
		return fmt.Errorf("--auth-url is required for vps deployments (e.g. --auth-url https://id.example.com)")
	}
	if target == "vps" && configurePostgresHost == "" {
		return fmt.Errorf("--postgres-host is required for vps deployments (e.g. --postgres-host 127.0.0.1:5432)")
	}

	// Configure looks like the harmless step of the three -- it starts
	// nothing and touches no database -- but its own Finalize is
	// irreversible: it overwrites state.json to point at this run's bundle
	// and deletes the previous deployment's (see state.Finalize). On vps
	// that record is how status/down/uninstall find the containers actually
	// serving traffic, so a configure that then fails, or was simply run by
	// mistake, costs the machine its handle on the live deployment. That's
	// the same hazard that moved bootstrap's own confirmation to *before*
	// Configure (flagged in review on versolauth/versola-cli#7) -- running
	// configure standalone must not be the way around it.
	//
	// Two separate reasons to confirm here, and either can hold without the
	// other:
	//
	//  1. target == "vps": this run would make a *new* vps deployment the
	//     one this machine tracks -- the case this used to be the only
	//     check for.
	//
	//  2. the record this run is about to *replace* already targets vps
	//     (checked via confirmIfVpsState, the same helper migrate/up use,
	//     which reads stored state rather than this run's target). Finalize
	//     (see state.Finalize) overwrites state.json and deletes the
	//     previous bundle unconditionally, regardless of this run's own
	//     target -- so "configure local ..." against a machine currently
	//     tracking a live vps deployment silently orphans it: the vps
	//     containers stay up, but status/down/uninstall can no longer find
	//     them, with no prompt to stop and reconsider (flagged in review).
	//     Case 1 alone doesn't cover this: it only fires when the *new*
	//     target is vps, not when the *replaced* one is.
	//
	// Only one of the two ever fires: confirmIfVpsState itself is a no-op
	// unless stored state targets vps, and target == "vps" here means the
	// replaced state, if any, is asked about via case 1's own message
	// instead -- so this is deliberately if/else-if, not two independent
	// ifs, to avoid a double prompt when both technically apply.
	if target == "vps" {
		action := fmt.Sprintf("make Versola %s the deployment this machine tracks for the VPS, replacing its current record (nothing is started or stopped yet)", version)
		if err := deploy.ConfirmVpsDeploy(action); err != nil {
			return err
		}
	} else if _, err := confirmIfVpsState(func(prevVersion string) string {
		return fmt.Sprintf("replace this machine's record of VPS deployment %s (still running) with a %s deployment -- versola status/down/uninstall won't be able to find or manage the VPS containers until you configure vps again", prevVersion, target)
	}); err != nil {
		// The state confirmIfVpsState loaded (if any) describes the record
		// this run is about to REPLACE, not the deployment Configure below
		// is about to build -- there is nothing to hand forward here the
		// way cmd/migrate.go and cmd/up.go do with their own state.
		return err
	}

	_, err := deploy.Configure(target, version, configureAuthURL, configurePostgresHost)
	return err
}
