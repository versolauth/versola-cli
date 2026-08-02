package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// version is the versola-cli release this binary was built from. It's
// overwritten at build time via -ldflags "-X .../internal/cmd.version=v1.2.3"
// (see .github/workflows/release.yml). Builds made straight from source
// without that flag report "dev", which is accurate: they don't correspond
// to any published release.
//
// Note this is the CLI's OWN version, not the Versola version it deploys --
// those are deliberately independent, since one CLI build can bootstrap many
// different Versola releases via the argument to "bootstrap local".
var version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the versola-cli version",
	Long: `version prints which versola-cli release this binary was built from,
along with the Go version and platform it was built for.

This is the CLI's own version, not the version of Versola it deploys --
that one is whatever you pass to "versola bootstrap local <version>".`,
	RunE: runVersion,
}

// versionString is the single source of the version output, shared by the
// "version" subcommand and the "--version" flag so the two can't drift.
func versionString() string {
	return fmt.Sprintf("versola-cli %s (%s, %s/%s)",
		version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func runVersion(cmd *cobra.Command, args []string) error {
	fmt.Println(versionString())
	return nil
}
