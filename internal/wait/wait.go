// Package wait polls an HTTP endpoint until it answers 200 or a timeout
// elapses. Used by "bootstrap" to know when a service is actually ready
// to receive traffic, not just that its container has started — see the
// comment on auth's depends_on in versola-tools/compose.fragment.yml.template
// for why "container started" isn't good enough here.
package wait

import (
	"fmt"
	"net/http"
	"time"
)

// ForReady polls url every second until it returns HTTP 200, or returns
// an error once timeout has elapsed without that happening.
func ForReady(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)

	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s to answer 200", timeout, url)
		}
		time.Sleep(1 * time.Second)
	}
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
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)

	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s to answer at all: %w", timeout, url, err)
		}
		time.Sleep(1 * time.Second)
	}
}
