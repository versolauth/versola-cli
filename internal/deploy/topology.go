package deploy

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// The compose services this CLI starts and waits on itself. Their names
// are the one thing still assumed about versola-tools' compose file; what
// they listen on is read from it (loadTopology).
const (
	CentralService = "central"
	AuthService    = "auth"
	EdgeService    = "edge"
)

// replica is one running (or about to run) copy of auth or edge.
type replica struct {
	Slot    int
	Service string           // compose service name: "auth", "auth-2", ...
	Ports   topology.Service // this replica's own ports (shifted for its slot)
}

// loadTopology reads the ports out of the deployment's compose file
// (together with proxy.yml, when there is one) through Compose itself, so
// ${VERSION} and the like are already resolved. The output is parsed and
// dropped; it is not logged or stored, since Compose inlines env_file
// values into it.
func loadTopology(composePath string) (topology.Topology, error) {
	out, err := docker.Output(state.ComposeArgs(composePath, "config", "--format", "json")...)
	if err != nil {
		return topology.Topology{}, composeConfigError(err)
	}
	return topology.Parse(out)
}

// composeConfigError explains a failed `docker compose config`. Compose's
// stderr is quoted only as its first line, shortened: that is where the
// reason is, and a longer excerpt of a parse error could carry a piece of
// an env file -- a secret.
func composeConfigError(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("couldn't read the compose configuration: %w", err)
	}
	msg := firstLine(string(exitErr.Stderr), 300)
	if strings.Contains(msg, "unknown flag") {
		return fmt.Errorf("couldn't read the compose configuration: this Docker Compose is too old for `config --format json` -- update the Compose plugin (%s)", msg)
	}
	if msg == "" {
		return fmt.Errorf("couldn't read the compose configuration: %w", err)
	}
	return fmt.Errorf("couldn't read the compose configuration: %w: %s", err, msg)
}

// firstLine returns the first non-empty line of s, cut to max characters.
func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > max {
			return string(r[:max]) + "..."
		}
		return line
	}
	return ""
}

// checkNoPortClash refuses replicas that would listen on the same port.
// A service whose compose entry sets no PORT or DPORT silently gets the
// application's default (topology.DefaultPort...), which is another
// service's port; reading the compose file instead of hardcoding must not
// turn that into a deployment that starts and then misroutes.
func checkNoPortClash(groups ...[]replica) error {
	owner := map[int]string{}
	for _, group := range groups {
		for _, r := range group {
			for _, p := range []int{r.Ports.Port, r.Ports.DiagnosticsPort} {
				if other, dup := owner[p]; dup {
					if other == r.Service {
						return fmt.Errorf("%s uses port %d for both PORT and DPORT -- check the compose file", r.Service, p)
					}
					return fmt.Errorf("%s and %s would both listen on port %d -- check PORT/DPORT in the compose file", other, r.Service, p)
				}
				owner[p] = r.Service
			}
		}
	}
	return nil
}

// appUpArgs are the arguments of the `compose up` that starts the app
// services (every auth and edge replica). legacyGateway adds versola-tools'
// own nginx, for a local deployment made before this CLI generated the
// proxy.
func appUpArgs(services []string, legacyGateway bool) []string {
	args := append([]string{"up", "-d"}, services...)
	if legacyGateway {
		args = append(args, "nginx")
	}
	return args
}

// replicasOf returns the replicas of one service for the given slots.
func replicasOf(topo topology.Topology, service string, slots []state.Slot) ([]replica, error) {
	base, err := topo.Service(service)
	if err != nil {
		return nil, err
	}
	out := make([]replica, 0, len(slots))
	for _, s := range slots {
		ports, err := base.ForSlot(s.N)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", service, err)
		}
		out = append(out, replica{Slot: s.N, Service: topology.SlotService(service, s.N), Ports: ports})
	}
	return out, nil
}

// readinessURL is where a replica reports whether it is ready, as seen
// from the host this CLI runs on: its diagnostics port, published (local)
// or the host's own (vps).
func readinessURL(r topology.Service) string {
	return fmt.Sprintf("http://localhost:%d/readiness", r.HostPort(r.DiagnosticsPort))
}

// serviceNames lists the compose service names of the replicas.
func serviceNames(replicas ...[]replica) []string {
	var names []string
	for _, group := range replicas {
		for _, r := range group {
			names = append(names, r.Service)
		}
	}
	return names
}

// proxyBackends is what the proxy's upstream routes to: each replica's
// main port.
func proxyBackends(replicas []replica) []proxy.Backend {
	backends := make([]proxy.Backend, len(replicas))
	for i, r := range replicas {
		backends[i] = proxy.Backend{Service: r.Service, Port: r.Ports.Port}
	}
	return backends
}
