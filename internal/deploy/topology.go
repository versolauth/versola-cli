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
	Base    string           // the service it is a copy of: "auth", "edge", "central"
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
		return topology.Topology{}, composeConfigError(composePath, err)
	}
	return topology.Parse(out)
}

// composeConfigError explains a failed `docker compose config`.
//
// Compose's stderr is never quoted. Its parse errors for env files repeat
// the offending text (an unterminated quote in auth.secrets.env comes back
// as "unterminated quoted value <the rest of the line>"), so any excerpt,
// however short, can carry a secret into the terminal and the logs. The
// causes worth naming are recognised from the stderr and answered with a
// fixed sentence; for everything else the user is pointed at the command to
// run themselves, where the output goes only where they send it.
func composeConfigError(composePath string, err error) error {
	const prefix = "couldn't read the compose configuration"
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	stderr := strings.ToLower(string(exitErr.Stderr))
	switch {
	case strings.Contains(stderr, "unknown flag"):
		return fmt.Errorf("%s: this Docker Compose is too old for `config --format json` -- update the Compose plugin", prefix)
	case strings.Contains(stderr, "docker daemon") || strings.Contains(stderr, "error during connect"):
		return fmt.Errorf("%s: can't reach Docker -- is it running?", prefix)
	}
	return fmt.Errorf("%s (%w); run `docker %s` to see why -- its output can include values from your env files, so mind where you paste it", prefix, err, shellJoin(state.ComposeArgs(composePath, "config")))
}

// shellJoin renders args as a command line to paste into a shell, quoting
// those with spaces or other characters a shell would split or expand.
func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$`\\&;|<>()*?[]{}!#~") {
			a = `"` + strings.NewReplacer(`"`, `\"`, "`", "\\`", `$`, `\$`).Replace(a) + `"`
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// listener is one port a replica's process opens, named by the environment
// variable that sets it.
type listener struct {
	Name string
	Port int
}

// listeners are the ports a replica's process binds: PORT and DPORT
// everywhere, and APORT for auth, the only service with an additional
// listener. (topology.Service carries a default APORT for every service, but
// edge and central never bind it; counting it would report auth's APORT as
// clashing with them.)
func (r replica) listeners() []listener {
	ls := []listener{{"PORT", r.Ports.Port}, {"DPORT", r.Ports.DiagnosticsPort}}
	if r.Base == AuthService {
		ls = append(ls, listener{"APORT", r.Ports.AdditionalPort})
	}
	return ls
}

// checkNoPortClash refuses replicas that would collide on a port. A
// service whose compose entry sets no PORT or DPORT silently gets the
// application's default (topology.DefaultPort...), which is another
// service's port; reading the compose file instead of hardcoding must not
// turn that into a deployment that starts and then misroutes.
//
// What collides depends on the network. Within one replica the listeners
// are one process's and must always differ. Between replicas, hostNetwork
// (vps: network_mode: host) puts every container on the host's network, so
// any two container ports must differ; on a bridge (local) each container
// has its own, auth and edge may both listen on 8080 in theirs, and only
// the host ports that `ports:` publishes can collide.
func checkNoPortClash(hostNetwork bool, groups ...[]replica) error {
	type claim struct{ service, listener string }
	owner := map[int]claim{}
	for _, group := range groups {
		for _, r := range group {
			ls := r.listeners()
			own := map[int]string{}
			for _, l := range ls {
				if prev, dup := own[l.Port]; dup {
					return fmt.Errorf("%s uses port %d for both %s and %s -- check the compose file", r.Service, l.Port, prev, l.Name)
				}
				own[l.Port] = l.Name
			}
			for _, l := range ls {
				hostPort, occupies := l.Port, true
				if !hostNetwork {
					hostPort, occupies = r.Ports.Published(l.Port)
				}
				if !occupies {
					continue
				}
				if other, dup := owner[hostPort]; dup {
					hint := "PORT/DPORT/APORT"
					if !hostNetwork {
						hint += " and `ports:`"
					}
					return fmt.Errorf("%s (%s) and %s (%s) would both use port %d on the host -- check %s in the compose file",
						other.service, other.listener, r.Service, l.Name, hostPort, hint)
				}
				owner[hostPort] = claim{r.Service, l.Name}
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
		out = append(out, replica{Slot: s.N, Base: service, Service: topology.SlotService(service, s.N), Ports: ports})
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
