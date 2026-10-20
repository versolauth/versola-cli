package proxy

import (
	"fmt"
	"strings"
)

const (
	// CentralMutualTLSPort is where edges reach central's mutual-TLS listener
	// (MPORT in its compose service), on all interfaces of the host: central
	// itself, or on vps the proxy's front of it (Fronts.CentralMutualTLS),
	// which balances over the replicas listening elsewhere.
	CentralMutualTLSPort = 8093
	// CentralFrontPort is where the proxy's http server listens when the
	// stream block on 443 sits in front of it (see CentralHost): loopback
	// only, never published.
	CentralFrontPort = 8444
	// CentralRelayPort: the stream relay that strips the PROXY protocol
	// header before central's listener, also loopback only.
	CentralRelayPort = 8445
)

// CentralURL is a validated, normalized --central-url: how edge reaches
// central's edge surface (gen-env's central-mutual-tls-url).
type CentralURL struct {
	URL  string // normalized: https://host[:port]
	Host string
	Port string // "" when the URL has none, so 443
}

// DeriveCentralURL is the default --central-url: central.<domain> next to
// the auth URL's host -- its first label replaced, or prepended when the
// host is a bare domain (id.example.com and example.com both give
// central.example.com). "" when there is no domain to derive from: an
// http auth URL (central's listener is mutual TLS, https only), an IP
// address, a single-label host.
func DeriveCentralURL(a AuthURL) string {
	if a.Scheme != "https" || strings.Trim(a.Host, "0123456789.") == "" || !strings.Contains(a.Host, ".") {
		return ""
	}
	labels := strings.Split(a.Host, ".")
	if len(labels) >= 3 {
		labels = labels[1:]
	}
	return "https://central." + strings.Join(labels, ".")
}

// ParseCentralURL validates --central-url, or derives it when raw is empty.
// ok is false when there is none: nothing was given and none can be derived,
// in which case gen-env keeps its own default (https://127.0.0.1:8093).
func ParseCentralURL(raw string, a AuthURL) (c CentralURL, ok bool, err error) {
	if raw == "" {
		raw = DeriveCentralURL(a)
		if raw == "" {
			return CentralURL{}, false, nil
		}
	}
	o, err := parseOrigin("--central-url", raw, ModeExternal)
	if err != nil {
		return CentralURL{}, false, err
	}
	if o.Scheme != "https" {
		return CentralURL{}, false, fmt.Errorf("--central-url %q: central's edge surface is mutual TLS, so the scheme must be https", raw)
	}
	if o.Port == "" && o.Host == a.Host {
		return CentralURL{}, false, fmt.Errorf("--central-url %q: the same host as --auth-url, which the proxy could not tell apart on port 443 -- use a second name, e.g. %s", raw, DeriveCentralURL(a))
	}
	return CentralURL{URL: o.URL, Host: o.Host, Port: o.Port}, true, nil
}

// RoutedHost is the host name the proxy itself routes to central's
// mutual-TLS port by SNI (a stream block on 443, see Config.CentralHost),
// or "" when it doesn't: only the proxy that owns 443 with a certificate of
// its own (ModeNginx, https) can share the port with the TLS it passes
// through, and only a URL on 443 points there. Behind --proxy external the
// operator's own web server has to do it (see the README).
func (c CentralURL) RoutedHost(a AuthURL, mode string) string {
	if c.Port != "" || !a.TLS(mode) {
		return ""
	}
	return c.Host
}
