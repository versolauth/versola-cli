package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/checks"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that this machine has everything Versola needs",
	Long: `doctor checks this machine for the dependencies Versola needs to run
locally: a reachable Docker daemon, the Docker Compose plugin, and a
free port for the gateway. It never installs anything on its own -- if
Docker isn't found at all, the most it does is offer to open an install
page in your browser, and only after you confirm.

Run this before "versola bootstrap local <version>" to see ahead of
time what's missing.`,
	RunE: runDoctor,
}

func runDoctor(cmd *cobra.Command, args []string) error {
	dockerDaemon := checks.DockerDaemon()
	results := []checks.Result{
		dockerDaemon,
		checks.ComposePlugin(),
		checks.PortFree(2821, "versola-nginx"),
		checks.DockerMemory(),
		checks.DiskSpace(),
	}

	failed := 0
	for _, r := range results {
		fmt.Println(r.String())
		if !r.OK {
			failed++
		}
	}

	if !dockerDaemon.OK {
		offerDockerHelp()
	}

	fmt.Println()
	if failed == 0 {
		fmt.Println("All checks passed — ready for `versola bootstrap local <version>`.")
		return nil
	}

	fmt.Printf("%d check(s) failed. Fix the issue(s) above before running bootstrap.\n", failed)
	return fmt.Errorf("doctor found %d problem(s)", failed)
}

// offerDockerHelp runs when the Docker daemon check failed. It distinguishes
// two different situations that look identical to DockerDaemon() but need
// different advice:
//
//   - docker isn't on PATH at all -> nothing is installed, so this offers to
//     open a download page (a choice of two, on macOS).
//   - docker IS on PATH but the daemon didn't answer -> it's installed but
//     not running, so "go install something" would be actively wrong advice;
//     this just says to start it.
//
// Never installs or downloads anything itself — only opens a browser tab a
// human then acts on, consistent with every other check in this package
// only reading state, never changing it.
func offerDockerHelp() {
	if path, err := exec.LookPath("docker"); err == nil {
		fmt.Println()
		fmt.Println(daemonUnreachableHint(path))
		return
	}

	fmt.Println("\nDocker doesn't seem to be installed.")

	switch runtime.GOOS {
	case "darwin":
		offerChoice(
			"Which would you like to install?",
			[2]choiceOption{
				{"Docker Desktop", "the standard, full-featured option", "https://www.docker.com/products/docker-desktop/"},
				{"Colima", "lightweight, CLI-only, no GUI/license considerations", "https://github.com/abiosoft/colima#installation"},
			},
		)
	case "windows":
		promptOpen("Open the Docker Desktop download page?", "https://www.docker.com/products/docker-desktop/")
	case "linux":
		promptOpen("Open the Docker Engine install instructions?", "https://docs.docker.com/engine/install/")
	default:
		fmt.Println("Install Docker for your platform: https://docs.docker.com/get-started/get-docker/")
	}
}

// daemonUnreachableHint runs `docker version` itself -- checks.DockerDaemon()
// already told us this failed, but not why, and "start Docker Desktop" is
// actively wrong advice for the most common Linux failure mode: the
// invoking user isn't in the `docker` group, so the daemon is running
// fine but the socket refuses this user specifically. That shows up as
// "permission denied" in docker's own output, which is what this checks
// for before falling back to the generic "it's not running" hint.
func daemonUnreachableHint(dockerPath string) string {
	// Same 5s bound as checks.DockerDaemon() -- that check already failed
	// within that window, so this diagnostic re-check shouldn't be able to
	// hang longer than the thing it's explaining, e.g. against a
	// misconfigured remote DOCKER_HOST that never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, dockerPath, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if strings.Contains(strings.ToLower(string(out)), "permission denied") {
		return "Docker is installed, but this user doesn't have permission to talk to it " +
			"(the daemon socket is normally root:docker, and group membership needs a fresh " +
			"login to take effect). Try: sudo usermod -aG docker $USER, then log out and back " +
			"in. See https://docs.docker.com/engine/install/linux-postinstall/"
	}
	return "Docker is installed but the daemon isn't responding — start Docker Desktop (or Colima, if that's what you use) and try again."
}

type choiceOption struct {
	name string
	desc string
	url  string
}

func offerChoice(prompt string, options [2]choiceOption) {
	fmt.Println(prompt)
	for i, o := range options {
		fmt.Printf("  %d. %s — %s: %s\n", i+1, o.name, o.desc, o.url)
	}

	// doctor is otherwise a pure read-only diagnostic -- it shouldn't hang
	// waiting for a keypress that will never come when stdin isn't a
	// terminal (a scripted/CI run, e.g.). Print the choices with their
	// URLs and return instead of blocking on ReadString.
	if !stdinIsInteractive() {
		return
	}

	fmt.Print("Open a download page in your browser? [1/2/N]: ")

	answer := readLine()
	var chosen *choiceOption
	switch answer {
	case "1":
		chosen = &options[0]
	case "2":
		chosen = &options[1]
	default:
		return
	}

	if err := browser.Open(chosen.url); err != nil {
		fmt.Printf("Couldn't open a browser: %v — visit %s yourself.\n", err, chosen.url)
		return
	}
	fmt.Printf("Opened %s in your browser.\n", chosen.url)
}

func promptOpen(prompt, url string) {
	if !stdinIsInteractive() {
		fmt.Printf("%s %s\n", prompt, url)
		return
	}

	fmt.Printf("%s [y/N]: ", prompt)
	if !yes(readLine()) {
		return
	}
	if err := browser.Open(url); err != nil {
		fmt.Printf("Couldn't open a browser: %v — visit %s yourself.\n", err, url)
		return
	}
	fmt.Printf("Opened %s in your browser.\n", url)
}

// stdinIsInteractive reports whether stdin looks like a real terminal
// rather than a pipe, a redirected file, or /dev/null -- the standard
// character-device check, no extra dependency needed. Used to skip
// doctor's install-page prompts (offerChoice/promptOpen) when nothing is
// there to answer them, so a scripted or CI run of a nominally read-only
// diagnostic command can't hang waiting on a keypress that will never
// come. uninstall's confirm() deliberately doesn't use this -- it already
// has an explicit --yes flag as its non-interactive escape hatch, which a
// destructive command should require opting into rather than silently
// inferring from the environment.
func stdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func readLine() string {
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func yes(answer string) bool {
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes"
}
