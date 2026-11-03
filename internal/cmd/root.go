// Package cmd wires up the versola CLI's subcommands using cobra.
//
// This binary is deliberately thin: it doesn't know anything about
// Versola's own architecture (services, ports, database schemas). It
// checks the local machine, talks to Docker, and — once "bootstrap" is
// implemented — asks a versioned "versola-tools" image to do anything
// that requires knowing how a specific Versola release is put together.
// See the project design doc, sections 2.4 and 3.5, for why: it keeps
// this CLI's own release cadence independent of Versola's, and keeps
// service topology knowledge in one place (Scala) instead of two.
package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "versola",
	Short: "Deploy and manage Versola",
	Long: `versola is the command-line tool for deploying and managing Versola.

It checks your machine for what Versola needs (Docker, free ports, disk
space), helps you install what's missing, and deploys a specific
released version of the Versola stack using Docker.`,
	SilenceUsage: true,
	// The error is printed once, by Run (exit.go), not also by cobra.
	SilenceErrors: true,
	// Setting this makes cobra add a "--version" flag automatically, so
	// both "versola version" and "versola --version" work -- people reach
	// for either, and a support question always starts with "what version
	// are you on?".
	Version: version,
	// Starts the daily lookup behind the "a newer versola-cli is available"
	// notice (updatenotice.go). No command defines a PersistentPreRun of its
	// own, which would shadow this one.
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		pendingUpdateCheck = startUpdateCheck(cmd)
	},
}

// Command groups of `versola --help`, in the order a new customer meets
// them: check the machine and deploy in one go, the same deployment step by
// step, day-to-day operation, upgrades and scaling, then everything else.
// A command joins a group by setting GroupID in its own cobra.Command
// literal; cobra panics at start-up if a command names a group that is not
// added below, which the tests in help_test.go catch first.
const (
	groupGetStarted = "get-started"
	groupDeploy     = "deploy"
	groupOperate    = "operate"
	groupUpgrade    = "upgrade"
	groupAdvanced   = "advanced"
)

func init() {
	rootCmd.AddGroup(
		&cobra.Group{ID: groupGetStarted, Title: "Get started:"},
		&cobra.Group{ID: groupDeploy, Title: "Deploy step by step:"},
		&cobra.Group{ID: groupOperate, Title: "Operate:"},
		&cobra.Group{ID: groupUpgrade, Title: "Upgrade and scale:"},
		&cobra.Group{ID: groupAdvanced, Title: "Advanced:"},
	)
	rootCmd.SetHelpCommandGroupID(groupAdvanced)
	rootCmd.SetCompletionCommandGroupID(groupAdvanced)

	// Commands are listed in the order they are added, not alphabetically:
	// within a group, the order below is the order a customer uses them in
	// (doctor before bootstrap, configure before migrate before up). It is a
	// package-level switch of cobra, so subcommands (secrets login/test/plan,
	// edge add/bootstrap, ...) also keep the order their files add them in.
	cobra.EnableCommandSorting = false

	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(bootstrapCmd)
	rootCmd.AddCommand(versionsCmd)

	rootCmd.AddCommand(configureCmd)
	rootCmd.AddCommand(postgresCmd)
	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(upCmd)

	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(statsCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(backupCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(uninstallCmd)

	rootCmd.AddCommand(rolloutCmd)
	rootCmd.AddCommand(replicaCmd)
	rootCmd.AddCommand(upgradeCmd)

	rootCmd.AddCommand(secretsCmd)
	rootCmd.AddCommand(certsCmd)
	rootCmd.AddCommand(edgeCmd)
	rootCmd.AddCommand(versionCmd)

	// Make "versola --version" print exactly what "versola version"
	// prints. Without this, cobra uses its own default template ("versola
	// version <x>") and the two spellings of the same question answer it
	// differently.
	rootCmd.SetVersionTemplate(versionString() + "\n")

	// --help-all and the template that shows hidden flags: see help.go.
	setupHelp(rootCmd)
}
