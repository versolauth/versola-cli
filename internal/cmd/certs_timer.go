package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// The tools the timer checks shell out to, replaceable in tests.
var (
	runOutput = func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).Output()
		return string(out), err
	}
	lookPath    = exec.LookPath
	currentUser = func() string {
		if u, err := user.Current(); err == nil {
			return u.Username
		}
		return os.Getenv("USER")
	}
)

// scheduleCertsRenewal installs the daily `certs renew` timer of a vps
// deployment -- the mutual-TLS certificates are short-lived, and a deployment
// that nobody renews stops working when they run out, whichever way it was
// deployed (`bootstrap`, or `configure`/`migrate`/`up`). A failure does not undo
// a deployment that is up: it is said, loudly. A user timer also stops with the
// user's last session unless lingering is on, which gets a line of its own with
// the exact command.
func scheduleCertsRenewal(w io.Writer, target string, skip, unlimited bool) {
	if target != "vps" || skip || unlimited {
		return // unlimited certificates never need renewing: no timer
	}
	fmt.Fprintln(w, "\nScheduling the daily renewal of the mutual-TLS certificates...")
	if err := certsTimer(true); err != nil {
		fmt.Fprintf(w, "Warning: %v\nThe certificates expire unless `versola certs renew` runs: see `versola certs renew --show-timer`.\n", err)
		return
	}
	if lingerOff() {
		fmt.Fprintf(w, "Warning: lingering is off for %s, so the timer stops when the last session of this user ends. Run once: sudo loginctl enable-linger %s\n", currentUser(), currentUser())
	}
}

// lingerOff: systemd says this user's services do not outlive its sessions.
// Unknown (no loginctl, odd output) is not "off": nothing is claimed then.
func lingerOff() bool { return lingerState() == "no" }

// lingerState is systemd's Linger of this user: "yes", "no", or "" when it
// can't be told (no loginctl, an error, odd output).
func lingerState() string {
	if _, err := exec.LookPath("loginctl"); err != nil {
		return ""
	}
	out, err := runOutput("loginctl", "show-user", currentUser(), "-p", "Linger", "--value")
	if err != nil {
		return ""
	}
	switch v := strings.TrimSpace(out); v {
	case "yes", "no":
		return v
	}
	return ""
}

// certsTimerWarning is "" when something renews the certificates -- the
// systemd user timer enabled, or a crontab line -- and otherwise says in words
// that nothing does, and what to run. A missing timer must never be silent.
func certsTimerWarning() string {
	if dir, err := userUnitDir(); err == nil {
		if _, err := os.Stat(filepath.Join(dir, deploy.CertsTimerUnit)); err == nil {
			if out, err := runOutput("systemctl", "--user", "is-enabled", deploy.CertsTimerUnit); err == nil && strings.TrimSpace(out) == "enabled" {
				if lingerOff() {
					return fmt.Sprintf("the renewal timer is installed, but lingering is off for %s, so it stops when the last session ends -- run: sudo loginctl enable-linger %s", currentUser(), currentUser())
				}
				return ""
			}
		}
	}
	if out, err := runOutput("crontab", "-l"); err == nil && strings.Contains(out, "certs renew") {
		return ""
	}
	return "nothing renews the mutual-TLS certificates on this machine: no systemd user timer and no crontab line for `versola certs renew` -- they expire and the deployment stops working. Run `versola certs renew --install-timer` (or add the line `versola certs renew --show-timer` prints)"
}

// printCertsWarnings prints the timer warning for a vps deployment (local has
// no timer to speak of: its certificates are renewed by hand).
func printCertsWarnings(w io.Writer, st *state.State) {
	if msg := certsWarning(st); msg != "" {
		fmt.Fprintf(w, "WARNING: %s\n", msg)
	}
}

// certsWarning is printCertsWarnings' message, or "": also one of
// `status --json`'s warnings.
func certsWarning(st *state.State) string {
	if st.Target != "vps" || st.CertsUnlimited() || !deploy.IssuesMutualTLSCertificates(st.Version) {
		return ""
	}
	return certsTimerWarning()
}

// certsTimerFiles are the timer's unit files that exist in the user's systemd
// directory: what `certs renew --install-timer` wrote. A crontab line is not
// among them: the CLI only ever prints that one, the customer adds it.
func certsTimerFiles() []string {
	dir, err := userUnitDir()
	if err != nil {
		return nil
	}
	var found []string
	for _, name := range []string{deploy.CertsTimerUnit, deploy.CertsServiceUnit} {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			found = append(found, path)
		}
	}
	return found
}

// removeCertsTimer undoes the install of the daily renewal timer: stop and
// disable it, delete the unit files, reload systemd. Used by `uninstall`: a
// timer left behind keeps running `certs renew` for a deployment that is gone,
// and a later configure installs it again (scheduleCertsRenewal). Stopping may
// fail (no user bus over plain ssh, say); the files are removed anyway, so it
// does not start again, and the output says it.
func removeCertsTimer(w io.Writer) {
	files := certsTimerFiles()
	if len(files) == 0 {
		return
	}
	fmt.Fprintln(w, "Removing the daily certificate renewal timer...")
	stopped := true
	if _, err := lookPath("systemctl"); err != nil {
		stopped = false
	} else if _, err := runOutput("systemctl", "--user", "disable", "--now", deploy.CertsTimerUnit); err != nil {
		stopped = false
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			fmt.Fprintf(w, "  (couldn't remove %s: %v)\n", f, err)
		}
	}
	if stopped {
		_, _ = runOutput("systemctl", "--user", "daemon-reload")
		return
	}
	fmt.Fprintln(w, "  (systemd could not be asked to stop it: the unit files are removed, so it does not start again, but a copy already loaded runs until you log out or `systemctl --user daemon-reload` is run)")
}
