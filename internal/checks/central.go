package checks

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"
)

// lookupHost and interfaceAddrs are the network's answers, replaceable in
// tests.
var (
	lookupHost     = func(host string) ([]string, error) { return net.DefaultResolver.LookupHost(context.Background(), host) }
	interfaceAddrs = net.InterfaceAddrs
)

// CentralName checks that central's public name resolves, and to this
// host: the proxy routes it by SNI, so the name has to lead here. A server
// behind NAT has a public address that is none of its own, which is told
// apart from a wrong record only by the operator -- reported, not failed.
func CentralName(host string) Result { return NameResolves("Central name", host) }

// NameResolves is CentralName for any published name, labelled by what it is
// for: it has to resolve, and to this host.
func NameResolves(label, host string) Result {
	name := label + " " + host
	addrs, err := lookupHost(host)
	if err != nil || len(addrs) == 0 {
		return Result{Name: name, OK: false, Detail: "doesn't resolve -- add a DNS record for it pointing at this server"}
	}
	own := map[string]bool{}
	if ifs, err := interfaceAddrs(); err == nil {
		for _, a := range ifs {
			if ipn, ok := a.(*net.IPNet); ok {
				own[ipn.IP.String()] = true
			}
		}
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && own[ip.String()] {
			return Result{Name: name, OK: true, Detail: "resolves to " + a + " (this host)"}
		}
	}
	return Result{Name: name, OK: true, Detail: "resolves to " + strings.Join(addrs, ", ") + ", which is none of this host's addresses -- right only if this server is behind NAT and that is its public address"}
}

// TLSAnswers checks that something at addr answers a TLS handshake for
// serverName. Central's listener wants a client certificate, so a handshake
// the server refuses for the missing one still proves a TLS server is
// there; a connection that is refused, times out or isn't TLS does not.
// The certificate is not verified: it is central's own, pinned by edge.
func TLSAnswers(label, addr, serverName string) Result {
	name := label + " " + addr
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) //nolint:gosec // see above
	if err == nil {
		_ = conn.Close()
		return Result{Name: name, OK: true, Detail: "answers a TLS handshake"}
	}
	if strings.Contains(err.Error(), "remote error: tls:") {
		return Result{Name: name, OK: true, Detail: "answers a TLS handshake (and asks for a client certificate)"}
	}
	return Result{Name: name, OK: false, Detail: fmt.Sprintf("no TLS handshake: %v", err)}
}
