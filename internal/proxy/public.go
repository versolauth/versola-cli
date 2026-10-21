package proxy

import (
	"fmt"
	"strings"
)

const (
	// CentralMutualTLSPort is where edges reach central's mutual-TLS listener
	// (MPORT in its compose service): the proxy's front of it, on the loopback
	// only (Fronts.CentralMutualTLS), balanced over the replicas listening
	// elsewhere. The public relay ends here; nothing else publishes it.
	CentralMutualTLSPort = 8093
	// AuthMutualTLSPort is auth's mutual-TLS port as edge reaches it, the
	// proxy's own internal server (Fronts.MutualTLS balances it over the
	// auth replicas).
	AuthMutualTLSPort = 8083
	// CentralEnrollPort is central's enrollment listener (central's APORT, not auth's 8082, in its
	// compose service): the one route a new edge uses to get its first
	// certificate (POST /edge/enroll). Loopback; the proxy terminates TLS
	// for central.<domain> in front of it.
	CentralEnrollPort = 8094

	// FrontPort is where the proxy's http server listens when the stream
	// block on 443 shares the port (see PublicNames.SharesHTTPS): loopback
	// only, never published.
	FrontPort = 8444
	// CentralRelayPort and AuthRelayPort: the public servers of the stream
	// block, one per mutual-TLS name. They read the PROXY protocol header
	// the 443 server prepends (which the TLS listeners behind would take
	// for a broken handshake), count connections per client address
	// and forward the rest untouched to the internal listener. Loopback only.
	CentralRelayPort = 8445
	AuthRelayPort    = 8446

	// PublicConnLimit is how many connections one client address may hold
	// open to a public mutual-TLS name at once. L4 only: the TLS has not
	// been looked at, so this is all that can be said about a client before
	// its certificate is checked. A remote edge keeps a small pool open;
	// 100 is far above that and far below what exhausts a replica.
	PublicConnLimit = 100
)

// PublicNames are the names the deployment publishes beyond --auth-url's.
// All opt-in: the zero value publishes nothing, and then every mutual-TLS
// listener is reachable on this host's loopback only.
//
//	MTLS         id-mtls.<domain>       auth's mutual-TLS listener (remote
//	                                    edges, external FAPI clients); passed
//	                                    through by SNI, TLS stays end to end
//	CentralMTLS  central-mtls.<domain>  central's edge surface; the same
//	Enroll       central.<domain>       an edge's first certificate: TLS
//	                                    terminated here (public CA), only
//	                                    POST /edge/enroll, rate limited
type PublicNames struct {
	MTLS, CentralMTLS, Enroll string
}

// Any reports whether any name is published.
func (p PublicNames) Any() bool { return p.MTLS != "" || p.CentralMTLS != "" || p.Enroll != "" }

// SharesHTTPS reports whether a mutual-TLS name needs the stream server on
// 443: that port is then shared by TLS the proxy must not touch (these
// names) and TLS it terminates (the auth URL's, and Enroll).
func (p PublicNames) SharesHTTPS() bool { return p.MTLS != "" || p.CentralMTLS != "" }

// Relay is a public mutual-TLS name with where its relay listens and what
// it forwards to.
type Relay struct {
	Name  string
	Label string // "auth" or "central", for the log
	Port  int    // the relay's loopback port
	To    int    // the internal listener it forwards to
}

// Relays are the names SharesHTTPS routes, in a fixed order.
func (p PublicNames) Relays() []Relay {
	var out []Relay
	if p.MTLS != "" {
		out = append(out, Relay{Name: p.MTLS, Label: "auth", Port: AuthRelayPort, To: AuthMutualTLSPort})
	}
	if p.CentralMTLS != "" {
		out = append(out, Relay{Name: p.CentralMTLS, Label: "central", Port: CentralRelayPort, To: CentralMutualTLSPort})
	}
	return out
}

// LoopbackPorts are the loopback ports the proxy binds for these names
// (the 443 server's http fallback and the relays): all must be free.
func (p PublicNames) LoopbackPorts() []int {
	if !p.SharesHTTPS() {
		return nil
	}
	ports := []int{FrontPort}
	for _, r := range p.Relays() {
		ports = append(ports, r.Port)
	}
	return ports
}

// ParsePublicNames validates the three names against the auth URL and each
// other. Empty ones stay empty. A name is a bare, lowercase DNS name, with
// no scheme, port or path: SNI carries nothing else, and it ends up in
// nginx's configuration.
//
// Every name needs --auth-url to be https: the mutual-TLS names' leaf
// certificates and Let's Encrypt's for central.<domain> both mean a real
// domain. (Behind --proxy external the mutual-TLS names are still used for
// the certificates' names; routing them is the operator's, see the README.)
func ParsePublicNames(mtls, centralMTLS, enroll string, a AuthURL) (PublicNames, error) {
	p := PublicNames{}
	seen := map[string]string{a.Host: "--auth-url"}
	for _, n := range []struct {
		flag, raw string
		to        *string
	}{
		{"--mtls-domain", mtls, &p.MTLS},
		{"--central-mtls-domain", centralMTLS, &p.CentralMTLS},
		{"--central-domain", enroll, &p.Enroll},
	} {
		if n.raw == "" {
			continue
		}
		name, err := parseBareDomain(n.flag, n.raw)
		if err != nil {
			return PublicNames{}, err
		}
		if a.Scheme != "https" {
			return PublicNames{}, fmt.Errorf("%s needs an https --auth-url: the name is served with a certificate for a real domain", n.flag)
		}
		if other, dup := seen[name]; dup {
			return PublicNames{}, fmt.Errorf("%s %q is already %s's name: SNI could not tell them apart", n.flag, name, other)
		}
		seen[name] = n.flag
		*n.to = name
	}
	return p, nil
}

// parseBareDomain: a host name (letters, digits, '.', '-'), with at least
// two labels and not an IP address, in lowercase.
func parseBareDomain(flag, raw string) (string, error) {
	name := strings.ToLower(strings.TrimSuffix(raw, "."))
	if strings.Contains(name, "://") || strings.ContainsAny(name, "/:@ ") {
		return "", fmt.Errorf("%s %q: give the bare host name (e.g. id-mtls.example.com), without scheme, port or path", flag, raw)
	}
	// parseOrigin holds the rules for a host (allowed characters, label and
	// total length) -- one definition for all of them.
	o, err := parseOrigin(flag, "https://"+name, ModeExternal)
	if err != nil {
		return "", err
	}
	if strings.Trim(o.Host, "0123456789.") == "" {
		return "", fmt.Errorf("%s %q: an IP address has no SNI name -- use a domain", flag, raw)
	}
	if !strings.Contains(o.Host, ".") || o.Host == "localhost" || strings.HasSuffix(o.Host, ".localhost") {
		return "", fmt.Errorf("%s %q: needs a public domain name (e.g. id-mtls.example.com)", flag, raw)
	}
	return o.Host, nil
}
