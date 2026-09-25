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

// discoveryPath is auth's OIDC discovery document: requesting it through
// the proxy proves the whole path works -- nginx is up, its config loaded
// and it routes to a ready auth -- not just that the container started.
const discoveryPath = "/.well-known/openid-configuration"

// WaitReady waits until the proxy actually serves Versola.
//
// Without TLS: a request for the discovery document through the proxy
// answers 200. With TLS: first the port-80 redirect server answers at all
// (nginx is up), then -- within tlsTimeout -- HTTPS answers 200; if only
// the second part times out it returns an error wrapping ErrTLSPending.
func WaitReady(mode string, a AuthURL, timeout, tlsTimeout time.Duration) error {
	noRedirects := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	if !a.TLS(mode) {
		port := 80
		if mode == ModeExternal {
			port = ExternalPort
		}
		client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: noRedirects}
		url := "http://127.0.0.1:" + strconv.Itoa(port) + discoveryPath
		return poll(timeout, func() error { return expect200(client, url, a.Host) })
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
	if err := poll(tlsTimeout, func() error {
		return expect200(secure, "https://"+a.Host+discoveryPath, a.Host)
	}); err != nil {
		// HTTPS answered, just not with 200: the certificate is there and
		// something else is wrong (routing, auth) -- a real failure, not
		// one to wait out.
		var status *statusError
		if errors.As(err, &status) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrTLSPending, err)
	}
	return nil
}

// statusError: the request got an HTTP answer, just not 200.
type statusError struct {
	url  string
	code int
}

func (e *statusError) Error() string { return fmt.Sprintf("%s answered %d", e.url, e.code) }

func expect200(c *http.Client, url, host string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &statusError{url: url, code: resp.StatusCode}
	}
	return nil
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
