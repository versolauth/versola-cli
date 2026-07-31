package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap <target> <version>",
	Short: "Deploy a specific version of Versola",
	Long: `bootstrap deploys a specific released version of Versola.

Currently planned:

  versola bootstrap local 0.1.1

Not implemented yet. See the project design doc, section 3.2, for the
planned flow: detect, deps, preflight, tools, render, pull, up, verify.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("bootstrap is not implemented yet")
	},
}
