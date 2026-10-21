package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var upYes bool
var upNoBrowser bool
var upNoCertsTimer bool

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the deployment configure/migrate prepared",
	Long: `up starts the stack "versola configure" prepared and waits until it's
actually serving traffic, not merely started.

It assumes "versola migrate" already ran — this is not a duplicate
safety net, it's the whole point of splitting them: a schema change
gets its own explicit, reviewable step, not something that happens
silently as a side effect of starting the app. If migrate was skipped,
services fail loudly (schema out of date) instead of quietly migrating.

On vps it also installs the daily systemd timer that runs "versola certs
renew" (--no-certs-timer to skip it) and, if lingering is off for the user,
says what to run so the timer survives logout.

This is the last of the three steps "versola bootstrap" runs together
in one go.

Against a vps deployment this replaces whatever's currently serving
real traffic, so it asks for confirmation first.`,
	Args: cobra.NoArgs,
	RunE: runUp,
}

func init() {
	upCmd.Flags().BoolVarP(&upYes, "yes", "y", false, "don't ask for confirmation (vps)")
	upCmd.Flags().BoolVar(&upNoCertsTimer, "no-certs-timer", false, "vps only: don't install the daily timer that renews the mutual-TLS certificates (`versola certs renew`); refused unless the certificates are unlimited (--cert-lifetime-days unlimited at configure)")
	upCmd.Flags().BoolVar(&upNoBrowser, "no-browser", false, "don't open the admin console in a browser once it's ready")
}

func runUp(cmd *cobra.Command, args []string) error {
	// Held for the same reason as cmd/configure.go and cmd/migrate.go (see
	// state.Lock's own comment) -- not because Up itself writes state, but
	// because a concurrent `configure` finalizing mid-run would delete the
	// very bundle directory (compose file, configs) this Up is actively
	// reading from and running `docker compose` against (see
	// state.Finalize's own comment on removing the previous bundle).
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// What `up` would refuse on the record alone is refused before the vps
	// confirmation: nobody is asked to confirm something that was never
	// going to run. A state that can't be read is left to the calls below.
	if recorded, err := state.Load(); err == nil {
		if err := deploy.CheckUp(recorded); err != nil {
			return err
		}
	}

	if recorded, err := state.Load(); err == nil && recorded.Target == "vps" {
		if err := refuseNoTimerWhenFinite(upNoCertsTimer, recorded.CertLifetimeDays); err != nil {
			return err
		}
	}
	st, err := confirmIfVpsState(upYes, func(version string) string {
		return fmt.Sprintf("start Versola %s on the VPS, replacing whatever's currently serving traffic there", version)
	})
	if err != nil {
		return err
	}
	if err := deploy.Up(deploy.UpOptions{NoBrowser: upNoBrowser}, st); err != nil {
		return err
	}
	// Whichever way a vps deployment was made, something has to renew its
	// certificates (the same as `bootstrap` does).
	scheduleCertsRenewal(os.Stdout, st.Target, upNoCertsTimer || !issuesMutualTLSCertificates(st.Version), st.CertsUnlimited())
	return nil
}
