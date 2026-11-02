package checks

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// AuthName checks the host of --auth-url before a vps deployment: it has to
// resolve, and to this host. acme says whether Versola's proxy gets the
// certificate from Let's Encrypt itself (--proxy nginx with an https URL):
// Let's Encrypt then reaches this server through that name, on every address
// it resolves to, so an address that is not this host's is spelled out.
//
// "Not this host" is a warning, not a failure: a server behind NAT, or behind
// a CDN or a load balancer, is reached at an address none of its interfaces
// has, which only the operator can tell apart from a wrong record (the same
// rule as NameResolves).
func AuthName(host string, acme bool) Result {
	name := "Auth URL name " + host
	addrs, err := lookupHost(host)
	if err != nil || len(addrs) == 0 {
		return Result{Name: name, Detail: "doesn't resolve -- add a DNS record for it (A, and AAAA if the server has IPv6) pointing at this server"}
	}
	own := ownAddresses()
	var ours, foreign []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && own[ip.String()] {
			ours = append(ours, a)
		} else {
			foreign = append(foreign, a)
		}
	}
	consequence := ""
	if acme {
		consequence = "; otherwise Let's Encrypt will not issue the certificate until the record points here"
	}
	switch {
	case len(foreign) == 0:
		return Result{Name: name, OK: true, Detail: "resolves to " + strings.Join(ours, ", ") + " (this host)"}
	case len(ours) == 0:
		return Result{Name: name, OK: true, Warn: true, Detail: fmt.Sprintf("resolves to %s, which is none of this host's addresses -- right only if this server is behind NAT (or a load balancer) at that address%s", strings.Join(foreign, ", "), consequence)}
	default:
		stale := ""
		if acme {
			stale = " Let's Encrypt may try that address and fail;"
		}
		return Result{Name: name, OK: true, Warn: true, Detail: fmt.Sprintf("resolves to %s (this host), but also to %s, which is none of this host's addresses --%s remove that record if it is stale", strings.Join(ours, ", "), strings.Join(foreign, ", "), stale)}
	}
}

// ownAddresses are this host's interface addresses, as strings.
func ownAddresses() map[string]bool {
	own := map[string]bool{}
	if ifs, err := interfaceAddrs(); err == nil {
		for _, a := range ifs {
			if ipn, ok := a.(*net.IPNet); ok {
				own[ipn.IP.String()] = true
			}
		}
	}
	return own
}

// registryClient is what RegistryAnswers asks with, replaceable in tests. It
// takes HTTPS_PROXY from the environment, like docker's CLI does.
var registryClient = &http.Client{Timeout: 10 * time.Second}

// RegistryAnswers checks that this host reaches the registry the images are
// pulled from (host, e.g. ghcr.io) over HTTPS, before the first pull: a
// server whose outbound HTTPS is closed otherwise finds out halfway through
// `configure`. Any HTTP answer is enough (the registry API answers 401 to a
// request without a token); no answer at all is a failure.
func RegistryAnswers(host string) Result {
	return registryAnswersAt(host, "https://"+host+"/v2/")
}

func registryAnswersAt(host, url string) Result {
	name := "Registry " + host
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return Result{Name: name, Detail: err.Error()}
	}
	resp, err := registryClient.Do(req)
	if err != nil {
		return Result{Name: name, Detail: fmt.Sprintf("no answer over HTTPS (%v) -- Versola's images are pulled from there: allow outbound HTTPS (port 443) from this server, or use a registry it can reach with --registry. (Docker pulling through a proxy set for its daemon is not seen by this check: `docker pull` has the last word)", err)}
	}
	_ = resp.Body.Close()
	return Result{Name: name, OK: true, Detail: fmt.Sprintf("answers over HTTPS (%s)", resp.Status)}
}
