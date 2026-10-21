package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
)

// The public names are opt-in: with none of the three flags nothing is
// published, and every mutual-TLS listener answers on this host's loopback
// only. A name is the one thing a remote edge or an external mutual-TLS
// client is given to connect to, so each is its own decision, not derived
// from --auth-url.
const (
	mtlsDomainUsage        = "vps only: publish auth's mutual-TLS listener at this name, e.g. id-mtls.example.com (remote edges, external FAPI clients). Routed by SNI on port 443 with TLS passed through, limited per client address, and added to the listener's certificate. Remembered; --mtls-domain= (empty) stops publishing it"
	centralMTLSDomainUsage = "vps only: publish central's mutual-TLS listener (the edge surface) at this name, e.g. central-mtls.example.com, for edges in another network. Same routing and limits as --mtls-domain. Remembered; --central-mtls-domain= (empty) stops publishing it"
	centralDomainUsage     = "vps only: serve central's enrollment route at this name, e.g. central.example.com: TLS terminated by Versola's proxy (Let's Encrypt), only POST /edge/enroll, rate limited. It is where `versola edge bootstrap` gets an edge its first certificate. Remembered; --central-domain= (empty) stops publishing it"
)

const certLifetimeUsage = "vps only: how many days the mutual-TLS certificates that versola-tools writes live (auth's and central's listeners, and the identity of the edge on this host): default 365, renewed at a third of the life left by `versola certs renew`, whose daily timer is then installed and required. `unlimited` (or 0) gives them a notAfter 100 years away: they never need renewing, no timer is installed and nothing warns about expiry -- and a leaked key then never expires. Remembered. The edges central issues renew themselves and are not affected"

// publicFlagNames are the flags addPublicFlags registers.
var publicFlagNames = []string{"mtls-domain", "central-mtls-domain", "central-domain", "cert-lifetime-days"}

// addPublicFlags registers --mtls-domain, --central-mtls-domain and
// --central-domain on configure and bootstrap.
func addPublicFlags(c *cobra.Command) {
	c.Flags().String("cert-lifetime-days", "", certLifetimeUsage)
	c.Flags().String("mtls-domain", "", mtlsDomainUsage)
	c.Flags().String("central-mtls-domain", "", centralMTLSDomainUsage)
	c.Flags().String("central-domain", "", centralDomainUsage)
}

// publicDomains resolves the three flags for target: what was recorded for
// the vps deployment this machine tracks, overridden by every flag that was
// passed (an empty value clears that name). The names are not validated
// here: deploy.Configure does, against the auth URL it normalizes.
func publicDomains(cmd *cobra.Command, target string) (deploy.PublicDomains, error) {
	if target != "vps" {
		// rejectVpsOnlyFlags has already refused any flag that was passed.
		return deploy.PublicDomains{}, nil
	}
	p := deploy.StoredPublicDomains()
	for name, to := range map[string]*string{"mtls-domain": &p.MTLS, "central-mtls-domain": &p.CentralMTLS, "central-domain": &p.Enroll} {
		if !cmd.Flags().Changed(name) {
			continue
		}
		v, err := cmd.Flags().GetString(name)
		if err != nil {
			return deploy.PublicDomains{}, err
		}
		*to = v
	}
	return p, nil
}

// certDays resolves --cert-lifetime-days for target: what was recorded for the
// vps deployment, overridden by the flag. nil is the default (365); 0 is
// unlimited. Not given and nothing recorded: nil.
func certDays(cmd *cobra.Command, target string) (*int, error) {
	if target != "vps" {
		return nil, nil
	}
	days := deploy.StoredCertDays()
	if !cmd.Flags().Changed("cert-lifetime-days") {
		return days, nil
	}
	raw, err := cmd.Flags().GetString("cert-lifetime-days")
	if err != nil {
		return nil, err
	}
	return parseCertDays(raw)
}

// parseCertDays reads "unlimited", "0" or a number of days up to a hundred years.
func parseCertDays(raw string) (*int, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "unlimited" || raw == "0" {
		zero := 0
		return &zero, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 36500 {
		return nil, fmt.Errorf("--cert-lifetime-days %q: use a number of days from 1 to 36500, or \"unlimited\"", raw)
	}
	return &n, nil
}

// refuseNoTimerWhenFinite: a finite lifetime needs the renewal timer, so
// --no-certs-timer is refused unless the certificates never expire.
func refuseNoTimerWhenFinite(noTimer bool, days *int) error {
	if noTimer && (days == nil || *days != 0) {
		return fmt.Errorf("--no-certs-timer is refused: the certificates live %s days and expire without `versola certs renew` -- install the timer, or pass --cert-lifetime-days unlimited to have certificates that never expire", certDaysText(days))
	}
	return nil
}

func certDaysText(days *int) string {
	if days == nil {
		return "365"
	}
	return strconv.Itoa(*days)
}
