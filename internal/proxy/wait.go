package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ErrTLSPending: the proxy is up and answering on port 80, but HTTPS
// isn't served yet -- normally because the ACME module is still getting
// the certificate (DNS not pointing here yet, Let's Encrypt slow, rate
// limited...). Not fatal: the module keeps retrying on its own.
var ErrTLSPending = errors.New("the proxy is up, but HTTPS isn't being served yet")

// probe is one request through the proxy and the status that proves the
// upstream behind it is serving -- not just that the proxy is up.
type probe struct {
	what   string
	path   string
	status int
}

// probes: one per kind of upstream the proxy routes to, so a proxy that
// answers but can't reach one of them isn't taken for ready.
//
//   - auth: its OIDC discovery document.
//   - edge: /permissions/me without a token. Edge itself answers 401; the
//     proxy's own answers would be 502/504 (edge not reachable) or 404 (no
//     route). Read-only, unlike /login/<preset>, which stores a pending
//     login on every request.
//   - the admin console: static files the proxy serves itself.
var probes = []probe{
	{"auth", "/.well-known/openid-configuration", http.StatusOK},
	{"edge", "/permissions/me", http.StatusUnauthorized},
	{"the admin console", "/central/admin/", http.StatusOK},
}

// WaitReady waits until the proxy actually serves Versola: every probe,
// through the proxy, answers its expected status.
//
// Without TLS: the probes over plain HTTP. With TLS: first the port-80
// redirect server answers at all (nginx is up), then -- within tlsTimeout --
// the probes over HTTPS; if only the second part times out it returns an
// error wrapping ErrTLSPending.
func WaitReady(mode string, a AuthURL, timeout, tlsTimeout time.Duration) error {
	noRedirects := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	if !a.TLS(mode) {
		port := 80
		if mode == ModeExternal || mode == ModeLocal {
			port = Ports(a, mode)[0]
		}
		client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: noRedirects}
		base := "http://127.0.0.1:" + strconv.Itoa(port)
		return poll(timeout, func() error {
			_, err := checkProbes(client, base, a.Host)
			return err
		})
	}

	plain := &http.Client{Timeout: 3 * time.Second, CheckRedirect: noRedirects}
	if err := poll(timeout, func() error {
		resp, err := plain.Get("http://127.0.0.1:80/")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}); err != nil {
		return fmt.Errorf("the proxy never answered on port 80: %w", err)
	}

	// HTTPS is dialed on 127.0.0.1 with the real host name as SNI, so this
	// works before (or without) public DNS pointing here. Certificate
	// verification is off on purpose: this only asks "is this local nginx
	// serving HTTPS yet", and a staging certificate (--acme-staging) is
	// untrusted by design. Nothing sensitive is sent.
	secure := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: noRedirects,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, "127.0.0.1:443")
			},
			TLSClientConfig: &tls.Config{ServerName: a.Host, InsecureSkipVerify: true}, //nolint:gosec // local readiness probe, see above
		},
	}
	// Once HTTPS has answered at all, the certificate is there: whatever
	// still fails -- a probe's status, or a backend too slow to answer in
	// time -- is a real failure, not one to wait out.
	served := false
	if err := poll(tlsTimeout, func() error {
		answered, err := checkProbes(secure, "https://"+a.Host, a.Host)
		served = served || answered
		return err
	}); err != nil {
		var status *statusError
		if served || errors.As(err, &status) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrTLSPending, err)
	}
	return nil
}

// statusError: the request got an HTTP answer, just not the expected one.
type statusError struct {
	what string
	url  string
	code int
	want int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s isn't served yet: %s answered %d, want %d", e.what, e.url, e.code, e.want)
}

// checkProbes runs every probe against base (scheme://address, no path),
// sending host as the Host header, and returns the first one that fails.
// answered: at least one probe got an HTTP answer, whatever its status.
func checkProbes(c *http.Client, base, host string) (answered bool, err error) {
	for _, p := range probes {
		got, err := expect(c, p, base+p.path, host)
		answered = answered || got
		if err != nil {
			return answered, err
		}
	}
	return answered, nil
}

// expect: answered is whether the request got an HTTP answer at all.
func expect(c *http.Client, p probe, url, host string) (answered bool, err error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	if resp.StatusCode != p.status {
		return true, &statusError{what: p.what, url: url, code: resp.StatusCode, want: p.status}
	}
	return true, nil
}

func poll(timeout time.Duration, try func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := try()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		time.Sleep(time.Second)
	}
}

// WaitFronts waits until the proxy serves its fronts of auth's APORT and
// MPORT (vps):
//
//   - APORT: an HTTP answer through the proxy that is not its own "nothing
//     behind me" (502, 504) -- whatever auth answers on its path, it is
//     auth answering.
//   - MPORT: the proxy accepts a TCP connection there -- which proves the
//     proxy listens, not that an auth replica answers behind it. No TLS
//     handshake: TLS is auth's, and a release without the mutual-TLS
//     listener has nothing behind it (nothing calls it then either).
func WaitFronts(f Fronts, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return poll(timeout, func() error { return checkFronts(client, f) })
}

func checkFronts(client *http.Client, f Fronts) error {
	url := "http://127.0.0.1:" + strconv.Itoa(f.Additional) + "/"
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("auth's APORT through the proxy (%s): %w", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
		return fmt.Errorf("auth's APORT through the proxy (%s) answered %d: no auth replica behind it", url, resp.StatusCode)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.MutualTLS))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("the proxy doesn't accept connections in front of auth's MPORT (%s): %w", addr, err)
	}
	conn.Close()
	return nil
}
