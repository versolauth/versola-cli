package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var (
	rolloutDryRun          bool
	rolloutYes             bool
	rolloutNoSurge         bool
	rolloutAllowDowntime   bool
	rolloutGenerateMissing []string
	rolloutAllowLegacy     bool
	rolloutAbort           bool
	rolloutAssumeCompat    bool
)

var rolloutCmd = &cobra.Command{
	Use:   "rollout (<version> | --abort)",
	Short: "Upgrade a running vps deployment to another version without stopping it",
	Long: `rollout moves a running vps deployment to another Versola version while it
keeps serving:

  1. prepare: the new version's configuration is generated next to the
     running one (versola-tools, the secrets plan -- secrets new in that
     version are stored in OpenBao). Nothing running changes.
  2. migrate: the new version's database migrations are applied. They
     cannot be rolled back: back the database up first. Releases keep their
     migrations additive, so the running version works on the new schema.
  3. central: a stand-in of the new version (central-2) starts next to the
     running one and takes traffic, then central itself is recreated on the
     new version and the stand-in is removed, so the admin console and
     self-registration stay available. Without a stand-in (--no-surge, or
     the first rollout of a deployment whose proxy does not front central
     yet) central is stopped and started in place, and the admin console and
     self-registration are unavailable until it is ready (usually under a
     minute); auth and edge keep serving.
  4. auth, then edge: a stand-in replica of the new version starts in a free
     slot and takes traffic; then each replica in turn is taken out of the
     proxy, drained, recreated on the new version, waited for, and put back;
     then the stand-in is removed. The service never has fewer replicas
     serving than before.
  5. the proxy moves to the new configuration and the old one is removed.

If it stops -- an error, Ctrl-C, a dropped SSH session -- running the same
command again continues from where it stopped: progress is read back from
the containers themselves. Ctrl-C stops after the step under way; a second
Ctrl-C leaves at once. A different version can't be rolled out while one
rollout is unfinished.

Compatibility: the two versions run side by side on the new version's
schema, and Versola releases before 1.0 don't promise that works (0.7.0, for
one, needs a fresh database). A rollout therefore refuses to start unless
--assume-compatible says you have checked the release notes: additive
migrations, and the running version working on the new schema.

--abort goes back to the version the unfinished rollout came from. If no
container was started from the new version's configuration yet, the
previous configuration simply becomes the active one again. Otherwise it
becomes the active one again too, and the replicas that moved on -- central,
auth, edge -- are moved back the same way, stand-in first; the ones that
never moved are not touched. (If that configuration is gone, the previous
version is configured anew and every replica is recreated from it.) Migrations and secrets do not go back: the old version runs
on the migrated schema (not guaranteed before 1.0). Cut off, --abort again
continues it. After a rollout has finished there is nothing to abort: roll
the earlier version out instead.

--no-surge replaces replicas without a stand-in (for a server without the
memory for one more JVM): one replica fewer serves meanwhile, so it needs
two or more -- with one, --allow-downtime accepts that it is down until
the new one is ready. It goes with --abort too: going back starts a stand-in
for each service it moves back (all of them if the previous version is
configured anew), so it checks free memory and Postgres
connections first, and --abort --no-surge skips both checks.

--dry-run prints the plan, and from the new version's versola-tools (run
into a temporary directory) its secrets plan and the migrations it would
apply. It changes nothing in the deployment (a sealed OpenBao -- it is after
every reboot of the server -- is unsealed first with the key saved on this
machine, same as the rollout itself does).

Only for vps deployments made by this versola-cli; a local one is configured
and started again instead.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if rolloutAbort {
			if len(args) != 0 {
				return fmt.Errorf("--abort takes no version: it goes back to the one the rollout came from")
			}
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: runRollout,
}

func init() {
	rolloutCmd.Flags().BoolVar(&rolloutDryRun, "dry-run", false, "print what would happen (plan, secrets, pending migrations) and change nothing")
	rolloutCmd.Flags().BoolVarP(&rolloutYes, "yes", "y", false, "don't ask for confirmation")
	rolloutCmd.Flags().BoolVar(&rolloutNoSurge, "no-surge", false, "no stand-in replica: replace each replica with one fewer serving meanwhile (needs two or more, or --allow-downtime)")
	rolloutCmd.Flags().BoolVar(&rolloutAllowDowntime, "allow-downtime", false, "with --no-surge: recreate a service that has one replica in place, unreachable until it is ready")
	rolloutCmd.Flags().StringSliceVar(&rolloutGenerateMissing, "generate-missing", nil, generateMissingUsage)
	rolloutCmd.Flags().BoolVar(&rolloutAllowLegacy, "allow-legacy-secrets", false, allowLegacyUsage)
	rolloutCmd.Flags().BoolVar(&rolloutAbort, "abort", false, "go back to the version the unfinished rollout came from (migrations and secrets stay; add --no-surge if memory or Postgres connections are short)")
	rolloutCmd.Flags().BoolVar(&rolloutAssumeCompat, "assume-compatible", false, "you checked the release notes: the running version works on the new version's schema (releases before 1.0 don't promise it)")
}

func runRollout(cmd *cobra.Command, args []string) error {
	opts := deploy.RolloutOptions{
		NoSurge: rolloutNoSurge, AllowDowntime: rolloutAllowDowntime,
		Secrets:          deploy.SecretOptions{GenerateMissing: rolloutGenerateMissing, AllowLegacy: rolloutAllowLegacy},
		AssumeCompatible: rolloutAssumeCompat,
	}
	if rolloutAbort {
		return runAbort(opts)
	}
	version := args[0]

	// Held for the whole run, dry run included: a concurrent configure, up
	// or replica would change the deployment this acts on halfway through.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := state.Load()
	if err != nil {
		if !errors.Is(err, state.ErrNotConfigured) {
			return err
		}
		st = nil
	}
	done, err := deploy.CheckRollout(st, version, opts)
	if err != nil {
		return err
	}
	if done {
		fmt.Printf("Versola %s is already what runs here -- nothing to roll out.\n", version)
		return nil
	}
	if rolloutDryRun {
		return deploy.RolloutDryRun(st, version, opts)
	}
	if err := deploy.RequireCompatible(st, version, opts); err != nil {
		return err
	}

	fmt.Print(deploy.RolloutPlan(st, version, opts))
	if err := deploy.ConfirmVpsDeployYes(rolloutYes, fmt.Sprintf("roll Versola %s out on the VPS as above, applying its migrations to the live database", version)); err != nil {
		return err
	}
	return deploy.Rollout(st, version, opts)
}

func runAbort(opts deploy.RolloutOptions) error {
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := state.Load()
	if err != nil {
		if !errors.Is(err, state.ErrNotConfigured) {
			return err
		}
		st = nil
	}
	if err := deploy.CheckAbort(st, opts); err != nil {
		return err
	}
	if rolloutDryRun {
		return deploy.AbortDryRun(st, opts)
	}
	fmt.Print(deploy.AbortPlan(st, opts))
	back := st.PreviousVersion
	if st.RollingBack {
		back = st.Version
	}
	if err := deploy.ConfirmVpsDeployYes(rolloutYes, fmt.Sprintf("go back to Versola %s on the VPS as above (migrations and secrets stay)", back)); err != nil {
		return err
	}
	return deploy.Abort(st, opts)
}
