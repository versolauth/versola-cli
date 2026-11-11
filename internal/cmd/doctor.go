package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

var doctorTarget, doctorAuthURL, doctorCentralURL string
var doctorMTLSDomain, doctorCentralMTLSDomain, doctorCentralDomain string
var doctorProxy, doctorPostgresHost, doctorPostgresCA, doctorRegistry string
var doctorBundle string
var doctorLogs bool

// doctorDockerOK is whether doctorChecks found the Docker daemon answering: the
// diagnostics archive skips what needs Docker when it did not, instead of
// waiting on a daemon that accepts a connection and never replies.
var doctorDockerOK = true

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	GroupID: groupGetStarted,
	Short:   "Check that this machine has everything Versola needs",
	Long: `doctor checks a machine for what Versola needs, your computer (the default)
or a server (--target vps): a reachable Docker daemon, the Docker Compose
plugin, enough memory and disk, and on local a free port for its reverse
proxy. It never installs anything on its own -- if Docker isn't found at
all, the most it does is offer to open an install page in your browser,
and only after you confirm.

Run this before "versola bootstrap" to see ahead of time what's missing.
Pass --target vps when checking a real server -- some checks differ:
running it with no flag against a vps machine can fail a check that
"configure vps" itself would actually pass, or pass one it would actually
fail. The checks of the public names a deployment will have are under
"versola doctor --help-all" (README, "doctor").

On a server, give it the flags you will give "bootstrap vps": with
--auth-url (and --proxy) it checks that the name points at this server
and that the proxy's ports are free; without --postgres-host, that port
5432 is free for the Postgres container; and that the image registry
answers. When everything passes, it prints the bootstrap command with
those flags. Choose its <version> with "versola versions".`,
	Example: `  versola doctor
  versola doctor --target vps --auth-url https://id.example.com
  versola doctor --target vps --auth-url https://id.example.com --proxy external --postgres-host db.internal:5432`,
	RunE: runDoctor,
}

func init() { addDoctorFlags(doctorCmd) }

// addDoctorFlags defines doctor's flags on cmd (a fresh command in tests).
func addDoctorFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&doctorAuthURL, "auth-url", "", "vps only: the --auth-url you will deploy with (the public names in --help-all are checked against it)")
	f.StringVar(&doctorCentralURL, "central-url", "", "vps only: the --central-url you will deploy with -- checks that its name resolves to this server and that its port answers a TLS handshake")
	f.StringVar(&doctorMTLSDomain, "mtls-domain", "", "vps only: the --mtls-domain you will deploy with -- checks that it resolves to this server and that 443 answers a TLS handshake for it")
	f.StringVar(&doctorCentralMTLSDomain, "central-mtls-domain", "", "vps only: the --central-mtls-domain you will deploy with -- same checks")
	f.StringVar(&doctorCentralDomain, "central-domain", "", "vps only: the --central-domain you will deploy with -- same checks")
	f.StringVar(&doctorProxy, "proxy", proxy.ModeNginx, `vps only: the --proxy you will deploy with ("nginx" or "external"); decides which ports are checked`)
	f.StringVar(&doctorPostgresHost, "postgres-host", "", "vps only: the --postgres-host you will deploy with, if you use a Postgres of your own (without it, port 5432 is checked for the Postgres container)")
	f.StringVar(&doctorPostgresCA, "postgres-ca", "", "vps only: the --postgres-ca you will deploy with (PEM file); the CA the connection to your Postgres is verified against")
	f.StringVar(&doctorRegistry, "registry", "", "vps only: the --registry you will deploy with; the registry whose answer over HTTPS is checked (default: ghcr.io)")
	f.StringVar(&doctorBundle, "bundle", "", "write a diagnostics archive for whoever helps you: what doctor says, versions, the deployment's record, status and stats, and generated files without secrets, to this file (or into this directory), never replacing a file. Checks as the deployment on this machine unless --target is given. Holds no secret value; see the README, \"doctor --bundle\"")
	f.BoolVar(&doctorLogs, "logs", false, "with --bundle: add the last lines of every service's log (not redacted: read them before you send the file)")
	f.StringVar(&doctorTarget, "target", "local", `"local" or "vps": the target you will deploy to (the memory and port checks differ)`)
	// The public names, and the registry, are for remote edges and test
	// registries: under --help-all (see help.go), as in configure.
	hideFlags(cmd, "central-url", "mtls-domain", "central-mtls-domain", "central-domain", "registry", "postgres-ca")
}

func runDoctor(cmd *cobra.Command, args []string) error {
	if doctorBundle == "" {
		if doctorLogs {
			return exitcode.With(exitcode.Usage, fmt.Errorf("--logs goes with --bundle: it adds the services' logs to the archive"))
		}
		return doctorChecks(cmd, os.Stdout, false)
	}
	// Before any check: a name that is taken is a usage error, not something to
	// find out after the checks have run.
	if err := deploy.CheckDiagTarget(doctorBundle); err != nil {
		return exitcode.With(exitcode.Usage, err)
	}
	// What doctor says goes into the archive as it was printed.
	var said bytes.Buffer
	doctorDockerOK = true
	checkErr := doctorChecks(cmd, io.MultiWriter(os.Stdout, &said), true)
	// The archive is made when the checks ran: they passed, or found problems
	// (exit code 3), which is why it is wanted. Any other error is a flag or an
	// input doctor refused before it checked anything (a usage error, a name
	// that does not parse): there the customer needs the message, not a file
	// whose name is then taken when they run it again.
	if checkErr != nil && exitcode.Code(checkErr) != exitcode.Prerequisites {
		return checkErr
	}
	fmt.Fprintln(os.Stdout, "\nCollecting the diagnostics archive (the Docker calls in it can take a while)...")
	if err := writeDiagBundle(os.Stdout, said.String(), doctorDockerOK); err != nil {
		// Its own code: a script must tell "the archive could not be written"
		// from "the machine is not ready", which checkErr says.
		return exitcode.With(exitcode.Failure, errors.Join(err, checkErr))
	}
	return checkErr
}

// doctorChecks is doctor: it writes what it says to w. bundling is whether
// the output is also going into a diagnostics archive (--bundle): then the
// target defaults to the one of the deployment on this machine, and nothing
// interactive is offered.
func doctorChecks(cmd *cobra.Command, w io.Writer, bundling bool) error {
	// A support archive is for the deployment that is there: when --target is
	// left out, check as that one, not as a first local run.
	if bundling && !cmd.Flags().Changed("target") {
		if st, err := state.Load(); err == nil && (st.Target == "vps" || st.Target == "local") {
			doctorTarget = st.Target
			fmt.Fprintf(w, "Checking as the %s deployment on this machine.\n\n", st.Target)
		}
	}
	// Same two values deploy.Configure itself accepts (see its own check) --
	// validated here too, not left to fall through: DockerMemory/PortFree's
	// own target branches only ever recognize "vps" explicitly and treat
	// anything else as "local" by default, so an unrecognized value (a
	// typo, "staging") would silently run local's checks and print
	// local's success message while claiming to have checked something
	// else entirely (flagged in review).
	if doctorTarget != "local" && doctorTarget != "vps" {
		return exitcode.With(exitcode.Usage, fmt.Errorf(`--target must be "local" or "vps", got %q`, doctorTarget))
	}
	// The flags are checked before any check runs: a value configure would
	// refuse is refused here first, and a vps flag on local is not ignored.
	var vps vpsInputs
	if doctorTarget == "vps" {
		// What configure takes from the deployment on this machine when the
		// flag is left out, doctor takes too: a redeploy is checked as it
		// will run, not as a first install with the defaults.
		fill := recordedVpsFlags(cmd.Flags().Changed, deploy.StoredVps(), deploy.StoredPublicDomains())
		applyRecorded(fill)
		var err error
		if vps, err = parseVpsInputs(doctorAuthURL, doctorProxy, doctorPostgresHost, doctorRegistry, doctorCentralURL, doctorMTLSDomain, doctorCentralMTLSDomain, doctorCentralDomain); err != nil {
			return err
		}
		vps.postgresCA = doctorPostgresCA
		if len(fill) > 0 {
			fmt.Fprintf(w, "From the deployment on this machine, as configure takes them: %s\n\n", describeRecorded(fill))
		}
	} else {
		for _, name := range doctorVpsOnlyFlags {
			if cmd.Flags().Changed(name) {
				return exitcode.With(exitcode.Usage, fmt.Errorf("--%s only applies to --target vps", name))
			}
		}
	}

	dockerDaemon := checks.DockerDaemon()
	doctorDockerOK = dockerDaemon.OK
	results := []checks.Result{
		dockerDaemon,
		checks.ComposePlugin(),
	}
	// Mirrors deploy.Configure's own target split (see its comment on the
	// same check) -- vps's reverse proxy is versola-cli's own and which
	// ports it needs depends on --proxy and --auth-url (vpsChecks below).
	if doctorTarget == "local" {
		results = append(results, checks.PortFree(proxy.LocalPort, proxy.LocalContainerName))
	}
	results = append(results, checks.DockerMemory(doctorTarget, doctorTarget == "vps" && deploy.PostgresIsBundled(vps.postgresHost)), checks.DiskSpace())
	if doctorTarget == "vps" {
		results = append(results, vpsChecks(vps)...)
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
	}

	failed, warned := 0, 0
	for _, r := range results {
		fmt.Fprintln(w, r.String())
		if !r.OK {
			failed++
		} else if r.Warn {
			warned++
		}
	}
	if doctorTarget == "vps" && doctorAuthURL == "" {
		fmt.Fprintln(w, "\nPass --auth-url (and --proxy, if you deploy behind a web server of your own) to check its DNS name and the proxy's ports too.")
	}

	if !dockerDaemon.OK && !bundling {
		offerDockerHelp()
	} else if !dockerDaemon.OK {
		// Nothing interactive in an archive run, but the hint that says what to
		// do about a daemon that is installed and not reachable is kept.
		if path, err := exec.LookPath("docker"); err == nil {
			fmt.Fprintln(w)
			fmt.Fprintln(w, daemonUnreachableHint(path))
		}
	}

	// The certificates of a deployment on this host, each with its notAfter.
	if st, err := state.Load(); err == nil && st.Target == doctorTarget {
		fmt.Fprintln(w)
		deploy.PrintCertificates(w, st, time.Now())
		printCertsWarnings(w, st)
	}

	fmt.Fprintln(w)
	if failed == 0 {
		// Target-specific, not hardcoded to local's own command -- a
		// successful `doctor --target vps` used to print this same local
		// suggestion regardless, telling someone who just checked a server
		// to go run a local deployment instead (flagged in review).
		if warned > 0 {
			fmt.Fprintf(w, "%d warning(s) above: read them before going on.\n", warned)
		}
		printDoctorReady(w, cmd)
		return nil
	}

	fmt.Fprintf(w, "%d check(s) failed. Fix the issue(s) above before running bootstrap.\n", failed)
	// Exit code 3 (exitcode.Prerequisites), the same as configure's own
	// prerequisite check: a script running doctor first can tell "this
	// machine isn't ready" from doctor itself failing.
	return exitcode.With(exitcode.Prerequisites, fmt.Errorf("doctor found %d problem(s)", failed))
}

// doctorVpsOnlyFlags are the flags that describe a vps deployment: refused
// with --target local rather than ignored.
var doctorVpsOnlyFlags = []string{"auth-url", "central-url", "mtls-domain", "central-mtls-domain", "central-domain", "proxy", "postgres-host", "postgres-ca", "registry"}

// vpsInputs are doctor's vps flags, validated and normalized as configure
// does it.
type vpsInputs struct {
	// auth is set when --auth-url was given.
	auth         *proxy.AuthURL
	mode         string
	public       proxy.PublicNames
	postgresHost string
	postgresCA   string
	registry     string
}

func parseVpsInputs(authURL, mode, postgresHost, registry, centralURL, mtls, centralMTLS, enroll string) (vpsInputs, error) {
	var in vpsInputs
	var err error
	if in.mode, err = proxy.ParseMode(mode); err != nil {
		return in, err
	}
	if in.registry, err = deploy.ParseRegistry(registry); err != nil {
		return in, err
	}
	in.postgresHost = postgresHost
	if authURL == "" {
		// Checked by centralChecks against an empty auth URL, as here.
		if _, _, err := proxy.ParseCentralURL(centralURL, proxy.AuthURL{}); err != nil {
			return in, err
		}
		return in, nil
	}
	auth, err := proxy.ParseAuthURL(authURL, in.mode)
	if err != nil {
		return in, err
	}
	in.auth = &auth
	if _, _, err := proxy.ParseCentralURL(centralURL, auth); err != nil {
		return in, err
	}
	if in.public, err = proxy.ParsePublicNames(mtls, centralMTLS, enroll, auth); err != nil {
		return in, err
	}
	return in, nil
}

// vpsChecks are the checks of what `configure vps` is going to be given:
// the --auth-url name and the proxy's ports (with --auth-url, which decides
// them), the Postgres (its port, for the container), the image registry and
// lingering.
func vpsChecks(in vpsInputs) []checks.Result {
	var results []checks.Result
	if in.auth != nil {
		results = append(results, checks.AuthName(in.auth.Host, in.auth.TLS(in.mode)), deploy.ProxyPortsCheck(*in.auth, in.mode, in.public))
	}
	results = append(results, deploy.PostgresCheck(in.postgresHost))
	if r, ok := deploy.PostgresProbeCheck(in.postgresHost, in.postgresCA); ok {
		results = append(results, r)
	}
	results = append(results, checks.RegistryAnswers(deploy.RegistryHost(in.registry)))
	// No timer is installed for certificates that never expire: lingering
	// does not matter then.
	if days := deploy.StoredCertDays(); days == nil || *days != 0 {
		if r, ok := lingerCheck(lingerState()); ok {
			results = append(results, r)
		}
	}
	return results
}

// lingerCheck: `up` installs a systemd user timer that renews the mutual-TLS
// certificates, and a user timer stops with the user's last session unless
// lingering is on -- said now, not after `up`. Nothing when it can't be told.
func lingerCheck(state string) (checks.Result, bool) {
	switch state {
	case "yes":
		return checks.Result{Name: "Lingering", OK: true, Detail: "on: the certificate-renewal timer `up` installs keeps running after you log out"}, true
	case "no":
		u := currentUser()
		return checks.Result{Name: "Lingering", OK: true, Warn: true, Detail: fmt.Sprintf("off for %s: the certificate-renewal timer `up` installs would stop when your last session ends. Run once: sudo loginctl enable-linger %s", u, u)}, true
	}
	return checks.Result{}, false
}

func printDoctorReady(w io.Writer, cmd *cobra.Command) {
	if doctorTarget == "vps" {
		fmt.Fprintf(w, "All checks passed — ready for:\n\n  %s\n\nReplace <version> with a Versola release from `versola versions`.\n", bootstrapLine(cmd))
	} else {
		fmt.Fprintln(w, "All checks passed — ready for `versola bootstrap local` (the latest Versola release).")
	}
}

// bootstrapLine is the `bootstrap vps` command for the flags doctor was
// given, ready to copy: the values as typed (--proxy too when it is the
// default: given, it may override a recorded mode), and a placeholder for
// --auth-url when there is none, neither given nor recorded. Recorded values are not written out: configure takes
// them itself.
func bootstrapLine(cmd *cobra.Command) string {
	parts := []string{"versola bootstrap vps <version>"}
	if doctorAuthURL == "" {
		parts = append(parts, "--auth-url https://<your domain>")
	}
	for _, name := range []string{"auth-url", "proxy", "postgres-host", "postgres-ca", "registry", "central-url", "mtls-domain", "central-mtls-domain", "central-domain"} {
		f := cmd.Flags().Lookup(name)
		if f == nil || !cmd.Flags().Changed(name) || f.Value.String() == "" {
			continue
		}
		parts = append(parts, "--"+name+" "+shellArg(f.Value.String()))
	}
	return strings.Join(parts, " ")
}

// recordedVpsFlags are the flags `configure vps` takes from the recorded
// deployment when they are left out (resolveVpsFlags, publicDomains), by
// name, with the recorded value: --auth-url, --proxy and the public names.
// --postgres-host is not among them: deploy.PostgresCheck applies the record
// itself, as configure does.
func recordedVpsFlags(changed func(string) bool, rec *deploy.RecordedVps, pub deploy.PublicDomains) map[string]string {
	fill := map[string]string{}
	if rec != nil {
		if !changed("auth-url") && rec.AuthURL != "" {
			fill["auth-url"] = rec.AuthURL
		}
		if !changed("proxy") && rec.ProxyMode != "" {
			fill["proxy"] = rec.ProxyMode
		}
	}
	for name, v := range map[string]string{"mtls-domain": pub.MTLS, "central-mtls-domain": pub.CentralMTLS, "central-domain": pub.Enroll} {
		if !changed(name) && v != "" {
			fill[name] = v
		}
	}
	return fill
}

// applyRecorded sets doctor's flag variables from fill.
func applyRecorded(fill map[string]string) {
	for name, to := range map[string]*string{"auth-url": &doctorAuthURL, "proxy": &doctorProxy, "mtls-domain": &doctorMTLSDomain, "central-mtls-domain": &doctorCentralMTLSDomain, "central-domain": &doctorCentralDomain} {
		if v, ok := fill[name]; ok {
			*to = v
		}
	}
}

// describeRecorded lists fill as flags, in a fixed order.
func describeRecorded(fill map[string]string) string {
	var out []string
	for _, name := range []string{"auth-url", "proxy", "mtls-domain", "central-mtls-domain", "central-domain"} {
		if v, ok := fill[name]; ok {
			out = append(out, "--"+name+" "+v)
		}
	}
	return strings.Join(out, ", ")
}

// shellArgRe is what a shell takes as one word without quotes.
var shellArgRe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// shellArg quotes s for a POSIX shell when it needs it.
func shellArg(s string) string {
	if shellArgRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
		return nil, exitcode.With(exitcode.Usage, fmt.Errorf("--mtls-domain, --central-mtls-domain and --central-domain are checked against --auth-url: pass it too"))
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
