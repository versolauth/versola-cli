package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

var assumeYes bool

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop the stack and remove its data, images, and local state",
	Long: `uninstall stops the locally deployed Versola stack (including its
Postgres data volume), removes the Docker images versola pulled, and
clears ~/.versola/active.

~/.versola/openbao is left untouched -- it holds AppRole credentials for
every target this machine has logged into (see "versola secrets login"),
and uninstalling one target's stack shouldn't discard another target's
access.

If a deployment was recorded but Docker isn't reachable to confirm it's
actually stopped, ~/.versola/active is deliberately left in place instead
of cleared -- deleting it there would destroy the only way to properly
stop that deployment later, if it turns out to still be running somewhere
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
	// Held for this whole run -- see state.Lock's own comment. Without it,
	// a concurrent `configure`/`up` could be actively reading from or
	// writing into stateDir while the os.RemoveAll below runs, or could
	// finalize a fresh deployment into it right after this reads
	// composePath/target but before it acts on them (flagged in review).
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	composePath, deployed, err := state.ComposeFile()
	if err != nil {
		return err
	}

	// Needed below to decide whether it's safe to also remove OpenBao's
	// data volume (see the comment on that) -- errors here are treated the
	// same way ComposeFile already treats them internally: no state means
	// nothing to know a target for, not a reason to fail uninstall itself.
	var target string
	if st, loadErr := state.Load(); loadErr == nil {
		target = st.Target
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

	// state.Dir() returns .../.versola/active -- that's what gets removed
	// below, NOT its parent .../.versola. .../.versola/openbao also lives
	// under there, holding AppRole credentials for every target this
	// machine has ever logged into (see openbao.Credentials' own comment:
	// configuring local after vps, or back again, must not discard either
	// target's access) -- deleting the whole parent would wipe every
	// target's credentials just because the active one is being torn
	// down, defeating that entirely. Removing only "active" leaves
	// credentials exactly where "versola secrets login" put them.
	stateDir, err := state.Dir()
	if err != nil {
		return err
	}
	_, statErr := os.Stat(stateDir)
	if statErr != nil && !os.IsNotExist(statErr) {
		// Something other than "doesn't exist" -- e.g. a permissions
		// error -- shouldn't be silently treated as "nothing here to
		// remove"; that could leave real state behind while uninstall
		// reports success.
		return fmt.Errorf("couldn't check %s: %w", stateDir, statErr)
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
		if target == "local" {
			fmt.Println("  - the running Versola stack, its Postgres data volume, and OpenBao's secrets volume")
		} else {
			fmt.Println("  - the running Versola stack (OpenBao's secrets volume is left in place — see below)")
		}
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
			fmt.Printf("  - (keeping %s — Docker isn't reachable, so there's no way to confirm the stack is actually stopped)\n", stateDir)
		} else {
			fmt.Printf("  - %s\n", stateDir)
		}
	}

	if !assumeYes && !confirm("Continue?") {
		fmt.Println("Aborted.")
		return nil
	}

	if stopStack {
		fmt.Println("Stopping stack and removing volumes...")
		if err := docker.Run("compose", "-f", composePath, "down", "--volumes"); err != nil {
			return fmt.Errorf("docker compose down failed: %w", err)
		}

		// `down --volumes` only removes volumes Compose itself owns --
		// OpenbaoVolumeName's volume is declared `external: true` (see
		// compose.fragment.yml.template's comment on why: so it survives
		// being reconfigured into a fresh bundle directory), which means
		// Compose never removes it either, uninstall included. Left alone,
		// a later fresh install would silently reuse this "uninstalled"
		// deployment's OpenBao data and every secret already resolved into
		// it -- surprising for a command whose whole job is a clean slate.
		//
		// Only for local: local and vps each get their own distinctly
		// named volume (see OpenbaoVolumeName -- this used to be a single
		// shared name across both targets, flagged in review as a risk if
		// they ever ran under the same Docker daemon), but vps's still
		// holds the real secrets (AppRole credentials, resolved Postgres
		// password, etc.) -- deleting those should stay a deliberate
		// decision someone makes on purpose, not a side effect of running
		// this general cleanup command.
		if target == "local" {
			fmt.Println("Removing OpenBao's data volume...")
			volumeName := deploy.OpenbaoVolumeName(target)
			if err := docker.Run("volume", "rm", volumeName); err != nil {
				// Not fatal -- same reasoning as the image removal loop
				// below: it might already be gone, or held by something else.
				fmt.Printf("  (couldn't remove %s: %v)\n", volumeName, err)
			}
		} else if target == "vps" {
			volumeName := deploy.OpenbaoVolumeName(target)
			fmt.Printf("Leaving OpenBao's data volume in place (vps target — remove it yourself with `docker volume rm %s` if you really mean to discard it).\n", volumeName)
		}
	} else if deployed {
		fmt.Println("Docker isn't reachable — skipping docker compose down, and leaving ~/.versola in place so it can still be stopped properly once Docker's reachable again.")
	}

	// Independent of everything above: Configure starts openbao under its
	// (target-specific, see deploy.OpenbaoContainerName) container_name
	// *before* anything gets recorded to state.json (deliberately -- see
	// state.Finalize's comment, and the develop.md step that expects the
	// very first `configure vps` to fail once OpenBao is up but before
	// credentials exist). If that run never reaches a successful
	// Configure, this container ends up running with no compose.yml or
	// state.json anywhere pointing at it -- deployed stays false,
	// stopStack above never runs, and this uninstall would otherwise
	// silently leave it running forever. Only the container is touched
	// here, never its volume: an orphan like this could belong to either
	// target, and guessing which one owns the volume is exactly the kind
	// of decision uninstall shouldn't make on someone's behalf.
	//
	// Checked for both targets' container names, not just one -- there's
	// no tracked state to say which target this orphan belongs to (that's
	// the whole reason it's an orphan), and the container name is
	// target-specific now, so a single fixed name can't catch both cases
	// the way it used to.
	if dockerUp {
		for _, t := range []string{"local", "vps"} {
			name := deploy.OpenbaoContainerName(t)
			if running, err := docker.IsRunning(name); err == nil && running {
				fmt.Printf("Found an OpenBao container not tied to any tracked deployment (%s, likely left over from an incomplete configure) — stopping it...\n", name)
				if err := docker.Run("rm", "-f", name); err != nil {
					fmt.Printf("  (couldn't remove %s: %v)\n", name, err)
				}
			}
		}
	}

	for _, img := range images {
		fmt.Printf("Removing image %s...\n", img)
		if err := docker.Run("rmi", img); err != nil {
			// Not fatal -- an image still referenced by something else, or
			// one already removed by hand, shouldn't stop the rest of the
			// cleanup.
			fmt.Printf("  (couldn't remove %s: %v)\n", img, err)
		}
	}

	if dirExists && !keepDirForLaterCleanup {
		fmt.Printf("Removing %s...\n", stateDir)
		if err := os.RemoveAll(stateDir); err != nil {
			return fmt.Errorf("couldn't remove %s: %w", stateDir, err)
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
