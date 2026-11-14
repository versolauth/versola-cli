package deploy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

// What auth says about the first login of the bootstrap admin
// (versolauth/versola#824). The CLI reads these two lines and nothing else of
// auth's log: the link is the one value it ever prints out of that log, and
// it is printed once, never stored (not in state.json, not in a file).
const (
	// firstLoginMarker precedes the link in the line auth writes when it
	// issues one: "first-login link for admin: <url>?first_login_token=..." as the message of a JSON log line.
	firstLoginMarker = "first-login link for admin: "
	// resetIgnoredMarker is auth's warning that BOOTSTRAP_RESET was refused
	// because the admin already has a permanent password.
	resetIgnoredMarker = "BOOTSTRAP_RESET ignored"
	// adminResetFile is the compose override that puts BOOTSTRAP_RESET=1 into
	// auth's environment for the one start that resets; it exists only while
	// that start runs.
	adminResetFile = "admin-reset.yml"
	// adminResetLogWait is how long the reset waits, once auth is ready, for
	// auth's answer in its log: readiness and the bootstrap step are not the
	// same moment.
	adminResetLogWait = 20 * time.Second
)

// parseFirstLoginLink returns the link auth logged in text, the first one when
// there are several, and "" when there is none or the line is not a URL: a
// release without the link prints nothing, and a malformed line is not worth
// putting in front of an operator.
func parseFirstLoginLink(text string) string {
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, firstLoginMarker)
		if i < 0 {
			continue
		}
		// Auth logs one JSON object per line, the message in quotes and
		// followed by more fields ("...first_login_token=T","span_id":...),
		// so the URL ends at the first quote, backslash, angle bracket or
		// space, not at the first space. A token is URL-safe base64 or hex:
		// it never holds a dot or a comma, so those at the end are the
		// sentence's.
		rest := line[i+len(firstLoginMarker):]
		if end := strings.IndexAny(rest, " \t\r\"'\\<>"); end >= 0 {
			rest = rest[:end]
		}
		rest = strings.TrimRight(rest, ".,;")
		if strings.HasPrefix(rest, "https://") || strings.HasPrefix(rest, "http://") {
			// The first one: auth logs it once, at the bootstrap that
			// issued it, so a later line with the marker is not auth's.
			return rest
		}
	}
	return ""
}

// adminResetOps is what the reset does to the world, so a test can run it
// without Docker.
type adminResetOps interface {
	// recreateAuth recreates auth's first replica from the compose file, with
	// BOOTSTRAP_RESET=1 in its environment when reset is set, without it
	// otherwise.
	recreateAuth(reset bool) error
	waitReady() error
	// authLogs is what auth's replicas logged since the given time (all of it
	// for the zero time).
	authLogs(since time.Time) (string, error)
	now() time.Time
	sleep(d time.Duration)
}

// AdminResetPassword gives the bootstrap admin of the deployment a new
// first-login link: auth is recreated once with BOOTSTRAP_RESET=1 -- which
// drops an unused temporary password and link and issues new ones -- and once
// more without it, and the link auth logs is printed.
//
// A plain restart would not do: auth reads the variable from its container's
// environment, which `docker restart` does not renew, and a restart of a
// still-valid temporary password changes nothing. Only slot 1 is recreated
// (it is the one that resets and logs the link); the other replicas keep
// serving.
//
// On local nothing is reset: the demo login is fixed, as `up` prints it.
func AdminResetPassword(st *state.State, out io.Writer) error {
	if st == nil {
		return fmt.Errorf("nothing has been configured yet — %s", NothingDeployedHint)
	}
	if st.Target != "vps" {
		fmt.Fprintln(out, "This is a local deployment: its admin login is fixed.")
		fmt.Fprintln(out, "Login: admin / Admin1234!")
		return nil
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}
	dir := filepath.Dir(composePath)
	if err := checkRunnable(st, dir); err != nil {
		return err
	}
	readyURL, authService, err := authReadiness(st, composePath)
	if err != nil {
		return err
	}
	ops := dockerAdminResetOps{composePath: composePath, dir: dir, service: authService, readyURL: readyURL}
	defer ops.removeOverride()

	// A Ctrl-C, or the SIGHUP of a dropped SSH session, must not stop the
	// command between the two recreates: auth would keep BOOTSTRAP_RESET and
	// reset again on every start. The signals are taken here (and the docker
	// calls shielded from the terminal's Ctrl-C) so the sequence reaches its
	// last step; a later `versola up` also repairs an auth that was cut off.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	docker.SetShielded(true)
	defer docker.SetShielded(false)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-sigs:
				fmt.Fprintln(os.Stderr, "\nFinishing first: auth is restarted without the reset before this command ends (a kill -9 would leave BOOTSTRAP_RESET set until the next `versola up`).")
			case <-done:
				return
			}
		}
	}()
	return resetAdminPassword(ops, out, filepath.Join(dir, "auth.secrets.env"))
}

// resetAdminPassword is AdminResetPassword's sequence. secretsPath is only
// ever named, never read.
func resetAdminPassword(ops adminResetOps, out io.Writer, secretsPath string) error {
	fmt.Fprintln(out, "Restarting auth to issue a new first-login link...")
	link, ignored, runErr := resetAuth(ops)

	// Whatever happened above, auth is recreated without the variable: left
	// set, every later start would reset again.
	fmt.Fprintln(out, "Restarting auth once more, without the reset...")
	var cleanupErr error
	if err := ops.recreateAuth(false); err != nil {
		// The recreate itself failed: the container may still carry the variable.
		cleanupErr = fmt.Errorf("couldn't restart auth without the reset: %w -- run `versola up`; until it succeeds auth may still have BOOTSTRAP_RESET set and reset again on every start", err)
	} else if err := ops.waitReady(); err != nil {
		cleanupErr = fmt.Errorf("auth was restarted without the reset but did not become ready: %w -- run `versola up` to start it again (%s)", err, LogsHint(AuthService))
	}
	var refusal error
	if ignored {
		refusal = fmt.Errorf("the admin already has a permanent password, so nothing was reset: this command only renews the first login. A forgotten permanent password is recovered through the product's own account recovery")
	}
	if err := errors.Join(runErr, refusal, cleanupErr); err != nil {
		return err
	}
	if link == "" {
		fmt.Fprintf(out, "auth logged no first-login link: this release or this deployment's configuration has none, or auth took longer than %s to log it (run the command again). If the admin has no permanent password yet, the temporary one is ADMIN_BOOTSTRAP_PASSWORD in %s.\n", adminResetLogWait, secretsPath)
		return nil
	}
	fmt.Fprintf(out, "\nFirst login (opens once; set the admin's permanent password there):\n%s\n", link)
	return nil
}

// resetAuth is the part with BOOTSTRAP_RESET set: recreate, wait until auth
// is ready and then until it has said what it did.
func resetAuth(ops adminResetOps) (link string, ignored bool, err error) {
	if err := ops.recreateAuth(true); err != nil {
		return "", false, fmt.Errorf("couldn't restart auth: %w", err)
	}
	if err := ops.waitReady(); err != nil {
		return "", false, fmt.Errorf("auth never became ready: %w -- %s", err, LogsHint(AuthService))
	}
	deadline := ops.now().Add(adminResetLogWait)
	for {
		text, err := ops.authLogs(time.Time{})
		if err != nil {
			return "", false, fmt.Errorf("couldn't read auth's log: %w", err)
		}
		if link = parseFirstLoginLink(text); link != "" {
			return link, false, nil
		}
		if strings.Contains(text, resetIgnoredMarker) {
			return "", true, nil
		}
		if !ops.now().Before(deadline) {
			return "", false, nil
		}
		ops.sleep(time.Second)
	}
}

// authReadiness is where auth's first replica answers readiness, and its
// compose service name, from the compose file as `up` reads them.
func authReadiness(st *state.State, composePath string) (url, service string, err error) {
	isVps := st.Target == "vps"
	topo, err := loadTopology(composePath)
	if err != nil {
		return "", "", err
	}
	central, err := replicasOf(topo, CentralService, []state.Slot{{N: 1, Version: st.Version}})
	if err != nil {
		return "", "", err
	}
	if central, err = frontCentral(central, isVps); err != nil {
		return "", "", err
	}
	auth, err := replicasOf(topo, AuthService, st.ActiveSlots(AuthService))
	if err != nil {
		return "", "", err
	}
	if auth, err = frontAuth(auth, isVps); err != nil {
		return "", "", err
	}
	edge, err := replicasOf(topo, EdgeService, st.ActiveSlots(EdgeService))
	if err != nil {
		return "", "", err
	}
	urls, err := checkTopology(topo, isVps, deployedProxyReservations(st), central, auth, edge)
	if err != nil {
		return "", "", err
	}
	if len(auth) == 0 {
		return "", "", fmt.Errorf("the deployment has no auth service")
	}
	return urls[auth[0].Service], auth[0].Service, nil
}

type dockerAdminResetOps struct {
	composePath string
	dir         string
	service     string
	readyURL    string
}

func (o dockerAdminResetOps) overridePath() string { return filepath.Join(o.dir, adminResetFile) }

// adminResetOverride is the whole override: one variable on one service. It
// holds no secret.
func adminResetOverride(service string) string {
	return fmt.Sprintf("services:\n  %s:\n    environment:\n      BOOTSTRAP_RESET: \"1\"\n", service)
}

func adminResetRecreateArgs(composePath, overridePath, service string, reset bool) []string {
	args := state.ComposeArgs(composePath)
	if reset {
		args = append(args, "-f", overridePath)
	}
	return append(args, "up", "-d", "--no-deps", "--force-recreate", "-t", stopTimeoutSeconds, service)
}

func (o dockerAdminResetOps) recreateAuth(reset bool) error {
	if reset {
		if err := os.WriteFile(o.overridePath(), []byte(adminResetOverride(o.service)), 0o600); err != nil {
			return err
		}
	} else {
		o.removeOverride()
	}
	return docker.Run(adminResetRecreateArgs(o.composePath, o.overridePath(), o.service, reset)...)
}

func (o dockerAdminResetOps) removeOverride() { _ = os.Remove(o.overridePath()) }

func (o dockerAdminResetOps) waitReady() error { return wait.ForReady(o.readyURL, readyTimeout) }

func (o dockerAdminResetOps) authLogs(since time.Time) (string, error) {
	return authLogsText(o.composePath, []string{o.service}, since)
}

func (o dockerAdminResetOps) now() time.Time        { return time.Now() }
func (o dockerAdminResetOps) sleep(d time.Duration) { time.Sleep(d) }

// authLogsText reads what the given auth services logged since a time (all of
// it for the zero time). Only parsed by the caller, never shown.
func authLogsText(composePath string, services []string, since time.Time) (string, error) {
	rest := []string{"logs", "--no-color", "--no-log-prefix"}
	if !since.IsZero() {
		rest = append(rest, "--since", since.UTC().Format(time.RFC3339))
	}
	rest = append(rest, services...)
	out, err := docker.Output(state.ComposeArgs(composePath, rest...)...)
	return string(out), err
}

// firstLoginLinkSince is the link auth logged since a time, for `up` to print
// once it has started the stack, and "" when there is none (an older release,
// a start that issued none, a log that cannot be read): `up` then says where
// the password file is, as it did before.
func firstLoginLinkSince(composePath string, auth []replica, since time.Time) string {
	var services []string
	for _, r := range auth {
		services = append(services, r.Service)
	}
	text, err := authLogsText(composePath, services, since)
	if err != nil {
		return ""
	}
	return parseFirstLoginLink(text)
}
