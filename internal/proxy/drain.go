package proxy

import (
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
)

const (
	// DrainTimeout bounds the wait after a reload: nginx itself stops its
	// old workers at worker_shutdown_timeout (nginx.conf), so waiting
	// longer would only be waiting for nothing.
	DrainTimeout = 30 * time.Second

	drainEvery = 500 * time.Millisecond
	// drainSettle: `nginx -s reload` only signals the master; the old
	// workers are not marked as shutting down until it has acted on it.
	drainSettle = time.Second
	// drainFallback is how long to wait when the proxy cannot be asked
	// which workers are still finishing (no `ps` in the image).
	drainFallback = 10 * time.Second
)

// WaitDrained waits until the workers a reload retired have finished the
// requests they were serving. After a replica has been taken out of the
// upstream and nginx reloaded, requests already sent to it are still being
// answered by those old workers; it is safe to stop the replica once they
// are gone.
//
// Asked with `ps` inside the proxy container: an old worker shows up as
// "nginx: worker process is shutting down". If `ps` cannot be run, a fixed
// wait stands in.
func WaitDrained(mode string) {
	name := ContainerFor(mode)
	ps := func() (string, error) {
		out, err := docker.Output("exec", name, "ps")
		return string(out), err
	}
	waitDrained(ps, DrainTimeout, drainEvery, drainFallback, time.Sleep)
}

func waitDrained(ps func() (string, error), max, every, fallback time.Duration, sleep func(time.Duration)) {
	sleep(drainSettle)
	var waited time.Duration
	for waited < max {
		out, err := ps()
		if err != nil {
			sleep(min(fallback, max-waited))
			return
		}
		if !strings.Contains(out, "shutting down") {
			return
		}
		sleep(every)
		waited += every
	}
}
