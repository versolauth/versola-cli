package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

var doctorTarget, doctorAuthURL, doctorCentralURL string
var doctorMTLSDomain, doctorCentralMTLSDomain, doctorCentralDomain string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that this machine has everything Versola needs",
	Long: `doctor checks this machine for the dependencies Versola needs to run
locally: a reachable Docker daemon, the Docker Compose plugin, and a
free port for its reverse proxy. It never installs anything on its own -- if
Docker isn't found at all, the most it does is offer to open an install
page in your browser, and only after you confirm.

Run this before "versola bootstrap local <version>" to see ahead of
time what's missing. Pass --target vps when checking a real server --
some checks differ (see --target's own help): running it with no flag
against a vps machine can fail a check that "configure vps" itself
would actually pass, or pass one it would actually fail.`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().StringVar(&doctorAuthURL, "auth-url", "", "vps only: the --auth-url you will deploy with -- needed for the public name checks below")
	doctorCmd.Flags().StringVar(&doctorCentralURL, "central-url", "", "vps only: the --central-url you will deploy with -- checks that its name resolves to this server and that its port answers a TLS handshake")
	doctorCmd.Flags().StringVar(&doctorMTLSDomain, "mtls-domain", "", "vps only: the --mtls-domain you will deploy with -- checks that it resolves to this server and that 443 answers a TLS handshake for it")
	doctorCmd.Flags().StringVar(&doctorCentralMTLSDomain, "central-mtls-domain", "", "vps only: the --central-mtls-domain you will deploy with -- same checks")
	doctorCmd.Flags().StringVar(&doctorCentralDomain, "central-domain", "", "vps only: the --central-domain you will deploy with -- same checks")
	doctorCmd.Flags().StringVar(&doctorTarget, "target", "local", `which deployment target to check for ("local" or "vps") -- affects the memory minimum and whether port 2821 (local's own reverse proxy, not part of a vps deployment) is checked`)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	// Same two values deploy.Configure itself accepts (see its own check) --
	// validated here too, not left to fall through: DockerMemory/PortFree's
	// own target branches only ever recognize "vps" explicitly and treat
	// anything else as "local" by default, so an unrecognized value (a
	// typo, "staging") would silently run local's checks and print
	// local's success message while claiming to have checked something
	// else entirely (flagged in review).
	if doctorTarget != "local" && doctorTarget != "vps" {
		return fmt.Errorf(`--target must be "local" or "vps", got %q`, doctorTarget)
	}

	dockerDaemon := checks.DockerDaemon()
	results := []checks.Result{
		dockerDaemon,
		checks.ComposePlugin(),
	}
	// Mirrors deploy.Configure's own target split (see its comment on the
	// same check) -- vps's reverse proxy is versola-cli's own and which
	// ports it needs depends on --proxy and --auth-url, which doctor
	// doesn't take; "configure vps" checks them itself (checkProxyPorts).
	if doctorTarget == "local" {
		results = append(results, checks.PortFree(proxy.LocalPort, proxy.LocalContainerName))
	}
	results = append(results, checks.DockerMemory(doctorTarget), checks.DiskSpace())
	if doctorTarget == "vps" {
		central, err := centralChecks(doctorAuthURL, doctorCentralURL)
		if err != nil {
			return err
		}
		results = append(results, central...)
		public, err := publicNameChecks(doctorAuthURL, doctorMTLSDomain, doctorCentralMTLSDomain, doctorCentralDomain)
		if err != nil {
			return err
		}
		results = append(results, public...)
		// Certificates of a deployment already on this host: none to check
		// before the first configure.
		if r, found := checks.MTLSLeafCheck(deploy.MTLSVolumeName("vps"), "versola-step-ca"); found {
			results = append(results, r)
		}
	} else if doctorAuthURL != "" || doctorCentralURL != "" || doctorMTLSDomain != "" || doctorCentralMTLSDomain != "" || doctorCentralDomain != "" {
		return fmt.Errorf("--auth-url, --central-url, --mtls-domain, --central-mtls-domain and --central-domain only apply to --target vps")
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

	// The certificates of a deployment on this host, each with its notAfter.
	if st, err := state.Load(); err == nil && st.Target == doctorTarget {
		fmt.Println()
		deploy.PrintCertificates(os.Stdout, st, time.Now())
	}

	fmt.Println()
	if failed == 0 {
		// Target-specific, not hardcoded to local's own command -- a
		// successful `doctor --target vps` used to print this same local
		// suggestion regardless, telling someone who just checked a server
		// to go run a local deployment instead (flagged in review).
		if doctorTarget == "vps" {
			fmt.Println("All checks passed — ready for `versola configure vps <version> --auth-url ... --postgres-host ...`.")
		} else {
			fmt.Println("All checks passed — ready for `versola bootstrap local <version>`.")
		}
		return nil
	}

	fmt.Printf("%d check(s) failed. Fix the issue(s) above before running bootstrap.\n", failed)
	return fmt.Errorf("doctor found %d problem(s)", failed)
}

// centralChecks: --central-url, when given, is where an edge reaches
// central's mutual-TLS listener: its name has to resolve to this server and
// the port answer a TLS handshake. Only with --auth-url or --central-url,
// since doctor otherwise knows nothing about the deployment. The handshake
// checks are for a server whose central is running: before `versola up`
// there is nothing to answer.
func centralChecks(authURL, centralURL string) ([]checks.Result, error) {
	if centralURL == "" {
		return nil, nil
	}
	var auth proxy.AuthURL
	if authURL != "" {
		var err error
		if auth, err = proxy.ParseAuthURL(authURL, proxy.ModeExternal); err != nil {
			return nil, err
		}
	}
	c, ok, err := proxy.ParseCentralURL(centralURL, auth)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	name := checks.CentralName(c.Host)
	results := []checks.Result{name}
	if !name.OK {
		return results, nil
	}
	port := strconv.Itoa(proxy.CentralMutualTLSPort)
	if c.Port != "" {
		port = c.Port
	}
	return append(results, checks.TLSAnswers("Central mutual TLS", net.JoinHostPort(c.Host, port), c.Host)), nil
}

// publicNameChecks: the names --mtls-domain, --central-mtls-domain and
// --central-domain will publish: each has to resolve to this server (the
// proxy routes it by SNI on 443), and 443 has to answer a TLS handshake for
// it (before `versola up` nothing does: run it after). Needs --auth-url, the
// name the others are checked against.
func publicNameChecks(authURL, mtls, centralMTLS, enroll string) ([]checks.Result, error) {
	if mtls == "" && centralMTLS == "" && enroll == "" {
		return nil, nil
	}
	if authURL == "" {
		return nil, fmt.Errorf("--mtls-domain, --central-mtls-domain and --central-domain are checked against --auth-url: pass it too")
	}
	auth, err := proxy.ParseAuthURL(authURL, proxy.ModeExternal)
	if err != nil {
		return nil, err
	}
	names, err := proxy.ParsePublicNames(mtls, centralMTLS, enroll, auth)
	if err != nil {
		return nil, err
	}
	var results []checks.Result
	for _, n := range []struct{ label, host string }{
		{"mutual-TLS name", names.MTLS}, {"central mutual-TLS name", names.CentralMTLS}, {"enrollment name", names.Enroll},
	} {
		if n.host == "" {
			continue
		}
		name := checks.NameResolves(n.label, n.host)
		results = append(results, name)
		if name.OK {
			results = append(results, checks.TLSAnswers(strings.ToUpper(n.label[:1])+n.label[1:], net.JoinHostPort(n.host, "443"), n.host))
		}
	}
	return results, nil
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
	line, err := deploy.ReadLine(os.Stdin)
	if err != nil && !errors.Is(err, io.EOF) {
		// A read error (or an over-long line) is no answer, even if what came
		// before it looks like "y".
		return ""
	}
	return strings.TrimSpace(line)
}

func yes(answer string) bool {
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes"
}
