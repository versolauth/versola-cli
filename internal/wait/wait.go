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
