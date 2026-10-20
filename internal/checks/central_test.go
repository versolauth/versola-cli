package checks

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubNetwork(t *testing.T, hosts map[string][]string, own ...string) {
	t.Helper()
	oldLookup, oldIfs := lookupHost, interfaceAddrs
	t.Cleanup(func() { lookupHost, interfaceAddrs = oldLookup, oldIfs })
	lookupHost = func(h string) ([]string, error) {
		if a, ok := hosts[h]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, a := range own {
			out = append(out, &net.IPNet{IP: net.ParseIP(a)})
		}
		return out, nil
	}
}

func TestCentralName(t *testing.T) {
	stubNetwork(t, map[string][]string{
		"central.example.com": {"203.0.113.7"},
		"nat.example.com":     {"198.51.100.1"},
	}, "10.0.0.5", "203.0.113.7")

	if r := CentralName("central.example.com"); !r.OK || !strings.Contains(r.Detail, "(this host)") {
		t.Errorf("own address: %+v", r)
	}
	if r := CentralName("missing.example.com"); r.OK || !strings.Contains(r.Detail, "doesn't resolve") {
		t.Errorf("unresolved: %+v", r)
	}
	// Possibly NAT: reported, not failed.
	if r := CentralName("nat.example.com"); !r.OK || !strings.Contains(r.Detail, "NAT") {
		t.Errorf("foreign address: %+v", r)
	}
}

func TestTLSAnswers(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	// What central's listener does: a handshake needs a client certificate.
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MaxVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	if r := TLSAnswers("Central", addr, "central.example.com"); !r.OK {
		t.Errorf("a TLS server demanding a client certificate: %+v", r)
	}

	plain := httptest.NewServer(nil)
	defer plain.Close()
	if r := TLSAnswers("Central", strings.TrimPrefix(plain.URL, "http://"), "x"); r.OK {
		t.Errorf("a plain http server counted as TLS: %+v", r)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	if r := TLSAnswers("Central", closed, "x"); r.OK {
		t.Errorf("a closed port counted as TLS: %+v", r)
	}
}
