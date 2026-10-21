package cmd

import (
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

// publicFlagNames are the flags addPublicFlags registers.
var publicFlagNames = []string{"mtls-domain", "central-mtls-domain", "central-domain"}

// addPublicFlags registers --mtls-domain, --central-mtls-domain and
// --central-domain on configure and bootstrap.
func addPublicFlags(c *cobra.Command) {
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
