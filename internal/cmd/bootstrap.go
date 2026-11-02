package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

var noBrowser bool
var authURL string
var postgresHost string
var setupOpenBao bool
var bootstrapProxy string
var bootstrapACMEStaging bool
var bootstrapCentralURL string
var bootstrapPostgresDB string
var bootstrapPostgresCA string
var bootstrapRegistry string
var bootstrapGenerateMissing []string
var bootstrapAllowLegacy bool

var bootstrapYes bool
var bootstrapNoCertsTimer bool

var bootstrapCmd = &cobra.Command{
	Use:     "bootstrap <target> [<version>]",
	GroupID: groupGetStarted,
	Short:   "Deploy a version of Versola",
	Long: `bootstrap deploys a released version of Versola in one go: configure, migrate, up.

  versola bootstrap local
  versola bootstrap local 0.1.1
  versola bootstrap vps 0.1.1 --auth-url https://id.example.com
  versola bootstrap vps 0.1.1 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432
  versola bootstrap vps 0.1.1 --auth-url https://id.example.com --proxy external

On local the version can be left out: bootstrap then keeps the version
already deployed on this machine or, on a first deployment, deploys the
latest release (never a pre-release), and says which on its first line. On
vps it is required. "versola versions" lists the releases.

local needs no flag. On vps, --auth-url is the public URL the deployment is
reached at (needed the first time, then remembered); Versola gets its own nginx
with a Let's Encrypt certificate, OpenBao is set up automatically, Postgres
runs in a container unless --postgres-host names your own, and a daily timer
renews the mutual-TLS certificates. On vps it asks once, before anything
changes. With your own Postgres a first install stops before migrate, after
printing how to set the role's password. Run "versola configure", "migrate"
and "up" one by one to stop between the steps.

More: README "bootstrap local [<version>]", "Deploying to a VPS: the reverse
proxy", "Postgres on vps", "Secrets". Every flag: versola bootstrap --help-all`,
	// One argument is "local" without a version (the latest release, or the
	// version already deployed -- see target_version.go); vps without one is
	// refused there, with an example, rather than by cobra's "accepts
	// between 1 and 2 arg(s)".
	Args: cobra.RangeArgs(1, 2),
	RunE: runBootstrap,
}

func init() {
	bootstrapCmd.Flags().BoolVarP(&bootstrapYes, "yes", "y", false, "don't ask for confirmation (vps)")
	bootstrapCmd.Flags().BoolVar(&bootstrapNoCertsTimer, "no-certs-timer", false, "vps only: don't install the daily timer that renews the mutual-TLS certificates ('versola certs renew'); refused unless the certificates are unlimited (--cert-lifetime-days unlimited), since they expire without it")
	bootstrapCmd.Flags().BoolVar(&noBrowser, "no-browser", false, "don't open the admin console in a browser once it's ready")
	bootstrapCmd.Flags().StringVar(&authURL, "auth-url", "", authURLUsage)
	bootstrapCmd.Flags().StringVar(&postgresHost, "postgres-host", "", postgresHostUsage)
	bootstrapCmd.Flags().StringVar(&bootstrapPostgresDB, "postgres-db", "", postgresDBUsage)
	bootstrapCmd.Flags().StringVar(&bootstrapPostgresCA, "postgres-ca", "", postgresCAUsage)
	bootstrapCmd.Flags().StringVar(&bootstrapRegistry, "registry", "", registryUsage)
	bootstrapCmd.Flags().StringVar(&bootstrapProxy, "proxy", proxy.ModeNginx, proxyUsage)
	bootstrapCmd.Flags().StringVar(&bootstrapCentralURL, "central-url", "", centralURLUsage)
	addPublicFlags(bootstrapCmd)
	bootstrapCmd.Flags().BoolVar(&bootstrapACMEStaging, "acme-staging", false, acmeStagingUsage)
	bootstrapCmd.Flags().StringSliceVar(&bootstrapGenerateMissing, "generate-missing", nil, generateMissingUsage)
	bootstrapCmd.Flags().BoolVar(&bootstrapAllowLegacy, "allow-legacy-secrets", false, allowLegacyUsage)
	bootstrapCmd.Flags().BoolVar(&setupOpenBao, "setup-openbao", false, setupOpenBaoUsage)
	// As configure: the advanced flags are under --help-all (see help.go).
	hideFlags(bootstrapCmd, advancedDeployFlags...)
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
	target := args[0]

	// First, as in cmd/configure.go: before the lock and any question.
	if err := deploy.CheckTarget(target); err != nil {
		return err
	}
	if err := rejectVpsOnlyFlags(cmd, target); err != nil {
		return err
	}
	// Validated before the lock and the confirmation: a typo'd CA path
	// should fail immediately, not after the user has said yes.
	pg, err := deploy.PostgresOptions{DB: bootstrapPostgresDB, CAFile: bootstrapPostgresCA}.Validate(target)
	if err != nil {
		return err
	}

	registry, err := deploy.ParseRegistry(bootstrapRegistry)
	if err != nil {
		return err
	}

	// Held for the whole configure/migrate/up sequence below -- see
	// state.Lock's own comment, and cmd/configure.go's/cmd/migrate.go's
	// identical use of it. Without it, a concurrent `versola configure`
	// (a second terminal, a second SSH session) could finalize a different
	// deployment at any point in this sequence -- before the confirmation
	// decision just below is even acted on, or between Migrate and Up --
	// and this run would carry on against state.json out from under
	// itself.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// Under the lock, first: the version decides what everything below is
	// about, and "Deploying Versola 0.7.5 (the latest release ...)" has to
	// be the first thing said. Also refuses vps without a version, before
	// --auth-url is asked for.
	choice, err := chooseVersion(os.Stdout, "bootstrap", args)
	if err != nil {
		return err
	}
	version := choice.Version

	// Under the lock, as in cmd/configure.go: the flags left out come from
	// the record this run replaces. Also where a missing --auth-url is
	// refused, before anything is pulled or started.
	vf, err := resolveVpsFlags(cmd, target)
	if err != nil {
		return err
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
	// configure.go. That second branch passes false for assumeYes on purpose,
	// whatever --yes says (see configure.go): answering it for a script would
	// leave status/down/uninstall unable to find the vps containers.
	if target == "vps" {
		action := fmt.Sprintf("deploy Versola %s to the VPS, including running database migrations against the live database", version)
		if err := confirmVpsRun(os.Stdout, vf, bootstrapYes, action); err != nil {
			return err
		}
	} else if _, err := confirmIfVpsState(false, func(prevVersion string) string {
		return fmt.Sprintf("replace this machine's record of VPS deployment %s (still running) with a %s deployment -- versola status/down/uninstall won't be able to find or manage the VPS containers until you configure vps again", prevVersion, target)
	}); err != nil && !errors.Is(err, state.ErrOldInstall) {
		// The state loaded here (if any) describes the record this run is
		// about to REPLACE, not the deployment Configure below is about to
		// build -- nothing to hand forward to Migrate/Up the way the state
		// loaded just below is.
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
		if err := refuseNoTimerWhenFinite(bootstrapNoCertsTimer, days); err != nil {
			return err
		}
	}
	res, err := deploy.Configure(target, version, vf.AuthURL, vf.PostgresHost, pg, vf.SetupOpenBao,
		deploy.ProxyOptions{Mode: vf.Proxy, ACMEStaging: vf.ACMEStaging, CentralURL: bootstrapCentralURL, Public: public, CertDays: days, Est: est},
		deploy.SecretOptions{GenerateMissing: bootstrapGenerateMissing, AllowLegacy: bootstrapAllowLegacy}, registry)
	if err != nil {
		return explainMissingImage(err, choice, "bootstrap")
	}
	// Migrate would only fail to log in to Postgres: the role doesn't
	// have the password Configure just generated until someone runs the
	// SQL it wrote (and said where). Stop here with the next steps instead.
	if res.PostgresRoleSetupNeeded {
		// An error, not nil: the deployment isn't up yet, and a script
		// running bootstrap must be able to tell that from its exit code.
		if res.PostgresRoleScript == "" {
			return fmt.Errorf("stopped before migrate: set the Postgres role's password as said above (or let `versola postgres init` create the role, the database and the schemas), then run `versola migrate` and `versola up`")
		}
		return fmt.Errorf("stopped before migrate: set the Postgres role's password by feeding %s to psql as the Postgres superuser (see above), or let `versola postgres init` create the role, the database and the schemas, then run `versola migrate` and `versola up`", res.PostgresRoleScript)
	}

	// Loaded once here and passed to both Migrate and Up, rather than
	// letting each read its own -- Configure above just Finalized this
	// exact deployment, so this is that same record, not a re-derived
	// guess. The lock held for this whole function already rules out
	// another `configure` finalizing something else mid-sequence (see
	// above); this just avoids two redundant reads of what's already
	// known, the same reasoning as cmd/migrate.go's own comment on
	// confirmIfVpsState.
	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("configure succeeded but couldn't reload its own state: %w", err)
	}

	// Zero-value MigrateOptions: not a dry run, every service -- bootstrap
	// runs configure/migrate/up together precisely to deploy for real, so
	// it has no use for either flag standalone `versola migrate` exposes
	// (see cmd/migrate.go).
	if err := deploy.Migrate(st, deploy.MigrateOptions{}); err != nil {
		return err
	}
	if err := deploy.Up(deploy.UpOptions{NoBrowser: noBrowser}, st); err != nil {
		return err
	}
	// The certificates are short-lived: the timer is part of deploying to a
	// server, not an afterthought in the runbook (see scheduleCertsRenewal).
	scheduleCertsRenewal(os.Stdout, target, bootstrapNoCertsTimer || !deploy.IssuesMutualTLSCertificates(version), days != nil && *days == 0)
	return nil
}
