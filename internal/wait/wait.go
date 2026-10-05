// Package wait polls an HTTP endpoint until it answers 200 or a timeout
// elapses. Used by `configure` and `up` to know when a service is actually ready
// to receive traffic, not just that its container has started — see the
// comment on auth's depends_on in versola-tools/compose.fragment.yml.template
// for why "container started" isn't good enough here.
package wait

import (
	"context"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// attemptTimeout bounds one request, and pollEvery is the pause between
	// requests; both are cut short by the time left.
	attemptTimeout = 3 * time.Second
	pollEvery      = 1 * time.Second

	// minAttempt is the least an attempt is given when the deadline is
	// near or past: a request needs some time to be an attempt at all.
	minAttempt = 250 * time.Millisecond
)

// client is the HTTP client of the probes. They are always to this host
// (localhost, a published address), so it never goes through a proxy from
// HTTP_PROXY: Go bypasses it for loopback only, and a probe to another
// address of this host would otherwise be sent to the proxy and time out.
// It does not follow redirects either: the answer of the address that was
// asked is the answer, not that of wherever it points (a 302 to some other
// server's 200 is not this service being ready).
func client() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// checkURL is an error for a URL that can never be requested: no amount of
// waiting makes "localhost:8081/readiness" (which parses, as scheme
// "localhost") or a URL without a host work.
func checkURL(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil {
		// The parse error repeats the URL, which can carry a password.
		return fmt.Errorf("not a URL that can be requested: %s", strings.ReplaceAll(err.Error(), raw, "<url>"))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL with a host", u.Redacted())
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q has no valid port", u.Redacted())
		}
	}
	return nil
}

// shown is the URL as an error may print it: without a password.
func shown(raw string) string {
	if u, err := neturl.Parse(raw); err == nil {
		return u.Redacted()
	}
	return "<url>"
}

// poll asks url until done says it is satisfied, or the timeout has
// elapsed. It makes at least one attempt, none that outlasts the deadline
// by more than minAttempt, and does not retry a URL that cannot be
// requested at all.
// done gets the response, or the error when there was none, and returns
// whether to stop and a description of what it saw for the failure message.
func poll(url string, timeout time.Duration, done func(*http.Response, error) (bool, string)) (string, error) {
	if err := checkURL(url); err != nil {
		return "", err
	}
	c := client()
	defer c.CloseIdleConnections()
	deadline := time.Now().Add(timeout)

	last := "no answer"
	for {
		budget := min(attemptTimeout, max(time.Until(deadline), minAttempt))
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil { // checked above; kept so a failure can never be a nil request
			cancel()
			return "", err
		}
		resp, err := c.Do(req)
		ok, seen := done(resp, err)
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		if ok {
			return "", nil
		}
		last = seen

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return last, fmt.Errorf("timed out")
		}
		time.Sleep(min(pollEvery, remaining))
	}
}

// ForReady polls url every second until it returns HTTP 200, or returns
// an error once timeout has elapsed without that happening.
func ForReady(url string, timeout time.Duration) error {
	last, err := poll(url, timeout, func(resp *http.Response, err error) (bool, string) {
		if err != nil {
			return false, fmt.Sprintf("last error: %v", err)
		}
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("last answer: %s", resp.Status)
	})
	if err != nil {
		if last == "" {
			return err
		}
		return fmt.Errorf("timed out after %s waiting for %s to answer 200 (%s)", timeout, shown(url), last)
	}
	return nil
}

// ForReachable polls url every second until it answers with *any* HTTP
// response, or returns an error once timeout has elapsed without that
// happening.
//
// This exists separately from ForReady because not every service this
// CLI waits on treats "200" as "up" — OpenBao's health endpoint, for one,
// answers with different status codes for sealed/uninitialized/standby,
// all of which still mean the server itself is up and worth talking to
// (ForReachable is for confirming that much; whether it's sealed is the
// caller's problem to detect from there, not this function's).
func ForReachable(url string, timeout time.Duration) error {
	last, err := poll(url, timeout, func(resp *http.Response, err error) (bool, string) {
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	if err != nil {
		if last == "" {
			return err
		}
		return fmt.Errorf("timed out after %s waiting for %s to answer at all: %s", timeout, shown(url), last)
	}
	return nil
}
