package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

var (
	assumeYes      bool
	uninstallPurge bool
)

// Docker as uninstall sees it; the tests replace these.
var (
	uninstallDockerUp        = func() bool { return checks.DockerDaemon().OK }
	uninstallDocker          = docker.Run
	uninstallImages          = versolaImages
	uninstallIsRunning       = docker.IsRunning
	uninstallContainerExists = func(name string) (bool, error) {
		_, found, err := docker.Inspect(name, "{{.Name}}")
		return found, err
	}
	// uninstallVolumeExists lists volumes by name rather than inspecting one:
	// an empty answer is "no such volume", and an error is only ever Docker
	// failing to answer -- which the purge must not read as "nothing here".
	uninstallVolumeExists = func(name string) (bool, error) {
		out, err := docker.Output("volume", "ls", "--quiet", "--filter", "name="+name)
		if err != nil {
			return false, fmt.Errorf("couldn't ask Docker whether the volume %s exists: %w", name, err)
		}
		// The filter matches a part of the name: compare whole lines.
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == name {
				return true, nil
			}
		}
		return false, nil
	}
)

// volumeShown is uninstallVolumeExists for what is only printed: a volume
// Docker could not be asked about is just not named.
func volumeShown(name string) bool {
	ok, err := uninstallVolumeExists(name)
	return err == nil && ok
}

// purgeWord is what --purge asks to be typed. The target's name: the purge is
// only ever about the vps one, and typing it is a decision, not a reflex.
const purgeWord = "vps"

var uninstallCmd = &cobra.Command{
	Use:     "uninstall",
	GroupID: groupOperate,
	Short:   "Stop the stack and remove its data, images, and local state",
	Long: `uninstall stops the locally deployed Versola stack (including its
Postgres data volume), removes the Docker images versola pulled, and
clears ~/.versola/active.

On vps, OpenBao's data volume (every secret and signing key), the
Postgres data volume when Postgres runs in a container, and the TLS
certificate volume are kept: they are a live server's data. To start
over from scratch, after a failed first attempt for example:

  versola uninstall --purge

--purge also destroys OpenBao's and Postgres' vps volumes and this
machine's OpenBao credentials for vps (~/.versola/openbao/vps*), after
you type "vps"; --yes does not answer that question. The TLS
certificate volume is kept either way (Let's Encrypt rate limits). A
Postgres of your own (--postgres-host) is never touched.

~/.versola/openbao is otherwise left untouched -- it holds AppRole
credentials for every target this machine has logged into (see "versola
secrets login"), and uninstalling one target's stack shouldn't discard
another target's access.

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
	uninstallCmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "don't prompt for confirmation (does not answer --purge's question)")
	uninstallCmd.Flags().BoolVar(&uninstallPurge, "purge", false, `vps only: also destroy OpenBao's and Postgres' data volumes and the vps OpenBao credentials, to start over from scratch (asks you to type "vps")`)
}

// purgeSet is what --purge would destroy on this machine: only what exists.
type purgeSet struct {
	openbaoVolume  bool     // deploy.OpenbaoVolumeName("vps") exists
	postgresVolume bool     // deploy.PostgresVolumeName() exists
	files          []string // the existing ones of openbao.TargetFiles("vps")
	// Versola's vps containers that hold those volumes, as found before the
	// question: shown in it, because removing a running one stops a service.
	// runPurge looks again when it acts (`down` may have removed them).
	containers []string
	running    []string
}

func (p purgeSet) empty() bool {
	return !p.openbaoVolume && !p.postgresVolume && len(p.files) == 0
}

func findPurgeSet() (purgeSet, error) {
	var p purgeSet
	var err error
	// Not knowing whether a volume is there must not read as "nothing to
	// remove" either (see the files below).
	if p.openbaoVolume, err = uninstallVolumeExists(deploy.OpenbaoVolumeName("vps")); err != nil {
		return p, fmt.Errorf("%w\nNothing was removed", err)
	}
	if p.postgresVolume, err = uninstallVolumeExists(deploy.PostgresVolumeName()); err != nil {
		return p, fmt.Errorf("%w\nNothing was removed", err)
	}
	for _, name := range []string{deploy.OpenbaoContainerName("vps"), deploy.PostgresContainerName()} {
		exists, err := uninstallContainerExists(name)
		if err != nil {
			return p, fmt.Errorf("%w\nNothing was removed", err)
		}
		if !exists {
			continue
		}
		p.containers = append(p.containers, name)
		if running, err := uninstallIsRunning(name); err == nil && running {
			p.running = append(p.running, name)
		}
	}
	files, err := openbao.TargetFiles("vps")
	if err != nil {
		return p, err
	}
	for _, f := range files {
		_, err := os.Lstat(f)
		if err == nil {
			p.files = append(p.files, f)
			continue
		}
		if !os.IsNotExist(err) {
			// Not knowing whether a credentials file is there must not read as
			// "nothing to remove": the purge would then report a fresh host
			// that is not one.
			return p, fmt.Errorf("couldn't check %s: %w", f, err)
		}
	}
	return p, nil
}

// keptVpsVolumes are the vps volumes a plain uninstall leaves on this
// machine, by name, with what each one holds.
func keptVpsVolumes() []string {
	var kept []string
	if volumeShown(deploy.OpenbaoVolumeName("vps")) {
		kept = append(kept, deploy.OpenbaoVolumeName("vps")+" (OpenBao: every secret and signing key)")
	}
	if volumeShown(deploy.PostgresVolumeName()) {
		kept = append(kept, deploy.PostgresVolumeName()+" (Postgres: the database)")
	}
	if volumeShown(proxy.ACMEVolume) {
		kept = append(kept, proxy.ACMEVolume+" (TLS certificates)")
	}
	return kept
}

func printKeptVolumes(kept []string) {
	if len(kept) == 0 {
		return
	}
	fmt.Println("\nKept on this server (a live server's data):")
	for _, k := range kept {
		fmt.Printf("  - %s\n", k)
	}
	fmt.Println("To start over from scratch: versola uninstall --purge")
}

// confirmPurge asks for purgeWord to be typed. assumeYes does not answer it:
// the purge destroys what no backup taken by this CLI can bring back, so a
// script has to say so on stdin ("vps"), not just with -y.
func confirmPurge(in io.Reader) error {
	if assumeYes {
		fmt.Println("(--yes does not answer this question.)")
	}
	fmt.Printf("Type %q to destroy this for good: ", purgeWord)
	line, err := deploy.ReadLine(in)
	if err != nil && !errors.Is(err, io.EOF) {
		return exitcode.With(exitcode.Aborted, fmt.Errorf("aborted: nothing was removed: %w", err))
	}
	if strings.TrimSpace(line) != purgeWord {
		return exitcode.With(exitcode.Aborted, fmt.Errorf("aborted: nothing was removed (the purge needs %q typed)", purgeWord))
	}
	return nil
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
	var target, registry string
	// The record says the vps deployment uses a Postgres of its own
	// (--postgres-host): the purge makes what Versola stored there unreadable,
	// and says so even if a bundled volume from an earlier attempt is here too.
	ownPostgres := false
	if st, loadErr := state.Load(); loadErr == nil {
		target = st.Target
		registry = st.ImageRegistry
		ownPostgres = st.Target == "vps" && st.PostgresMode != deploy.PostgresModeBundled
	}

	// --purge is about vps only. With no record at all it still applies: the
	// usual way to need it is a plain uninstall that already removed the
	// record, then a configure refusing the OpenBao volume it left behind
	// ("unknown-state"). A local record is refused instead of purging the vps
	// leftovers next to it: a plain uninstall of local already removes
	// local's own volumes, and a vps volume beside a local record is not
	// something this machine's record says anything about.
	if uninstallPurge && target == "local" {
		return exitcode.With(exitcode.Usage, fmt.Errorf("--purge is for vps: the deployment recorded here is local, and a plain `versola uninstall` already removes its OpenBao and Postgres data"))
	}

	// If the daemon isn't reachable at all, there's nothing "docker compose
	// down" could stop even if a deployment was recorded -- Docker being
	// down means nothing is running, full stop. Skip that step instead of
	// letting it hard-fail the whole command and leave ~/.versola behind.
	dockerUp := uninstallDockerUp()
	stopStack := deployed && dockerUp

	if uninstallPurge && !dockerUp {
		// Removing the credentials without the volume would leave OpenBao's
		// secrets in place with nothing on this machine able to unseal or
		// read them: the purge is all or nothing, and the volumes need Docker.
		return fmt.Errorf("--purge needs Docker running to remove the volumes: nothing was removed. Start Docker (see `versola doctor`) and run it again")
	}

	var purge purgeSet
	if uninstallPurge {
		if purge, err = findPurgeSet(); err != nil {
			return err
		}
	}

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
		images, err = uninstallImages(registry)
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
	// credentials exactly where "versola secrets login" put them. --purge
	// removes vps's files there by name, never the directory.
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

	// What a plain uninstall leaves of vps, named at the end with the one
	// command that removes it. Not for a local record (see --purge above).
	var kept []string
	if !uninstallPurge && dockerUp && target != "local" {
		kept = keptVpsVolumes()
	}

	if !deployed && !dirExists && purge.empty() {
		if imagesUnknown {
			fmt.Println("No deployed stack and no local state, but Docker isn't reachable, so leftover versola images couldn't be checked.")
			fmt.Println("Make sure Docker is running (see `versola doctor` if it isn't obvious why) and run `versola uninstall` again to check for those.")
			return nil
		}
		if len(images) == 0 {
			if len(kept) > 0 {
				fmt.Println("No deployed stack, no versola images, no local state.")
				printKeptVolumes(kept)
				return nil
			}
			fmt.Println("Nothing to remove — no deployed stack, no versola images, no local state.")
			return nil
		}
	}

	fmt.Println("This will remove:")
	if stopStack {
		switch {
		case target == "local":
			fmt.Println("  - the running Versola stack, its Postgres data volume, and OpenBao's secrets volume")
		case uninstallPurge:
			fmt.Println("  - the running Versola stack")
		default:
			fmt.Println("  - the running Versola stack (its data volumes are kept — see the end)")
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
	if uninstallPurge {
		// Names only: these are the files that hold the root token, the
		// unseal key and the AppRole secret, and none of them is read here.
		if purge.empty() {
			fmt.Println("\n--purge: no vps OpenBao or Postgres volume and no vps OpenBao credentials on this machine, nothing more to destroy.")
		} else {
			fmt.Println("\nAnd DESTROY for good (--purge; no backup is taken):")
			if purge.openbaoVolume {
				fmt.Printf("  - volume %s: every secret and signing key in OpenBao; data encrypted with them can never be read again\n", deploy.OpenbaoVolumeName("vps"))
			}
			if purge.postgresVolume {
				fmt.Printf("  - volume %s: the database (every user, client and session)\n", deploy.PostgresVolumeName())
			}
			for _, f := range purge.files {
				fmt.Printf("  - %s\n", f)
			}
			for _, c := range purge.containers {
				how := "stopped"
				if slices.Contains(purge.running, c) {
					how = "RUNNING: it is stopped"
				}
				fmt.Printf("  - container %s (%s and removed first)\n", c, how)
			}
			if len(purge.running) > 0 && target == "" {
				// Versola running with no record of it on this machine is what
				// a live server whose ~/.versola was lost looks like, not only
				// a failed first attempt.
				fmt.Println("Versola's containers are running here, but this machine has no record of a deployment. If this server serves real users and ~/.versola was lost, do NOT purge: the keys protect their data.")
			}
		}
		if volumeShown(proxy.ACMEVolume) {
			fmt.Printf("Kept: %s (TLS certificates; reusing them avoids Let's Encrypt's rate limits).\n", proxy.ACMEVolume)
		}
		if ownPostgres || !purge.postgresVolume {
			fmt.Println("A Postgres of your own (--postgres-host), if this deployment used one, is not touched: what Versola stored in it is encrypted with the keys above, so recreate its schemas before configuring again.")
		}
	}

	if uninstallPurge && !purge.empty() {
		if err := confirmPurge(os.Stdin); err != nil {
			return err
		}
	} else if !assumeYes && !confirm("Continue?") {
		// Exit code 4 (exitcode.Aborted), as for every other "no" at a
		// confirmation: it used to be 0, which told a script that ran
		// `versola uninstall && <reinstall>` with nothing on stdin that the
		// deployment was gone when nothing was removed. "Aborted." stays the
		// one line printed (Reported keeps Run from adding "aborted" after it).
		fmt.Println("Aborted.")
		return exitcode.Reported(exitcode.Aborted, errors.New("aborted"))
	}

	if stopStack {
		fmt.Println("Stopping stack and removing volumes...")
		if err := uninstallDocker(state.ComposeArgs(composePath, "down", "--volumes")...); err != nil {
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
		// password, etc.) -- deleting those stays a deliberate decision,
		// --purge with its typed confirmation, not a side effect of
		// running this general cleanup command.
		if target == "local" {
			fmt.Println("Removing OpenBao's data volume...")
			volumeName := deploy.OpenbaoVolumeName(target)
			if err := uninstallDocker("volume", "rm", volumeName); err != nil {
				// Not fatal -- same reasoning as the image removal loop
				// below: it might already be gone, or held by something else.
				fmt.Printf("  (couldn't remove %s: %v)\n", volumeName, err)
			}
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
	// of decision uninstall shouldn't make on someone's behalf (--purge
	// removes vps's below, because it was asked to).
	//
	// Checked for both targets' container names, not just one -- there's
	// no tracked state to say which target this orphan belongs to (that's
	// the whole reason it's an orphan), and the container name is
	// target-specific now, so a single fixed name can't catch both cases
	// the way it used to. Before the purge: a container still using
	// OpenBao's volume would make `docker volume rm` fail.
	if dockerUp {
		for _, t := range []string{"local", "vps"} {
			name := deploy.OpenbaoContainerName(t)
			if running, err := uninstallIsRunning(name); err == nil && running {
				fmt.Printf("Found an OpenBao container not tied to any tracked deployment (%s, likely left over from an incomplete configure) — stopping it...\n", name)
				if err := uninstallDocker("rm", "-f", name); err != nil {
					fmt.Printf("  (couldn't remove %s: %v)\n", name, err)
				}
			}
		}
	}

	if uninstallPurge && !purge.empty() {
		if err := runPurge(purge); err != nil {
			return err
		}
	}

	for _, img := range images {
		fmt.Printf("Removing image %s...\n", img)
		if err := uninstallDocker("rmi", img); err != nil {
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

	printKeptVolumes(kept)
	if uninstallPurge {
		fmt.Println("\nThis server holds no Versola secrets or data any more: `versola configure vps ...` (or bootstrap) starts a first install.")
	}
	fmt.Println("\nDone. The versola binary itself is untouched.")
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("Delete it yourself if you're done with it: %s\n", exe)
	}
	return nil
}

// runPurge destroys what p lists, in an order that never leaves OpenBao's
// secrets without the credentials to open them: OpenBao's volume first, and
// the credential files only once it is gone. A failure stops there with
// nothing after it removed, and running `uninstall --purge` again picks up
// what is left (it works with no record).
func runPurge(p purgeSet) error {
	// A container holds its volume even when stopped (a reboot after a failed
	// first attempt leaves OpenBao's like that), and `docker volume rm` then
	// fails: the orphan loop above only removes running ones. Both of
	// Versola's vps containers whose volumes are destroyed here go first,
	// running or not; with nothing destroyed yet, a failure is safe to rerun.
	for _, name := range []string{deploy.OpenbaoContainerName("vps"), deploy.PostgresContainerName()} {
		exists, err := uninstallContainerExists(name)
		if err != nil {
			return fmt.Errorf("%w\nNothing was destroyed: run `versola uninstall --purge` again once Docker answers", err)
		}
		if !exists {
			continue
		}
		fmt.Printf("Removing container %s...\n", name)
		if err := uninstallDocker("rm", "-f", name); err != nil {
			return fmt.Errorf("couldn't remove the container %s: %w\nNothing was destroyed: run `versola uninstall --purge` again", name, err)
		}
	}
	if p.openbaoVolume {
		name := deploy.OpenbaoVolumeName("vps")
		fmt.Printf("Destroying volume %s...\n", name)
		if err := uninstallDocker("volume", "rm", name); err != nil {
			return fmt.Errorf("couldn't remove %s: %w\nThe vps OpenBao credentials and the Postgres volume were kept. If a container that is not Versola's uses the volume, remove it, then run `versola uninstall --purge` again", name, err)
		}
	}
	for _, f := range p.files {
		fmt.Printf("Removing %s...\n", f)
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("couldn't remove %s: %w\nRun `versola uninstall --purge` again once it can be removed", f, err)
		}
	}
	if p.postgresVolume {
		name := deploy.PostgresVolumeName()
		fmt.Printf("Destroying volume %s...\n", name)
		if err := uninstallDocker("volume", "rm", name); err != nil {
			return fmt.Errorf("couldn't remove %s: %w\nIf a container that is not Versola's uses the volume, remove it, then run `versola uninstall --purge` again", name, err)
		}
	}
	return nil
}

// versolaImages lists locally pulled images matching Versola's own
// repositories (versola-auth, versola-central, etc. under
// ghcr.io/versolauth/versola-*, and the registry the deployment was
// configured with, if it was not that one) — not every image on the machine, and not
// third-party images bootstrap also pulls (postgres) that the user almost
// certainly has other uses for.
//
// Callers should only invoke this once they know the Docker daemon is
// reachable (see runUninstall's dockerUp check) -- an unreachable daemon
// isn't this function's job to diagnose, and folding it into "no images
// found" here made that failure indistinguishable from any other real
// error `docker images` could return.
func versolaImages(registry string) ([]string, error) {
	args := []string{"images", "--format", "{{.Repository}}:{{.Tag}}",
		"--filter", "reference=" + deploy.DefaultRegistry + "/versola-*"}
	if registry != "" {
		// Several reference filters match any of them.
		args = append(args, "--filter", "reference="+registry+"/versola-*")
	}
	out, err := exec.Command("docker", args...).Output()
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
