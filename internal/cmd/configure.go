package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

var configureYes bool
var configureAuthURL string
var configurePostgresHost string
var configureSetupOpenBao bool
var configureProxy string
var configureACMEStaging bool
var configureCentralURL string
var configureNoCertsTimer bool
var configureGenerateMissing []string
var configureAllowLegacy bool
var configurePostgresDB string
var configurePostgresCA string
var configureRegistry string
var configureAllowFreshDB bool

var configureCmd = &cobra.Command{
	Use:     "configure <target> [<version>]",
	GroupID: groupDeploy,
	Short:   "Prepare a deployment without starting it",
	Long: `configure prepares a deployment without starting it: it checks the machine,
generates this release's configs and stores its secrets in OpenBao.

  versola configure local
  versola configure local 0.7.5
  versola configure vps 0.7.5 --auth-url https://id.example.com
  versola configure vps 0.7.5 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432
  versola configure vps 0.7.5 --auth-url https://id.example.com --proxy external
  versola configure vps 0.7.6      # later: the flags given before are remembered

On local the version can be left out: the version already deployed on this
machine is kept, otherwise the latest release (never a pre-release) is used,
and the first line says which. On vps it is required. "versola versions"
lists the releases.

It starts none of Versola's services and touches no database: "versola
migrate" and "versola up" come next ("versola bootstrap" runs all three). On
vps, Postgres runs in a container of the deployment unless --postgres-host
names your own, and Versola's nginx serves ports 80/443 with a Let's Encrypt
certificate (the domain's DNS has to point at this server) unless --proxy
external puts it behind your web server. On vps it asks before it replaces
this machine's record of the current deployment, and it writes no secret
while one looks lost (see "versola secrets plan").

More: README "configure <target> [<version>] / migrate / up", "Redeploying on
the same server", "Postgres on vps", "Deploying to a VPS: the reverse proxy",
"Secrets", "Public names". Every flag: versola configure --help-all`,
	// One argument is "local" without a version (the latest release, or the
	// version already deployed -- see target_version.go); vps without one is
	// refused there, with an example, rather than by cobra's "accepts
	// between 1 and 2 arg(s)".
	Args: cobra.RangeArgs(1, 2),
	RunE: runConfigure,
}

func init() {
	configureCmd.Flags().BoolVarP(&configureYes, "yes", "y", false, "don't ask for confirmation (vps)")
	configureCmd.Flags().StringVar(&configureAuthURL, "auth-url", "", authURLUsage)
	configureCmd.Flags().StringVar(&configurePostgresHost, "postgres-host", "", postgresHostUsage)
	configureCmd.Flags().StringVar(&configurePostgresDB, "postgres-db", "", postgresDBUsage)
	configureCmd.Flags().StringVar(&configurePostgresCA, "postgres-ca", "", postgresCAUsage)
	configureCmd.Flags().StringVar(&configureRegistry, "registry", "", registryUsage)
	configureCmd.Flags().StringVar(&configureProxy, "proxy", proxy.ModeNginx, proxyUsage)
	configureCmd.Flags().StringVar(&configureCentralURL, "central-url", "", centralURLUsage)
	addPublicFlags(configureCmd)
	configureCmd.Flags().BoolVar(&configureNoCertsTimer, "no-certs-timer", false, "vps only: don't install the daily timer that renews the mutual-TLS certificates; refused unless --cert-lifetime-days is unlimited")
	configureCmd.Flags().BoolVar(&configureACMEStaging, "acme-staging", false, acmeStagingUsage)
	configureCmd.Flags().StringSliceVar(&configureGenerateMissing, "generate-missing", nil, generateMissingUsage)
	configureCmd.Flags().BoolVar(&configureAllowLegacy, "allow-legacy-secrets", false, allowLegacyUsage)
	configureCmd.Flags().BoolVar(&configureSetupOpenBao, "setup-openbao", false, setupOpenBaoUsage)
	configureCmd.Flags().BoolVar(&configureAllowFreshDB, "allow-fresh-database", false, allowFreshDatabaseUsage)
	// --help shows what a first deployment on one server needs; the rest is
	// under --help-all (see help.go).
	hideFlags(configureCmd, advancedDeployFlags...)
	hideFlags(configureCmd, "allow-fresh-database")
}

func runConfigure(cmd *cobra.Command, args []string) error {
	target := args[0]

	// First of all, before the lock and any question: a typo'd target is a
	// usage error (exit code 2), and must not cost a "Continue?" answered
	// for a deployment that was never going to happen.
	if err := deploy.CheckTarget(target); err != nil {
		return err
	}
	if err := rejectVpsOnlyFlags(cmd, target); err != nil {
		return err
	}
	// Validated before the lock and the confirmation: a typo'd CA path
	// should fail immediately, not after the user has said yes.
	pg, err := deploy.PostgresOptions{DB: configurePostgresDB, CAFile: configurePostgresCA}.Validate(target)
	if err != nil {
		return err
	}

	registry, err := deploy.ParseRegistry(configureRegistry)
	if err != nil {
		return err
	}

	// Held from here through deploy.Configure's own Finalize -- see
	// state.Lock's own comment. Without it, the confirmation decision just
	// below is checking state that a concurrent `configure` could still
	// change before deploy.Configure gets around to acting on it (flagged
	// in review): this run might confirm "nothing to replace" and then
	// silently overwrite a vps deployment that Finalized in the gap.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// Under the lock, first: the version decides what everything below is
	// about, and "Deploying Versola 0.7.5 (the latest release ...)" has to
	// be the first thing said. Also refuses vps without a version, before
	// --auth-url is asked for.
	choice, err := chooseVersion(os.Stdout, "configure", args)
	if err != nil {
		return err
	}
	version := choice.Version

	// Under the lock: the flags left out are taken from the record this run
	// replaces (see vpsFlags), which a concurrent configure could otherwise
	// change in between. Also where a missing --auth-url is refused.
	vf, err := resolveVpsFlags(cmd, target)
	if err != nil {
		return err
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
	//
	// The second branch passes false for confirmIfVpsState's assumeYes on
	// purpose, whatever --yes says: it guards the record being REPLACED, and
	// answering it for a script would leave status/down/uninstall unable to
	// find the vps containers.
	if target == "vps" {
		action := fmt.Sprintf("make Versola %s the deployment this machine tracks for the VPS, replacing its current record (nothing is started or stopped yet)", version)
		if err := confirmVpsRun(os.Stdout, vf, configureYes, action); err != nil {
			return err
		}
	} else if _, err := confirmIfVpsState(false, func(prevVersion string) string {
		return fmt.Sprintf("replace this machine's record of VPS deployment %s (still running) with a %s deployment -- versola status/down/uninstall won't be able to find or manage the VPS containers until you configure vps again", prevVersion, target)
	}); err != nil && !errors.Is(err, state.ErrOldInstall) {
		// The state confirmIfVpsState loaded (if any) describes the record
		// this run is about to REPLACE, not the deployment Configure below
		// is about to build -- there is nothing to hand forward here the
		// way cmd/migrate.go and cmd/up.go do with their own state.
		return err
	}
	// (A record too old to read -- ErrOldInstall -- is a local one: vps
	// deployments came later. This run replaces it, which is what that
	// error asks for, so it is not a reason to stop.)

	public, err := publicDomains(cmd, target)
	if err != nil {
		return err
	}
	days, err := certDays(cmd, target)
	if err != nil {
		return err
	}
	est, err := estEnabled(cmd, target)
	if err != nil {
		return err
	}
	if target == "vps" {
		if err := refuseNoTimerWhenFinite(configureNoCertsTimer, days); err != nil {
			return err
		}
	}
	_, err = deploy.Configure(target, version, vf.AuthURL, vf.PostgresHost, pg, vf.SetupOpenBao,
		deploy.ProxyOptions{Mode: vf.Proxy, ACMEStaging: vf.ACMEStaging, CentralURL: configureCentralURL, Public: public, CertDays: days, Est: est},
		deploy.SecretOptions{GenerateMissing: configureGenerateMissing, AllowLegacy: configureAllowLegacy}, registry, configureAllowFreshDB)
	if err != nil {
		return explainMissingImage(err, choice, "configure")
	}
	scheduleCertsRenewal(os.Stdout, target, configureNoCertsTimer || !deploy.IssuesMutualTLSCertificates(version), days != nil && *days == 0)
	return nil
}

// rejectVpsOnlyFlags: --proxy/--acme-staging/--postgres-db/--postgres-ca only mean something for vps
// (local's proxy is always on 127.0.0.1:2821, without TLS) -- say so instead of
// silently ignoring them. A usage error (exit code 2).
//
// The four in localFixed used to be accepted on local and ignored, which is
// the worse failure: a vps command copied with only the target changed gave a
// local deployment that silently did not use the domain, the database or the
// OpenBao it named. Their message says what local does instead.
// --generate-missing, --allow-legacy-secrets, --registry, --no-browser and
// --yes stay accepted: they mean the same on local.
func rejectVpsOnlyFlags(cmd *cobra.Command, target string) error {
	if target == "vps" {
		return nil
	}
	for _, name := range append([]string{"proxy", "acme-staging", "central-url", "postgres-db", "postgres-ca", "est", "allow-fresh-database"}, publicFlagNames...) {
		if cmd.Flags().Changed(name) {
			return exitcode.With(exitcode.Usage, fmt.Errorf("--%s only applies to vps deployments", name))
		}
	}
	for _, f := range localFixed {
		if cmd.Flags().Changed(f.name) {
			return exitcode.With(exitcode.Usage, fmt.Errorf("--%s only applies to vps deployments (%s)", f.name, f.instead))
		}
	}
	return nil
}

// localFixed are the vps flags whose job local always does one fixed way;
// instead says which, for rejectVpsOnlyFlags' message.
var localFixed = []struct{ name, instead string }{
	{"auth-url", "local always runs at http://localhost:2821"},
	{"postgres-host", "local always runs its own Postgres, in a container"},
	{"setup-openbao", "local always sets up its own OpenBao"},
	{"no-certs-timer", "local installs no renewal timer"},
}

const centralURLUsage = "vps only: how edge reaches central's mutual-TLS listener (default: https://127.0.0.1:8093, the loopback address -- a local edge never uses a public name). Only for an edge elsewhere that is configured by hand; see --central-mtls-domain for publishing the name and 'versola edge add' for a remote edge's bootstrap bundle"

const registryUsage = "pull Versola's images from this registry prefix instead of ghcr.io/versolauth (e.g. cr.yandex/crp1a2b3c): versola-tools, central, auth and edge as PREFIX/versola-<name>:<version>; no login is done, run docker login first"

const allowFreshDatabaseUsage = "vps only: deploy a release that says no in-place upgrade exists from the version recorded here, because you already put a new, empty database behind it (without it configure refuses before changing anything)"

const postgresDBUsage = "vps only: name of the Postgres database to use (default: auth)"
const postgresCAUsage = "vps only: PEM file with the CA certificate(s) that signed Postgres' server certificate, for sslmode=verify-full when that CA is not in the JVM trust store"
