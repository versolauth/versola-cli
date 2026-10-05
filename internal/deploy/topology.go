package deploy

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
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

// reservation is a host port something other than the replicas holds, the
// reverse proxy's for one, that no replica may use.
type reservation struct {
	Name    string // who holds it, as it reads in an error
	Binding topology.Binding
}

// proxyHolder is how the reverse proxy is named in an error.
const proxyHolder = "Versola's reverse proxy"

// openbaoHolder and openbaoPort: configure starts OpenBao on this machine
// at localhost:8200 (a published port locally, the host's own on vps), and
// it stays there while the stack runs. Every replica, whatever its target,
// must stay off that port.
const (
	openbaoHolder = "Versola's OpenBao"
	openbaoPort   = 8200
)

// proxyReservations are the host ports the reverse proxy binds: loopback
// only for local and external, every interface for a proxy that serves
// 80/443 itself.
func proxyReservations(ports []int, mode string) []reservation {
	ip := ""
	if mode == proxy.ModeLocal || mode == proxy.ModeExternal {
		ip = "127.0.0.1"
	}
	out := make([]reservation, len(ports))
	for i, p := range ports {
		out[i] = reservation{Name: proxyHolder, Binding: topology.Binding{HostIP: ip, Port: p}}
	}
	return out
}

// reservationsFor is what the proxy of a deployment with this auth URL and
// mode holds on the host. configure and up both ask this one place.
func reservationsFor(auth proxy.AuthURL, mode string) []reservation {
	return proxyReservations(proxy.Ports(auth, mode), mode)
}

// checkNoPortClash refuses replicas that would collide on a port. A
// service whose compose entry sets no PORT or DPORT silently gets the
// application's default (topology.DefaultPort...), which is another
// service's port; reading the compose file instead of hardcoding must not
// turn that into a deployment that starts and then misroutes.
//
// What collides depends on the network. Within one replica the listeners
// are one process's and must always differ. Between replicas, a service on
// the host's network (hostNetwork -- vps -- or `network_mode: host` in the
// compose file) takes the host's own ports, so any two container ports
// must differ; on a bridge (local) each container has its own, auth and
// edge may both listen on 8080 in theirs, and only what `ports:` publishes
// on the host can collide -- every published port, not just the three
// listeners, and per address: 127.0.0.1:8080 and 127.0.0.2:8080 are
// different sockets. reserved are host ports of the proxy that no replica
// may take, whatever its network.
func checkNoPortClash(hostNetwork bool, reserved []reservation, groups ...[]replica) error {
	type claim struct {
		service, label string
		base           string // the service it is a copy of, for the slot note
		slot           int
		reserved       bool // held by something other than a replica
		onHost         bool // a host-network listener, as opposed to a publication
		binding        topology.Binding
	}
	var claims []claim
	// The address OpenBao binds is not in the compose file this reads, so
	// the port is held on every address.
	claims = append(claims, claim{service: openbaoHolder, reserved: true, binding: topology.Binding{Port: openbaoPort}})
	for _, r := range reserved {
		claims = append(claims, claim{service: r.Name, reserved: true, binding: r.Binding})
	}
	for _, group := range groups {
		for _, r := range group {
			ls := r.listeners()
			own := map[int]string{}
			for _, l := range ls {
				if prev, dup := own[l.Port]; dup {
					return fmt.Errorf("%s uses port %d for both %s and %s%s -- check the compose file",
						r.Service, l.Port, prev, l.Name, slotNote(r.Service, r.Base, r.Slot))
				}
				own[l.Port] = l.Name
			}

			var taken []claim // what this replica occupies on the host
			if hostNetwork || r.Ports.HostNetwork() {
				for _, l := range ls {
					taken = append(taken, claim{service: r.Service, label: l.Name, base: r.Base, slot: r.Slot, onHost: true, binding: topology.Binding{Port: l.Port}})
				}
			} else {
				for _, p := range r.Ports.Publications() {
					taken = append(taken, claim{service: r.Service, label: r.labelOf(p.Target), base: r.Base, slot: r.Slot, binding: p.Binding})
				}
			}
			for _, t := range taken {
				for _, other := range claims {
					if !other.binding.Overlaps(t.binding) {
						continue
					}
					// Docker takes whichever port of a range is free, so a
					// range clashes with nothing it merely contains.
					if other.binding.IsRange() || t.binding.IsRange() {
						continue
					}
					var msg string
					switch {
					case other.reserved:
						msg = fmt.Sprintf("%s (%s) would use %s on the host, which %s holds", t.service, t.label, t.binding, other.service)
					case other.onHost && t.onHost:
						msg = fmt.Sprintf("%s (%s) and %s (%s) would both use port %d on the host", other.service, other.label, t.service, t.label, t.binding.Port)
					case other.service == t.service:
						msg = fmt.Sprintf("%s publishes %s for both %s and %s", t.service, t.binding, other.label, t.label)
					default:
						msg = fmt.Sprintf("%s (%s) %s and %s (%s) %s, which is the same port on the host",
							other.service, other.label, publishes(other.onHost, other.binding),
							t.service, t.label, publishes(t.onHost, t.binding))
					}
					msg += slotNote(t.service, t.base, t.slot)
					if other.service != t.service {
						msg += slotNote(other.service, other.base, other.slot)
					}

					// Name what to look at: the listener variables when a
					// listener is involved, `ports:` when a publication is.
					var hints []string
					listener, publication := false, false
					for _, cl := range []claim{other, t} {
						if cl.reserved {
							continue
						}
						listener = listener || cl.onHost || !strings.HasPrefix(cl.label, "port ")
						publication = publication || !cl.onHost
					}
					if listener {
						hints = append(hints, "PORT/DPORT/APORT")
					}
					if publication {
						hints = append(hints, "`ports:`")
					}
					return fmt.Errorf("%s -- check %s in the compose file", msg, strings.Join(hints, " and "))
				}
				claims = append(claims, t)
			}
		}
	}

	// A range clashes with no single port inside it, but Docker still needs
	// a free port in it for each: 18080-18081 beside a fixed 18080 and a
	// fixed 18081 has none left.
	var fixed, ranges []claim
	for _, c := range claims {
		if c.binding.IsRange() {
			ranges = append(ranges, c)
		} else {
			fixed = append(fixed, c)
		}
	}
	// Narrowest end first, lowest free port: the order in which that greedy
	// choice is the best one for ranges that share an address.
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].binding.Last < ranges[j].binding.Last })
	var assigned []topology.Binding
	for _, r := range ranges {
		found := false
		for p := r.binding.Port; p <= r.binding.Last && !found; p++ {
			b := topology.Binding{HostIP: r.binding.HostIP, Port: p}
			free := true
			for _, f := range fixed {
				if f.binding.Overlaps(b) {
					free = false
					break
				}
			}
			for _, a := range assigned {
				if free && a.Overlaps(b) {
					free = false
				}
			}
			if free {
				assigned = append(assigned, b)
				found = true
			}
		}
		if !found {
			who := r.service
			if r.label != "" {
				who += " (" + r.label + ")"
			}
			hint := ""
			if !r.reserved {
				hint = " -- check `ports:` in the compose file"
			}
			return fmt.Errorf("%s publishes %s, but every port in that range is taken by another publication%s%s", who, r.binding, slotNote(r.service, r.base, r.slot), hint)
		}
	}
	return nil
}

// slotNote explains a number that is not the one in the compose file: a
// replica in slot n listens on its service's ports plus 100*(n-1).
func slotNote(service, base string, slot int) string {
	if slot <= 1 {
		return ""
	}
	return fmt.Sprintf(" (%s is slot %d: its ports are %s's in the compose file plus %d)", service, slot, base, topology.SlotOffset(slot))
}

// publishes says what a claim does with a port: a host-network service
// listens on it, any other publishes it.
func publishes(onHost bool, b topology.Binding) string {
	if onHost {
		return fmt.Sprintf("listens on %s", b)
	}
	return fmt.Sprintf("publishes %s", b)
}

// labelOf names a container port by the variable that sets it, or as a
// plain port when it is none of the replica's listeners (MPORT, say).
func (r replica) labelOf(containerPort int) string {
	for _, l := range r.listeners() {
		if l.Port == containerPort {
			return l.Name
		}
	}
	return fmt.Sprintf("port %d", containerPort)
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

// replicasOf returns the replicas of one service for the given slots, in
// slot order. Slots come from state.json: a slot recorded twice is that
// file's mistake, and is reported as that, not as a clash of ports.
func replicasOf(topo topology.Topology, service string, slots []state.Slot) ([]replica, error) {
	base, err := topo.Service(service)
	if err != nil {
		return nil, err
	}
	// Only auth binds APORT; for the others a bad one means nothing.
	if service == AuthService {
		if err := base.CheckAdditional(service); err != nil {
			return nil, err
		}
	}
	slots = append([]state.Slot(nil), slots...)
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].N < slots[j].N })
	for i := 1; i < len(slots); i++ {
		if slots[i].N == slots[i-1].N {
			return nil, fmt.Errorf("%s: the deployment's state lists slot %d twice", service, slots[i].N)
		}
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
// from the host this CLI runs on: its diagnostics port at the address it is
// published on (local; another host IP if that is all there is), or the
// host's own port (vps). A diagnostics port that is neither -- not
// published on a bridge network -- cannot be asked at all, and asking the
// same port on the host would reach whatever else holds it (auth's, say),
// so that is an error, not a guess.
func readinessURL(hostNetwork bool, r topology.Service) (string, error) {
	if !hostNetwork && !r.Reachable(r.DiagnosticsPort) {
		if r.Unfixed(r.DiagnosticsPort) {
			return "", fmt.Errorf("diagnostics port %d is published on a range of host ports or on one Docker picks, so there is no address to check its readiness at -- publish it on one fixed host port in `ports:` in the compose file", r.DiagnosticsPort)
		}
		return "", fmt.Errorf("diagnostics port %d is not published to the host, so its readiness cannot be checked -- add it to `ports:` in the compose file", r.DiagnosticsPort)
	}
	u := url.URL{Scheme: "http", Host: r.ProbeAddr(r.DiagnosticsPort), Path: "/readiness"}
	return u.String(), nil
}

// readinessURLs maps each replica's service to its readiness URL, failing
// on the first one that has none -- before anything is started.
func readinessURLs(hostNetwork bool, groups ...[]replica) (map[string]string, error) {
	out := map[string]string{}
	for _, group := range groups {
		for _, r := range group {
			u, err := readinessURL(hostNetwork, r.Ports)
			if err != nil {
				// A slot's ports are the base service's shifted: it is the
				// base service's entry that has to change.
				hint := ""
				if r.Slot > 1 {
					hint = fmt.Sprintf(" -- the fix is in %s's entry (DPORT %d)", r.Base, r.Ports.DiagnosticsPort-topology.SlotOffset(r.Slot))
				}
				return nil, fmt.Errorf("%s: %w%s%s", r.Service, err, slotNote(r.Service, r.Base, r.Slot), hint)
			}
			out[r.Service] = u
		}
	}
	return out, nil
}

// checkTopology is everything about where the replicas listen that can be
// known before anything starts: they stay off the ports the proxy, OpenBao
// and the rest of the compose file hold and off each other's, and each has
// a readiness URL. It returns the URLs by service. Up and Configure both
// call it, so what one accepts the other does.
func checkTopology(topo topology.Topology, hostNetwork bool, reserved []reservation, central, auth, edge []replica) (map[string]string, error) {
	if !hostNetwork {
		// A local deployment's proxy sits on Docker's network and reaches
		// the replicas by service name; a service on the host's network is
		// not there.
		for _, group := range [][]replica{central, auth, edge} {
			for _, r := range group {
				if r.Ports.HostNetwork() {
					return nil, fmt.Errorf("%s has `network_mode: host`, which only a vps deployment supports: on local the proxy reaches it by name over Docker's network", r.Service)
				}
			}
		}
	}
	var own []string // the replicas' own services, which are not "the rest"
	for _, group := range [][]replica{central, auth, edge} {
		for _, r := range group {
			own = append(own, r.Service, r.Base)
		}
	}
	// The services a start reaches: the replicas', what they depend on, and
	// Postgres, which `up` starts by name on local. A service nothing starts
	// holds no port, however it is declared.
	roots := append([]string(nil), own...)
	if !hostNetwork {
		roots = append(roots, "postgres")
	}
	for _, p := range topo.PublicationsReachedFrom(roots, own...) {
		reserved = append(reserved, reservation{Name: fmt.Sprintf("the compose file's %q service", p.Service), Binding: p.Binding})
	}
	if err := checkNoPortClash(hostNetwork, reserved, central, auth, edge); err != nil {
		return nil, err
	}
	return readinessURLs(hostNetwork, central, auth, edge)
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
