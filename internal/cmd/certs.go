package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var (
	certsPinWait      time.Duration
	certsRestart      bool
	certsInstallTimer bool
	certsShowTimer    bool
)

var certsCmd = &cobra.Command{
	Use:   "certs",
	Short: "Manage the mutual-TLS certificates of the deployment",
}

var certsRenewCmd = &cobra.Command{
	Use:   "renew",
	Short: "Renew the mutual-TLS certificates that are due, and restart what uses them",
	Long: `renew keeps the deployment's mutual-TLS certificates (auth's and central's
listeners, the certificate edge presents to central) valid. It runs
versola-tools' mtls-init service, which re-issues a certificate only
when a third of its lifetime or less is left. The services read their
certificates at startup, so they are restarted.

On vps that is rolling, and a server leaf is never replaced before the
edges know its successor: the replacement is staged first (its pin is
added to what edges trust), the edges restart one replica at a time --
each out of the proxy, drained, restarted, ready, back in -- central
restarts to publish the pin to edges in other networks (only when
--mtls-domain or --central-mtls-domain is set) and --pin-wait is left
for them to fetch it, and then the leaf is replaced and central and auth's
replicas are restarted onto it, auth's one at a time. A service with a
single replica is restarted where it stands (add a replica with
"versola replica add" to restart it without a gap). A renewal cut off
anywhere is finished by running it again. On local, central, auth and
edge are restarted in that order, each waited for.

When nothing is due it prints nothing and exits 0, so it is meant to run
on a schedule: --show-timer prints a daily systemd timer (or, without
systemd, a crontab line); --install-timer writes and enables the systemd
timer for the current user. Nothing is installed without --install-timer.

Works for the vps and local targets. A failing step exits non-zero with
its output. --restart restarts the services even when nothing was due,
for finishing a renewal whose restart failed.`,
	Args: cobra.NoArgs,
	RunE: runCertsRenew,
}

func init() {
	certsRenewCmd.Flags().DurationVar(&certsPinWait, "pin-wait", deploy.DefaultPinWait, "vps with a published mutual-TLS name: how long a staged pin is left to reach edges in other networks before the leaf is replaced")
	certsRenewCmd.Flags().BoolVar(&certsRestart, "restart", false, "restart central, auth and edge even when nothing was renewed")
	certsRenewCmd.Flags().BoolVar(&certsShowTimer, "show-timer", false, "print the daily timer (systemd units, or a crontab line) instead of renewing; writes nothing")
	certsRenewCmd.Flags().BoolVar(&certsInstallTimer, "install-timer", false, "write and enable a daily systemd timer for the current user instead of renewing")
	certsCmd.AddCommand(certsRenewCmd)
}

func runCertsRenew(cmd *cobra.Command, args []string) error {
	if certsShowTimer || certsInstallTimer {
		if certsShowTimer && certsInstallTimer {
			return fmt.Errorf("--show-timer and --install-timer are alternatives")
		}
		return certsTimer(certsInstallTimer)
	}
	// Held like `up`'s: a concurrent configure would delete the bundle this
	// runs compose against.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := state.Load()
	if err != nil {
		return err
	}
	return deploy.RenewCerts(deploy.RenewCertsOptions{Restart: certsRestart, PinWait: certsPinWait}, st)
}

// systemdUserAvailable: a systemd running as init, with systemctl on PATH.
func systemdUserAvailable() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

func certsTimer(install bool) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("couldn't find this binary's path: %w", err)
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	if !systemdUserAvailable() {
		fmt.Println("No systemd here. Add this line to the crontab of the user that runs versola (`crontab -e`):")
		fmt.Println()
		fmt.Println("  " + deploy.CertsCronLine(bin))
		fmt.Println()
		fmt.Println("cron's PATH is short: docker has to be in it, or set PATH at the top of the crontab.")
		if install {
			return fmt.Errorf("--install-timer only writes systemd units; the crontab line above is yours to add")
		}
		return nil
	}
	service, timer, err := deploy.CertsUnits(bin, os.Getenv("PATH"))
	if err != nil {
		return err
	}
	dir, err := userUnitDir()
	if err != nil {
		return err
	}
	if !install {
		fmt.Printf("`versola certs renew --install-timer` would write, in %s:\n\n# %s\n%s\n# %s\n%s\n", dir, deploy.CertsServiceUnit, service, deploy.CertsTimerUnit, timer)
		fmt.Println("and run `systemctl --user daemon-reload` and `systemctl --user enable --now " + deploy.CertsTimerUnit + "`.")
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, content := range map[string]string{deploy.CertsServiceUnit: service, deploy.CertsTimerUnit: timer} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("couldn't write %s: %w", path, err)
		}
		fmt.Println("Wrote", path)
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", deploy.CertsTimerUnit}} {
		c := exec.Command("systemctl", args...)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("systemctl %v failed: %w", args, err)
		}
	}
	fmt.Println("\nThe timer runs daily. A user timer stops when the user logs out unless lingering is on: `sudo loginctl enable-linger $USER`.")
	return nil
}

func userUnitDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}
