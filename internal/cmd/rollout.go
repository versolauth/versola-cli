package cmd

import (
	"errors"
	"fmt"
	"os"

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
	Use:     "rollout (<version> | --abort)",
	GroupID: groupUpgrade,
	Short:   "Upgrade a running vps deployment to another version without stopping it",
	Long: `rollout moves a running vps deployment to another Versola version while it
keeps serving.

  versola rollout 0.8.1 --dry-run
  versola rollout 0.8.1
  versola rollout --abort

It prepares the new version next to the running one, migrates the database
(back it up first: migrations can't be rolled back), replaces central, auth
and edge one replica at a time behind a stand-in, then moves the proxy.
The release declares which versions it can replace (release.json): a rollout
from one of them needs no flag, one from a version that needs a fresh database
is refused. --assume-compatible is for a release that declares nothing: it says
you read the release notes and the running version works on the new schema
(releases before 1.0 don't promise it). Cut off -- an error,
Ctrl-C, a dropped SSH session -- the same command continues where it stopped;
Ctrl-C stops after the step under way, a second Ctrl-C exits at once. --abort
goes back to the version the rollout came from; migrations and secrets stay.

More: README "rollout <version> / rollout --abort (vps)", "Runbook: a rolling
upgrade on the vps". Every flag: versola rollout --help-all`,
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
	rolloutCmd.Flags().BoolVar(&rolloutAssumeCompat, "assume-compatible", false, "only for a release that declares no compatibility with the running version: you checked its notes and the running version works on the new schema (cannot override a release that declares it needs a fresh database)")
	// Recovery flags, for a secrets plan that stops: under --help-all.
	hideFlags(rolloutCmd, "generate-missing", "allow-legacy-secrets")
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
	fmt.Print(deploy.RolloutPlan(st, version, opts))
	if err := deploy.ConfirmVpsDeployYes(rolloutYes, fmt.Sprintf("roll Versola %s out on the VPS as above, applying its migrations to the live database", version)); err != nil {
		return err
	}
	if err := deploy.Rollout(st, version, opts); err != nil {
		return err
	}
	// A rollout from 0.7 to 0.8 is where finite certificates first appear, and
	// nothing else installs the timer that renews them (a rollout is not `up`).
	// The record is read again: what runs now is the new version.
	if now, err := state.Load(); err == nil {
		scheduleCertsRenewal(os.Stdout, now.Target, !deploy.IssuesMutualTLSCertificates(now.Version), now.CertsUnlimited())
	}
	return nil
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
