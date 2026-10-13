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
)

var rolloutCmd = &cobra.Command{
	Use:   "rollout <version>",
	Short: "Upgrade a running vps deployment to another version without stopping it",
	Long: `rollout moves a running vps deployment to another Versola version while it
keeps serving:

  1. prepare: the new version's configuration is generated next to the
     running one (versola-tools, the secrets plan -- secrets new in that
     version are stored in OpenBao). Nothing running changes.
  2. migrate: the new version's database migrations are applied. They
     cannot be rolled back: back the database up first. Releases keep their
     migrations additive, so the running version works on the new schema.
  3. central is stopped and started on the new version: the admin console
     and self-registration are unavailable until it is ready (about a
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
rollout is unfinished, and going back (--abort) is not part of this
versola-cli yet.

--no-surge replaces replicas without a stand-in (for a server without the
memory for one more JVM): one replica fewer serves meanwhile, so it needs
two or more -- with one, --allow-downtime accepts that it is down until
the new one is ready.

--dry-run prints the plan, and from the new version's versola-tools (run
into a temporary directory) its secrets plan and the migrations it would
apply. It changes nothing.

Only for vps deployments made by this versola-cli; a local one is configured
and started again instead.`,
	Args: cobra.ExactArgs(1),
	RunE: runRollout,
}

func init() {
	rolloutCmd.Flags().BoolVar(&rolloutDryRun, "dry-run", false, "print what would happen (plan, secrets, pending migrations) and change nothing")
	rolloutCmd.Flags().BoolVarP(&rolloutYes, "yes", "y", false, "don't ask for confirmation")
	rolloutCmd.Flags().BoolVar(&rolloutNoSurge, "no-surge", false, "no stand-in replica: replace each replica with one fewer serving meanwhile (needs two or more, or --allow-downtime)")
	rolloutCmd.Flags().BoolVar(&rolloutAllowDowntime, "allow-downtime", false, "with --no-surge: recreate a service that has one replica in place, unreachable until it is ready")
	rolloutCmd.Flags().StringSliceVar(&rolloutGenerateMissing, "generate-missing", nil, generateMissingUsage)
	rolloutCmd.Flags().BoolVar(&rolloutAllowLegacy, "allow-legacy-secrets", false, allowLegacyUsage)
}

func runRollout(cmd *cobra.Command, args []string) error {
	version := args[0]
	opts := deploy.RolloutOptions{
		NoSurge: rolloutNoSurge, AllowDowntime: rolloutAllowDowntime,
		Secrets: deploy.SecretOptions{GenerateMissing: rolloutGenerateMissing, AllowLegacy: rolloutAllowLegacy},
	}

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

	fmt.Print(deploy.RolloutPlan(st, version, opts))
	if !rolloutYes {
		if err := deploy.ConfirmVpsDeploy(fmt.Sprintf("roll Versola %s out on the VPS as above, applying its migrations to the live database", version)); err != nil {
			return err
		}
	}
	return deploy.Rollout(st, version, opts)
}
