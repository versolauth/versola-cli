// Package proxy sets up the reverse proxy in front of a vps deployment: the
// official nginx image (not a Versola one), with its whole configuration
// generated here, in versola-cli.
//
// Why the CLI and not versola-tools: the proxy is part of *deploying*
// Versola, not of Versola itself -- how it's exposed (nginx or something
// else later, TLS or not, owning the host's ports or sitting behind the
// operator's own proxy) is decided by whoever deploys it, and adding or
// removing replicas behind it (0c/0d) happens on a live server, driven by
// this CLI. Keeping it here also means a change to the proxy needs a CLI
// release, never a Versola release.
//
// The known cost: this package knows Versola's routes (templates/
// routes.conf, copied from versola's nginx.conf.template) -- the one
// place this CLI knows Versola's topology. A Versola release that adds or
// moves a route needs a matching CLI change.
package proxy

import "fmt"

const (
	// ModeNginx: the proxy owns the host's ports 80/443 itself (and does
	// TLS, when the auth URL is https).
	ModeNginx = "nginx"
	// ModeExternal: the host already runs its own web server on 80/443;
	// the proxy listens on 127.0.0.1:ExternalPort behind it and leaves TLS
	// to it.
	ModeExternal = "external"

	// Image is the official nginx image -- 1.30 because
	// ngx_http_acme_module ships in it (since 1.29.1).
	Image = "nginx:1.30-alpine"
	// ContainerName is fixed, like auth/central/edge's.
	ContainerName = "versola-proxy"
	// ServiceName is the proxy's service name in ComposeFile.
	ServiceName = "proxy"
	// ComposeFile is written into the deployment bundle next to
	// versola-tools' compose.yml, and always passed together with it.
	ComposeFile = "proxy.yml"
	// ACMEVolume holds the ACME account key and certificates.
	ACMEVolume = "versola-acme-vps"
	// ExternalPort is where the proxy listens in ModeExternal.
	ExternalPort = 2821

	// ACMEProduction / ACMEStaging: Let's Encrypt's ACME directories.
	// Staging issues untrusted certificates without production's rate
	// limits -- for testing a deployment end to end.
	ACMEProduction = "https://acme-v02.api.letsencrypt.org/directory"
	ACMEStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// ParseMode validates the --proxy flag.
func ParseMode(mode string) (string, error) {
	switch mode {
	case ModeNginx, ModeExternal:
		return mode, nil
	}
	return "", fmt.Errorf("--proxy must be %q or %q, got %q", ModeNginx, ModeExternal, mode)
}

// Ports returns the host ports the proxy binds for this auth URL and mode.
func Ports(a AuthURL, mode string) []int {
	switch {
	case mode == ModeExternal:
		return []int{ExternalPort}
	case a.TLS(mode):
		return []int{80, 443}
	default:
		return []int{80}
	}
}
