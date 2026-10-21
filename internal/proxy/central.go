package proxy

import (
	"fmt"
)

// CentralURL is a validated, normalized --central-url: how edge reaches
// central's edge surface (gen-env's central-mutual-tls-url). Only ever an
// explicit value: nothing is derived from the auth URL (see PublicNames for
// the names the proxy publishes, which are a separate, opt-in decision).
type CentralURL struct {
	URL  string // normalized: https://host[:port]
	Host string
	Port string // "" when the URL has none, so 443
}

// ParseCentralURL validates --central-url. ok is false when raw is empty:
// gen-env then keeps its own default (https://127.0.0.1:8093, the loopback
// address a local edge reaches central on -- by address, never by a public
// name, so a local edge never leaves the host to reach it).
func ParseCentralURL(raw string, a AuthURL) (c CentralURL, ok bool, err error) {
	if raw == "" {
		return CentralURL{}, false, nil
	}
	o, err := parseOrigin("--central-url", raw, ModeExternal)
	if err != nil {
		return CentralURL{}, false, err
	}
	if o.Scheme != "https" {
		return CentralURL{}, false, fmt.Errorf("--central-url %q: central's edge surface is mutual TLS, so the scheme must be https", raw)
	}
	if o.Port == "" && o.Host == a.Host {
		return CentralURL{}, false, fmt.Errorf("--central-url %q: the same host as --auth-url, which serves the web UI, not central's mutual-TLS listener -- use central-mtls.<domain> (see --central-mtls-domain)", raw)
	}
	return CentralURL{URL: o.URL, Host: o.Host, Port: o.Port}, true, nil
}
