package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what's currently deployed and its health",
	Long:  `status is not implemented yet.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("status is not implemented yet")
	},
}
