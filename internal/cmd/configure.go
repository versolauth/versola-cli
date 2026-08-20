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
resolves its secrets against OpenBao. It doesn't start anything and
doesn't touch the database — see "versola migrate" and "versola up" for
the steps after this one.

  versola configure local 0.1.1
  versola configure vps 0.1.1 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432

vps requires OpenBao credentials to already be stored for it (see
"versola secrets login vps"). --auth-url and --postgres-host are both
required for vps — see "versola bootstrap --help" for why.

Against a vps deployment this asks for confirmation first: it doesn't
start or stop anything, but it does replace this machine's record of
which deployment is the current one, which is what "versola status",
"down" and "uninstall" all act on.

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
	// Keyed off the target argument, not stored state (unlike migrate/up --
	// see confirmIfVpsState): the deployment being replaced might not be a
	// vps one, and it's this run's target that decides whether a real server
	// is involved.
	if target == "vps" {
		action := fmt.Sprintf("make Versola %s the deployment this machine tracks for the VPS, replacing its current record (nothing is started or stopped yet)", version)
		if err := deploy.ConfirmVpsDeploy(action); err != nil {
			return err
		}
	}

	_, err := deploy.Configure(target, version, configureAuthURL, configurePostgresHost)
	return err
}
