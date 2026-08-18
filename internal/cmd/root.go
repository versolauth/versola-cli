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
	// Setting this makes cobra add a "--version" flag automatically, so
	// both "versola version" and "versola --version" work -- people reach
	// for either, and a support question always starts with "what version
	// are you on?".
	Version: version,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(bootstrapCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(uninstallCmd)
	rootCmd.AddCommand(upgradeCmd)
	rootCmd.AddCommand(secretsCmd)
	rootCmd.AddCommand(versionCmd)

	// Make "versola --version" print exactly what "versola version"
	// prints. Without this, cobra uses its own default template ("versola
	// version <x>") and the two spellings of the same question answer it
	// differently.
	rootCmd.SetVersionTemplate(versionString() + "\n")
}

// Execute runs the root command. Called once from main.main.
func Execute() error {
	return rootCmd.Execute()
}
