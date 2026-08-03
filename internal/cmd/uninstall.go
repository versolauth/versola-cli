package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/state"
)

var assumeYes bool

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove everything versola deployed locally",
	Long: `uninstall stops the locally deployed Versola stack (including its
Postgres data volume), removes the Docker images versola pulled, and
clears ~/.versola.

If a deployment was recorded but Docker isn't reachable to confirm it's
actually stopped, ~/.versola is deliberately left in place instead of
cleared -- deleting it there would destroy the only way to properly stop
that deployment later, if it turns out to still be running somewhere
uninstall couldn't reach.

It does NOT remove the versola binary itself or its PATH entry — safely
deleting a program's own currently-running executable isn't portable
across platforms (Windows in particular won't allow it). uninstall prints
the binary's path so you can remove it yourself.`,
	RunE: runUninstall,
}

func init() {
	uninstallCmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "don't prompt for confirmation")
}

func runUninstall(cmd *cobra.Command, args []string) error {
	composePath, deployed, err := state.ComposeFile()
	if err != nil {
		return err
	}

	// If the daemon isn't reachable at all, there's nothing "docker compose
	// down" could stop even if a deployment was recorded -- Docker being
	// down means nothing is running, full stop. Skip that step instead of
	// letting it hard-fail the whole command and leave ~/.versola behind.
	dockerUp := checks.DockerDaemon().OK
	stopStack := deployed && dockerUp

	// Only ask Docker what images exist if the daemon is actually
	// reachable -- calling it while dockerUp is false would just fail with
	// the same unreachable-daemon error `versola doctor` already covers.
	// imagesUnknown tracks that we genuinely don't know either way, which
	// is a different claim than "checked, found none" -- conflating the
	// two either risks a false "nothing to remove" (images could still be
	// sitting there) or silently reporting success without actually
	// checking.
	var images []string
	imagesUnknown := !dockerUp
	if dockerUp {
		images, err = versolaImages()
		if err != nil {
			return err
		}
	}

	stateDir, err := state.Dir()
	if err != nil {
		return err
	}
	// state.Dir() returns .../.versola/active; uninstall clears the whole
	// .versola directory, not just the active deployment, since nothing
	// else is meant to live there.
	versolaDir := filepath.Dir(stateDir)
	_, statErr := os.Stat(versolaDir)
	if statErr != nil && !os.IsNotExist(statErr) {
		// Something other than "doesn't exist" -- e.g. a permissions
		// error -- shouldn't be silently treated as "nothing here to
		// remove"; that could leave real state behind while uninstall
		// reports success.
		return fmt.Errorf("couldn't check %s: %w", versolaDir, statErr)
	}
	dirExists := statErr == nil

	// A deployment was recorded but we couldn't confirm via Docker that
	// it's actually stopped (stopStack is false while deployed is true).
	// Deleting compose.yml here would destroy the only handle left to
	// `docker compose down` it properly later -- if the stack is really
	// still running somewhere reachable (e.g. this user just lacks
	// permission to the daemon socket, which is a very different
	// situation from "Docker isn't running at all"), that leaves
	// containers and the Postgres volume orphaned with no record of what
	// created them. Keep the directory in that case instead of guessing.
	keepDirForLaterCleanup := deployed && !stopStack && dirExists

	if !deployed && !dirExists {
		if imagesUnknown {
			fmt.Println("No deployed stack and no local state, but Docker isn't reachable, so leftover versola images couldn't be checked.")
			fmt.Println("Make sure Docker is running (see `versola doctor` if it isn't obvious why) and run `versola uninstall` again to check for those.")
			return nil
		}
		if len(images) == 0 {
			fmt.Println("Nothing to remove — no deployed stack, no versola images, no local state.")
			return nil
		}
	}

	fmt.Println("This will remove:")
	if stopStack {
		fmt.Println("  - the running Versola stack and its Postgres data volume")
	} else if deployed {
		fmt.Println("  - recorded deployment state (Docker isn't reachable, so it can't be confirmed as stopped)")
	}
	if imagesUnknown {
		fmt.Println("  - (couldn't check for versola images — Docker isn't reachable; run uninstall again once it is to catch any)")
	} else {
		for _, img := range images {
			fmt.Printf("  - image %s\n", img)
		}
	}
	if dirExists {
		if keepDirForLaterCleanup {
			fmt.Printf("  - (keeping %s — Docker isn't reachable, so there's no way to confirm the stack is actually stopped)\n", versolaDir)
		} else {
			fmt.Printf("  - %s\n", versolaDir)
		}
	}

	if !assumeYes && !confirm("Continue?") {
		fmt.Println("Aborted.")
		return nil
	}

	if stopStack {
		fmt.Println("Stopping stack and removing volumes...")
		if err := runDocker("compose", "-f", composePath, "down", "--volumes"); err != nil {
			return fmt.Errorf("docker compose down failed: %w", err)
		}
	} else if deployed {
		fmt.Println("Docker isn't reachable — skipping docker compose down, and leaving ~/.versola in place so it can still be stopped properly once Docker's reachable again.")
	}

	for _, img := range images {
		fmt.Printf("Removing image %s...\n", img)
		if err := runDocker("rmi", img); err != nil {
			// Not fatal -- an image still referenced by something else, or
			// one already removed by hand, shouldn't stop the rest of the
			// cleanup.
			fmt.Printf("  (couldn't remove %s: %v)\n", img, err)
		}
	}

	if dirExists && !keepDirForLaterCleanup {
		fmt.Printf("Removing %s...\n", versolaDir)
		if err := os.RemoveAll(versolaDir); err != nil {
			return fmt.Errorf("couldn't remove %s: %w", versolaDir, err)
		}
	}

	fmt.Println("\nDone. The versola binary itself is untouched.")
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("Delete it yourself if you're done with it: %s\n", exe)
	}
	return nil
}

// versolaImages lists locally pulled images matching Versola's own
// repositories (versola-auth, versola-central, etc. under
// ghcr.io/versolauth/versola-*) — not every image on the machine, and not
// third-party images bootstrap also pulls (postgres) that the user almost
// certainly has other uses for.
//
// Callers should only invoke this once they know the Docker daemon is
// reachable (see runUninstall's dockerUp check) -- an unreachable daemon
// isn't this function's job to diagnose, and folding it into "no images
// found" here made that failure indistinguishable from any other real
// error `docker images` could return.
func versolaImages() ([]string, error) {
	out, err := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}",
		"--filter", "reference=ghcr.io/versolauth/versola-*").Output()
	if err != nil {
		// Output() populates ExitError.Stderr (since Stderr wasn't set to
		// anything else above), but *exec.ExitError.Error() itself is just
		// "exit status 1" -- the useful part is in that Stderr field, which
		// gets lost if it's not pulled out and included explicitly here.
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("docker images failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("docker images failed: %w", err)
	}
	var images []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			images = append(images, line)
		}
	}
	return images, nil
}

// confirm prompts for a yes/no answer. readLine/yes live in doctor.go —
// shared here rather than duplicated, since this is the same "ask before
// doing something irreversible" pattern doctor's install prompts use.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	return yes(readLine())
}
