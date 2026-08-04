package cmd

import (
	"context"
	"fmt"
	"runtime"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/spf13/cobra"
)

// upgradeRepoSlug is versola-cli's own repository -- not to be confused
// with the Versola product repo that "bootstrap" pulls versola-tools
// from. This command only ever touches the CLI binary itself.
const upgradeRepoSlug = "versolauth/versola-cli"

var upgradeAssumeYes bool

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Update versola-cli to the latest release",
	Long: `upgrade checks the latest versola-cli release on GitHub, and if it's
newer than this binary, downloads it (verified against the release's
published checksums.txt) and replaces the running executable in place.

Builds made straight from source ("versola version" reports "dev") aren't
tied to any published release, so upgrade refuses to touch those --
rebuild from source instead.`,
	RunE: runUpgrade,
}

func init() {
	upgradeCmd.Flags().BoolVarP(&upgradeAssumeYes, "yes", "y", false, "don't prompt for confirmation")
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	// A "dev" build didn't come from a release, so there's no meaningful
	// version to compare against and no guarantee it's even safe to
	// overwrite (e.g. it might not be at the path a normal install would
	// use). Bail out rather than guessing.
	if version == "dev" {
		fmt.Println(`This is a "dev" build (not installed from a release) -- nothing to upgrade. Rebuild from source instead.`)
		return nil
	}

	ctx := context.Background()

	// checksums.txt is the same plain "sha256sum" file release.yml
	// already generates and install.sh/install.ps1 already verify
	// downloads against -- reusing that convention here instead of
	// inventing a second one.
	updater, err := selfupdate.NewUpdater(selfupdate.Config{
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
	})
	if err != nil {
		return fmt.Errorf("couldn't set up the updater: %w", err)
	}

	latest, found, err := updater.DetectLatest(ctx, selfupdate.ParseSlug(upgradeRepoSlug))
	if err != nil {
		return fmt.Errorf("couldn't check the latest release: %w", err)
	}
	if !found {
		return fmt.Errorf("no release found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	if latest.LessOrEqual(version) {
		fmt.Printf("Already on the latest version (%s).\n", version)
		return nil
	}

	fmt.Printf("versola-cli %s is available (you have %s).\n", latest.Version(), version)

	if !upgradeAssumeYes && !confirm("Update now?") {
		fmt.Println("Aborted.")
		return nil
	}

	exe, err := selfupdate.ExecutablePath()
	if err != nil {
		return fmt.Errorf("couldn't locate the running executable: %w", err)
	}

	fmt.Println("Downloading and verifying...")
	if err := updater.UpdateTo(ctx, latest, exe); err != nil {
		return fmt.Errorf("update failed (the previous binary should still be intact): %w", err)
	}

	fmt.Printf("Updated to %s.\n", latest.Version())
	return nil
}
