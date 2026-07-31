package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the locally deployed Versola stack",
	Long:  `down is not implemented yet. Will support --volumes to also remove data.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("down is not implemented yet")
	},
}
