package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
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

Internally this is two steps — prepare the deployment, then start it —
which is what a deployment onto a server will need to be able to run
separately, with a database migration between them. See internal/deploy.`,
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
// The steps it calls are separate functions rather than one, because the
// server deployment this is being prepared for has to be able to stop
// between them. Nothing about that is visible here yet — deliberately, so
// that this rearrangement can be verified by a local deployment behaving
// exactly as it did before.
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

	if _, err := deploy.Configure(target, version, authURL, postgresHost); err != nil {
		return err
	}
	return deploy.Up(deploy.UpOptions{NoBrowser: noBrowser})
}
