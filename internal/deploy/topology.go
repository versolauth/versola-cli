package deploy

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
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
	// Fronted: auth's APORT/MPORT moved behind the reverse proxy (frontAuth),
	// so even slot 1 listens elsewhere than the compose file says.
	Fronted bool
}

// loadTopology reads the ports out of the deployment's compose file
// (together with proxy.yml, when there is one) through Compose itself, so
// ${VERSION} and the like are already resolved. The output is parsed and
// dropped; it is not logged or stored, since Compose inlines env_file
// values into it.
func loadTopology(composePath string) (topology.Topology, error) {
	// Without overrides.yml: the ports are compose.yml's own, before the CLI
	// moves any (see frontAuth).
	out, err := docker.Output(state.TopologyComposeArgs(composePath, "config", "--format", "json")...)
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
// everywhere, APORT and MPORT for auth, and MPORT for a central whose compose
// file sets one (see centralMutualTLS). (topology.Service carries default
// APORT/MPORT for every service, but edge never binds them and central only
// the MPORT it is given; counting the defaults would report auth's as
// clashing with them.)
func (r replica) listeners() []listener {
	ls := []listener{{"PORT", r.Ports.Port}, {"DPORT", r.Ports.DiagnosticsPort}}
	if r.Base == AuthService {
		ls = append(ls, listener{"APORT", r.Ports.AdditionalPort}, listener{"MPORT", r.Ports.MutualTLSPort})
	}
	if r.centralMutualTLS() {
		ls = append(ls, listener{"MPORT", r.Ports.MutualTLSPort})
	}
	return ls
}

// centralMutualTLS: this is a central replica with a mutual-TLS listener --
// the edge surface, which a release serves on the MPORT its compose file
// sets for central (8093; versola#607). A release without one sets none and
// central binds nothing there, so nothing about it is moved or routed.
func (r replica) centralMutualTLS() bool {
	return r.Base == CentralService && r.Ports.MutualTLSSet()
}

// extraEnv is the environment a replica needs besides its listeners. A
// fronted central's mutual-TLS listener needs MBIND_HOST: the compose file binds it on every interface
// (MBIND_HOST 0.0.0.0, because the listener is public by design), and so
// would every replica on its moved port, next to the proxy that is the
// public address. The replicas listen on the loopback, where the proxy
// reaches them.
func (r replica) extraEnv() [][2]string {
	if r.Fronted && r.centralMutualTLS() {
		return [][2]string{{"MBIND_HOST", "127.0.0.1"}}
	}
	return nil
}

// frontOffset is how far auth's APORT and MPORT move on a host network
// (vps) for the reverse proxy to listen where the compose file puts them.
//
// edge and central reach those two listeners at fixed addresses
// (127.0.0.1:8082 for auth-additional-url, 127.0.0.1:8083 for
// auth-mutual-tls-url, written by versola-tools): the first replica's own
// ports, which made every other replica unreachable there. The proxy now
// holds those addresses and balances over every replica, and each replica,
// the first included, listens 1000 higher (9082/9083, then +100 a slot).
// Like slotStride this is the CLI's own placement, not Versola's topology.
const frontOffset = 1000

// frontAuth moves auth's APORT and MPORT behind the reverse proxy (see
// frontOffset) on a host network; on a bridge network (local) every
// container has its own ports, so nothing moves. Out of range is an error.
func frontAuth(auth []replica, hostNetwork bool) ([]replica, error) {
	if !hostNetwork {
		return auth, nil
	}
	out := make([]replica, len(auth))
	for i, r := range auth {
		r.Ports = r.Ports.ShiftAuthListeners(frontOffset, r.Slot)
		if err := r.Ports.CheckAdditional(r.Service); err != nil {
			return nil, err
		}
		r.Fronted = true
		out[i] = r
	}
	return out, nil
}

// centralFrontOffset is how far central's PORT and DPORT move on a host
// network (vps) for the reverse proxy to listen where the compose file
// puts PORT (8090), which auth, edge and the admin console's resource all
// call. The same idea as frontOffset, with a bigger step: 28090/28091 is
// outside the ports other software on a server commonly takes (Prometheus
// and its exporters sit at 9090-9100) and below Linux's ephemeral range
// (from 32768), which outgoing connections pick from. Slot n adds the
// usual slot stride on top.
const centralFrontOffset = 20000

// frontCentral moves central's PORT and DPORT behind the reverse proxy (see
// centralFrontOffset) on a host network; on a bridge network (local) every
// container has its own ports, so nothing moves. Out of range is an error.
func frontCentral(central []replica, hostNetwork bool) ([]replica, error) {
	if !hostNetwork {
		return central, nil
	}
	out := make([]replica, len(central))
	for i, r := range central {
		ports, err := r.Ports.ShiftListeners(centralFrontOffset)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Service, err)
		}
		if r.centralMutualTLS() {
			// The mutual-TLS listener moves behind the proxy with the rest,
			// to the proxy's address for it (the compose file's own port).
			ports.MutualTLSPort += centralFrontOffset
			if ports.MutualTLSPort > 65535 {
				return nil, fmt.Errorf("%s: moving the ports by %d would need port %d for MPORT, which is out of range", r.Service, centralFrontOffset, ports.MutualTLSPort)
			}
		}
		r.Ports = ports
		r.Fronted = true
		out[i] = r
	}
	return out, nil
}

// proxyFronts are the addresses the reverse proxy takes over on a host
// network: in front of auth, its APORT and MPORT as the compose file sets
// them, on loopback (where auth binds and edge and central connect); and in
// front of central, its PORT as the compose file sets it (where auth, edge
// and the admin console's resource connect) and, when the compose file sets
// one, its MPORT (where edges reach central's mutual-TLS listener, on every
// interface as central itself did).
func proxyFronts(topo topology.Topology) (proxy.Fronts, error) {
	auth, err := topo.Service(AuthService)
	if err != nil {
		return proxy.Fronts{}, err
	}
	if err := auth.CheckAdditional(AuthService); err != nil {
		return proxy.Fronts{}, err
	}
	central, err := topo.Service(CentralService)
	if err != nil {
		return proxy.Fronts{}, err
	}
	f := proxy.Fronts{Additional: auth.AdditionalPort, MutualTLS: auth.MutualTLSPort, Central: central.Port}
	if central.MutualTLSSet() {
		f.CentralMutualTLS = central.MutualTLSPort
	}
	return f, nil
}

// proxyGenerationsDir is the vps proxy's own configuration directory (see
// proxy.Config.GenerationsDir): next to the bundles, never one of them, so
// it survives every configure and is not pruned with them.
func proxyGenerationsDir() (string, error) {
	dir, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "proxy-vps"), nil
}

// proxyRouting is what the proxy config of a deployment needs beyond its
// replicas: on a host network (vps) the fronts of auth's APORT/MPORT and
// the generations directory; nothing locally.
func proxyRouting(topo topology.Topology, hostNetwork bool) (proxy.Fronts, string, error) {
	if !hostNetwork {
		return proxy.Fronts{}, "", nil
	}
	fronts, err := proxyFronts(topo)
	if err != nil {
		return proxy.Fronts{}, "", err
	}
	dir, err := proxyGenerationsDir()
	if err != nil {
		return proxy.Fronts{}, "", err
	}
	return fronts, dir, nil
}

// routedConfig is the proxy config that routes to these replicas.
//
// central is the replicas behind the proxy's front of central (vps); on
// local nothing is in front of it and it is not part of the config.
func routedConfig(mode string, fronts proxy.Fronts, genDir string, central, auth, edge []replica) proxy.Config {
	c := proxy.Config{Mode: mode, Auth: proxyBackends(auth), Edge: proxyBackends(edge), Fronts: fronts, GenerationsDir: genDir}
	if fronts.Central != 0 {
		c.Central = centralBackends(central)
	}
	return c
}

// frontReservations are proxyFronts as ports nothing else may take.
func frontReservations(f proxy.Fronts) []reservation {
	out := []reservation{
		{Name: proxyHolder + " (in front of auth's APORT)", Binding: topology.Binding{HostIP: "127.0.0.1", Port: f.Additional}},
		{Name: proxyHolder + " (in front of auth's MPORT)", Binding: topology.Binding{HostIP: "127.0.0.1", Port: f.MutualTLS}},
		{Name: proxyHolder + " (in front of central's PORT)", Binding: topology.Binding{HostIP: "127.0.0.1", Port: f.Central}},
	}
	if f.CentralMutualTLS != 0 {
		// On every interface: it is the address edges on other hosts use.
		out = append(out, reservation{Name: proxyHolder + " (in front of central's MPORT)", Binding: topology.Binding{Port: f.CentralMutualTLS}})
	}
	return out
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
		// What is held outside the replicas must not collide either: the
		// proxy's port and a service of the compose file that publishes it.
		for _, held := range claims {
			if held.reserved && !held.binding.IsRange() && !r.Binding.IsRange() && held.binding.Overlaps(r.Binding) {
				return fmt.Errorf("%s and %s would both use %s on the host", held.service, r.Name, r.Binding)
			}
		}
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
						hints = append(hints, "PORT/DPORT/APORT/MPORT")
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
	// Ranges on different addresses compete in ways a simple greedy choice
	// gets wrong, so the assignment is searched. Each range's candidates are
	// its free ports; every other range can take away at most one of them,
	// so len(ranges) of them are enough to keep.
	candidates := make([][]int, len(ranges))
	for i, r := range ranges {
		for p := r.binding.Port; p <= r.binding.Last && len(candidates[i]) <= len(ranges); p++ {
			b := topology.Binding{HostIP: r.binding.HostIP, Port: p}
			free := true
			for _, f := range fixed {
				if f.binding.Overlaps(b) {
					free = false
					break
				}
			}
			if free {
				candidates[i] = append(candidates[i], p)
			}
		}
	}
	// Most constrained first.
	order := make([]int, len(ranges))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(candidates[order[a]]) < len(candidates[order[b]]) })
	assigned := make([]topology.Binding, 0, len(ranges))
	var search func(k int) bool
	search = func(k int) bool {
		if k == len(order) {
			return true
		}
		i := order[k]
		for _, p := range candidates[i] {
			b := topology.Binding{HostIP: ranges[i].binding.HostIP, Port: p}
			ok := true
			for _, a := range assigned {
				if a.Overlaps(b) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			assigned = append(assigned, b)
			if search(k + 1) {
				return true
			}
			assigned = assigned[:len(assigned)-1]
		}
		return false
	}
	if !search(0) {
		// Name the most constrained range: the first the search could not place.
		r := ranges[order[0]]
		for _, i := range order {
			if len(candidates[i]) == 0 {
				r = ranges[i]
				break
			}
		}
		who := r.service
		if r.label != "" {
			who += " (" + r.label + ")"
		}
		hint := ""
		if !r.reserved {
			hint = " -- check `ports:` in the compose file"
		}
		return fmt.Errorf("%s publishes %s, but the publications that share its addresses leave it no free port in that range%s%s", who, r.binding, slotNote(r.service, r.base, r.slot), hint)
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
		if service == AuthService {
			if err := ports.CheckAdditional(topology.SlotService(service, s.N)); err != nil {
				return nil, err
			}
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
	// OpenBao's port is held by name above; its own service is not another claim.
	own = append(own, "openbao")
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
	if hostNetwork {
		fronts, err := proxyFronts(topo)
		if err != nil {
			return nil, err
		}
		reserved = append(reserved, frontReservations(fronts)...)
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

// centralBackends is what the proxy's central upstreams route to: each
// replica's PORT and, when it has a mutual-TLS listener behind the proxy,
// its MPORT (central has no APORT).
func centralBackends(replicas []replica) []proxy.Backend {
	backends := make([]proxy.Backend, len(replicas))
	for i, r := range replicas {
		backends[i] = proxy.Backend{Service: r.Service, Port: r.Ports.Port}
		if r.Fronted && r.centralMutualTLS() {
			backends[i].MutualTLSPort = r.Ports.MutualTLSPort
		}
	}
	return backends
}

// proxyBackends is what the proxy's upstreams route to: each replica's
// main port, and for a fronted auth replica its APORT and MPORT too.
func proxyBackends(replicas []replica) []proxy.Backend {
	backends := make([]proxy.Backend, len(replicas))
	for i, r := range replicas {
		backends[i] = proxy.Backend{Service: r.Service, Port: r.Ports.Port}
		if r.Fronted {
			backends[i].AdditionalPort = r.Ports.AdditionalPort
			backends[i].MutualTLSPort = r.Ports.MutualTLSPort
		}
	}
	return backends
}
