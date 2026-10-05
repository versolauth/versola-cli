// Package topology answers two questions about a deployment from the
// compose file versola-tools generated, rather than from constants in this
// CLI: which ports does each service listen on, and which replica slots of
// a service exist (names and ports).
//
// This CLI is meant not to know Versola's topology (design doc §3.5), so
// that one build of it can deploy any release. The ports used to be
// hardcoded in internal/deploy/up.go (readiness URLs) and internal/proxy
// (upstream addresses); both now take them from here. What stays in this
// package is the CLI's own slot arithmetic -- how a replica's ports are
// derived from the base service's -- which is a property of how this CLI
// places replicas, not of Versola.
//
// The compose file is read through `docker compose config --format json`
// (see deploy.loadTopology), not parsed as YAML by hand: Compose has
// already substituted ${VERSION}, merged the files and normalised the
// `environment:` forms (map or list) into one shape, and the CLI needs no
// YAML dependency for it.
package topology

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// The ports VersolaApp.scala uses when a service's environment sets none
// (util/.../VersolaApp.scala: port, diagnosticsPort, additionalPort).
// That is the application's own contract, so falling back to it is not
// the CLI guessing at a topology.
const (
	DefaultPort            = 8080
	DefaultDiagnosticsPort = 8081
	DefaultAdditionalPort  = 8082
)

const (
	// MaxSlot is the highest slot number. Slots are positions, not a count:
	// a rolling upgrade (0d) brings a new version up in a free slot next to
	// the old one, so there have to be more slots than replicas running at
	// once.
	MaxSlot = 9

	// slotStride is how far apart consecutive slots' ports are. 100 keeps
	// every slot of every service clear of the others: Versola's base
	// ports (8080-8096) span less than 100, so slot n's range never
	// reaches slot n+1's.
	slotStride = 100
)

// Service is one compose service's ports.
type Service struct {
	// Port, DiagnosticsPort, AdditionalPort are the container's PORT,
	// DPORT and APORT: main traffic, /liveness + /readiness + /metrics,
	// and auth's account-settings surface.
	Port            int
	DiagnosticsPort int
	AdditionalPort  int

	// published is every tcp `ports:` entry with one plain host port, in
	// compose's order. Empty for a `network_mode: host` service (vps),
	// where a port is the host's port and Docker ignores `ports:`.
	// (`compose config` accepts `ports:` with a network_mode that shares
	// another service's network, and they are read like any other.)
	published []Publication

	// unfixed are the container ports published with no host port to name:
	// Compose picks one (`ports: ["8081"]`). Their address is not known.
	unfixed []int

	// additionalBad: APORT is set to something that is not a port. Only
	// auth binds APORT, so it is an error for auth alone (CheckAdditional);
	// for the others the variable means nothing.
	additionalBad bool

	hostNetwork bool
}

// CheckAdditional is an error when APORT is set to something that is not a
// port. Callers ask it for the services that actually bind APORT; for any
// other the value is never used and cannot fail a deployment.
func (s Service) CheckAdditional(service string) error {
	if s.additionalBad {
		return fmt.Errorf("service %q sets APORT to something that is not a port number (1-65535)", service)
	}
	return nil
}

// HostNetwork reports whether the compose file gives this service the
// host's network (`network_mode: host`).
func (s Service) HostNetwork() bool { return s.hostNetwork }

// Binding is a host address and port a container port is published on.
type Binding struct {
	// HostIP is the address Compose binds, as written in `host_ip`; empty
	// means every interface.
	HostIP string
	Port   int
	// Last is the end of a range of host ports (`8000-8010`), or 0 for one
	// port. Docker takes one port of the range; all of it is treated as
	// occupied, since which one is not known.
	Last int
}

// last is the highest port of the binding.
func (b Binding) last() int {
	if b.Last > b.Port {
		return b.Last
	}
	return b.Port
}

// IsRange reports whether the binding is more than one port. Docker takes
// whichever port of a range is free, so a range does not collide with a
// fixed port inside it; it is how a service's address is unknown.
func (b Binding) IsRange() bool { return b.last() > b.Port }

// Publication is one `ports:` entry: a container port and where it is
// published. A container port can have several (a loopback one and a LAN
// one, say).
type Publication struct {
	Target int // the container's port
	Binding
}

// String is the container port and where it is published, for messages
// (Publication embeds Binding and would otherwise print only the binding).
func (p Publication) String() string {
	return fmt.Sprintf("%d published on %s", p.Target, p.Binding)
}

// String is the binding as it would be written in `ports:`.
func (b Binding) String() string {
	port := strconv.Itoa(b.Port)
	if b.IsRange() {
		port += "-" + strconv.Itoa(b.last())
	}
	if b.HostIP == "" {
		return port
	}
	return net.JoinHostPort(strings.Trim(b.HostIP, "[]"), port)
}

// addr parses HostIP. ok is false for empty (every interface, both
// families) and for anything that is not an IP address.
func (b Binding) addr() (a netip.Addr, ok bool) {
	a, err := netip.ParseAddr(strings.Trim(b.HostIP, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

var (
	loopback4 = netip.MustParseAddr("127.0.0.1")
	loopback6 = netip.MustParseAddr("::1")
)

// local reports whether the binding can be reached as localhost from the
// host: it covers loopback (or every interface).
func (b Binding) local() bool {
	if b.HostIP == "" || strings.EqualFold(b.HostIP, "localhost") {
		return true
	}
	a, ok := b.addr()
	a = a.WithZone("")
	return ok && (a.IsUnspecified() || a == loopback4 || a == loopback6)
}

// probeHost is the host to dial to reach a local binding. A binding to one
// loopback address is dialled by that address: "localhost" can be either
// 127.0.0.1 or ::1, and two services published on 127.0.0.1:P and [::1]:P
// are different services. A binding to every interface (or to the name
// localhost) is reachable through either, so "localhost" does.
func (b Binding) probeHost() string {
	a, ok := b.addr()
	if !ok || a.IsUnspecified() {
		return "localhost"
	}
	return a.WithZone("").String()
}

// Overlaps reports whether two bindings would compete for the same socket:
// the same port, and addresses that are equal or one of which covers the
// other (a wildcard of the same IP family, or an empty one, which covers
// both). 127.0.0.1:8080 and 127.0.0.2:8080 do not overlap. Compose only
// writes IP addresses; a host_ip that is not one is compared by its text
// with another such, and is assumed to overlap with an address.
func (b Binding) Overlaps(o Binding) bool {
	if b.Port > o.last() || o.Port > b.last() {
		return false
	}
	if b.HostIP == "" || o.HostIP == "" {
		return true
	}
	ba, bok := b.addr()
	oa, ook := o.addr()
	if !bok && !ook {
		return strings.EqualFold(b.HostIP, o.HostIP)
	}
	if !bok || !ook {
		return true // a name against an address: can't tell, so assume the worst
	}
	if ba.Is4() != oa.Is4() {
		return false
	}
	if ba.Zone() != "" && oa.Zone() != "" && ba.Zone() != oa.Zone() {
		return false // fe80::1 on two different interfaces
	}
	ba, oa = ba.WithZone(""), oa.WithZone("")
	return ba == oa || ba.IsUnspecified() || oa.IsUnspecified()
}

// ProbeAddr is the host:port at which the host this CLI runs on reaches
// this container port: the first published port that is reachable from
// this host (loopback or every interface; local, Docker Desktop) -- at
// the loopback address it is bound to, or at localhost for every
// interface --, else the address of a binding to some other host IP, else
// localhost and the port itself (a host-network service on vps, or a
// container port that is not published). A container port published only
// on a LAN IP is therefore probed there, not at a port that is not on the
// host at all.
func (s Service) ProbeAddr(containerPort int) string {
	var other Binding // the first binding that is not localhost's
	found := false
	for _, p := range s.published {
		if p.Target != containerPort || p.IsRange() {
			continue
		}
		if p.local() {
			return net.JoinHostPort(p.probeHost(), strconv.Itoa(p.Port))
		}
		if !found {
			other, found = p.Binding, true
		}
	}
	if found {
		return net.JoinHostPort(strings.Trim(other.HostIP, "[]"), strconv.Itoa(other.Port))
	}
	return net.JoinHostPort("localhost", strconv.Itoa(containerPort))
}

// Reachable reports whether the host this CLI runs on can reach this
// container port at all: a host-network service has the host's own ports,
// any other has only what it publishes. A port that is not published is
// inside the container's network, where ProbeAddr's fallback would be
// some other service's port -- or none.
func (s Service) Reachable(containerPort int) bool {
	if s.hostNetwork {
		return true
	}
	for _, p := range s.published {
		if p.Target == containerPort && !p.IsRange() {
			return true
		}
	}
	return false
}

// Unfixed reports whether the container port is published, but not on one
// host port that is known: on a range, or on a port Compose picks. It is
// published, yet there is no address to ask.
func (s Service) Unfixed(containerPort int) bool {
	for _, p := range s.published {
		if p.Target == containerPort && p.IsRange() {
			return true
		}
	}
	for _, t := range s.unfixed {
		if t == containerPort {
			return true
		}
	}
	return false
}

// Publications returns every published port of this service (a copy).
// Only these occupy ports on the host when containers have their own
// network (local); ProbeAddr, in contrast, says where to reach a port.
func (s Service) Publications() []Publication {
	return append([]Publication(nil), s.published...)
}

// ForSlot returns the ports of this service's replica in the given slot.
// Slot 1 is the service as compose declares it; slot n is shifted by
// (n-1)*100 -- ports and published ports alike. Shifting the published
// (host) port the same way is an assumption about how the replica's
// compose service will be generated (a later step); nothing here checks
// that the host port is actually free.
func (s Service) ForSlot(slot int) (Service, error) {
	if err := ValidSlot(slot); err != nil {
		return Service{}, err
	}
	off := SlotOffset(slot)
	out := Service{
		Port:            s.Port + off,
		DiagnosticsPort: s.DiagnosticsPort + off,
		AdditionalPort:  s.AdditionalPort + off,
		additionalBad:   s.additionalBad,
		hostNetwork:     s.hostNetwork,
	}
	for _, p := range s.published {
		target, host := p.Target+off, p.Port+off
		var last int
		if p.IsRange() {
			last = p.last() + off
		}
		if target > 65535 {
			return Service{}, fmt.Errorf("slot %d would need container port %d, which is out of range", slot, target)
		}
		if host > 65535 || p.last()+off > 65535 {
			return Service{}, fmt.Errorf("slot %d would need host port %d (for container port %d), which is out of range", slot, p.last()+off, target)
		}
		out.published = append(out.published, Publication{Target: target, Binding: Binding{HostIP: p.HostIP, Port: host, Last: last}})
	}
	for _, t := range s.unfixed {
		if t+off > 65535 {
			return Service{}, fmt.Errorf("slot %d would need container port %d, which is out of range", slot, t+off)
		}
		out.unfixed = append(out.unfixed, t+off)
	}
	for _, p := range []int{out.Port, out.DiagnosticsPort, out.AdditionalPort} {
		if p > 65535 {
			return Service{}, fmt.Errorf("slot %d would need port %d, which is out of range", slot, p)
		}
	}
	return out, nil
}

// ValidSlot reports whether n is a slot number (1..MaxSlot).
func ValidSlot(n int) error {
	if n < 1 || n > MaxSlot {
		return fmt.Errorf("slot %d is out of range (1-%d)", n, MaxSlot)
	}
	return nil
}

// SlotOffset is how far slot n's ports are from slot 1's.
func SlotOffset(n int) int { return (n - 1) * slotStride }

// SlotService is the compose service name of base's replica in slot n:
// base itself for slot 1 (so a deployment made before replicas existed
// keeps its names), "base-n" for the others.
func SlotService(base string, n int) string {
	if n == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n)
}

// ServicePublication is a published port of the compose service that
// publishes it.
type ServicePublication struct {
	Service string
	Publication
}

// PublicationsExcept lists the published ports of every service that is
// not named, in service-name order: the host ports the rest of the
// deployment (Postgres, the old gateway, ...) holds. A service that is
// malformed, scaled to 0 or on the host's network has none to list; what a
// service's environment says is not looked at.
func (t Topology) PublicationsExcept(names ...string) []ServicePublication {
	skip := map[string]bool{}
	for _, n := range names {
		skip[n] = true
	}
	var services []string
	for name := range t.services {
		if !skip[name] {
			services = append(services, name)
		}
	}
	sort.Strings(services)
	var out []ServicePublication
	for _, name := range services {
		raw := t.services[name]
		if raw.malformed || raw.disabled {
			continue
		}
		for _, p := range raw.service().Publications() {
			out = append(out, ServicePublication{Service: name, Publication: p})
		}
	}
	return out
}

// Topology is the services of one compose configuration.
type Topology struct {
	services map[string]rawService
}

// rawService is all that is kept of one compose service: its port
// variables and its published ports. Nothing else from the configuration
// is retained -- `compose config` inlines env_file values into
// `environment`, secrets included, and they must not outlive Parse.
type rawService struct {
	env envVars
	// malformed: the service is not an object at all; asking for it is an
	// error, since reading nothing from it would mean the default ports.
	malformed bool
	// hostNetwork: `network_mode: host`. The container then has the host's
	// ports and Docker ignores any `ports:` on it, so they are not read.
	hostNetwork bool
	// disabled: scale or deploy.replicas is 0, so nothing publishes its ports.
	disabled bool
	ports    []rawPort
}

// portVar is one of PORT, DPORT and APORT as the environment has it: a
// port, or bad when it is not one. The text of a bad value is not kept --
// `compose config` inlines env_file values, so it may be a password that
// landed in the wrong variable, and nothing that holds a Topology (a
// %v in a log line, say) should be able to print it.
type portVar struct {
	n   int
	bad bool
}

// envVars holds just PORT, DPORT and APORT of a service's environment.
// Anything else in the environment is dropped while decoding, and an
// environment that isn't an object at all reads as empty, so an odd
// service cannot fail the whole configuration.
type envVars map[string]portVar

func (e *envVars) UnmarshalJSON(b []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		*e = nil
		return nil
	}
	out := envVars{}
	for _, key := range []string{"PORT", "DPORT", "APORT"} {
		raw, ok := all[key]
		if !ok || string(raw) == "null" {
			continue
		}
		// Not a port (a string that isn't digits, 8080.5, true, an object)
		// is marked bad, so that envPort rejects it rather than falling
		// back to the default port unnoticed.
		n, ok := plainPort(raw)
		out[key] = portVar{n: n, bad: !ok}
	}
	*e = out
	return nil
}

type rawPort struct {
	target    json.RawMessage
	hostIP    string
	published json.RawMessage
	protocol  string
}

// serviceJSON and portJSON are the parts of a compose service and of a
// `ports:` entry that are read. Everything is decoded as raw JSON first and
// then leniently, so that a value of an unexpected shape costs the entry
// (or the service's ports), never the whole configuration.
type serviceJSON struct {
	Environment envVars         `json:"environment"`
	NetworkMode json.RawMessage `json:"network_mode"`
	Ports       json.RawMessage `json:"ports"`
	Scale       json.RawMessage `json:"scale"`
	Deploy      struct {
		Replicas json.RawMessage `json:"replicas"`
	} `json:"deploy"`
}

// zero reports whether a compose number (scale, replicas) is 0.
func zero(raw json.RawMessage) bool {
	var f float64
	return json.Unmarshal(raw, &f) == nil && f == 0
}

type portJSON struct {
	Target    json.RawMessage `json:"target"`
	HostIP    json.RawMessage `json:"host_ip"`
	Published json.RawMessage `json:"published"`
	Protocol  json.RawMessage `json:"protocol"`
}

// jsonString is raw as a string, or "" when it is not one.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// Parse reads the output of `docker compose config --format json`.
// A service's ports are checked when it is asked for (Service), not here:
// an entry of an unexpected shape costs only that entry, and a service
// that is not an object only fails when it is asked for, so one odd service
// that nothing here cares about -- Postgres, OpenBao -- cannot fail a
// deployment.
func Parse(data []byte) (Topology, error) {
	var cfg struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Topology{}, fmt.Errorf("couldn't read the compose configuration: %w", err)
	}
	if len(cfg.Services) == 0 {
		return Topology{}, fmt.Errorf("the compose configuration declares no services")
	}
	t := Topology{services: make(map[string]rawService, len(cfg.Services))}
	for name, body := range cfg.Services {
		var s serviceJSON
		if string(body) == "null" || json.Unmarshal(body, &s) != nil {
			t.services[name] = rawService{malformed: true}
			continue
		}
		raw := rawService{
			env:         s.Environment,
			hostNetwork: jsonString(s.NetworkMode) == "host",
			// `up` starts no container of a service scaled to 0.
			disabled: zero(s.Scale) || zero(s.Deploy.Replicas),
		}
		var entries []json.RawMessage
		if json.Unmarshal(s.Ports, &entries) != nil {
			entries = nil
		}
		for _, e := range entries {
			var p portJSON
			if json.Unmarshal(e, &p) != nil {
				continue
			}
			raw.ports = append(raw.ports, rawPort{target: p.Target, hostIP: jsonString(p.HostIP), published: p.Published, protocol: jsonString(p.Protocol)})
		}
		t.services[name] = raw
	}
	return t, nil
}

// Service returns the ports of the named compose service.
func (t Topology) Service(name string) (Service, error) {
	raw, ok := t.services[name]
	if !ok {
		return Service{}, fmt.Errorf("the compose file has no %q service", name)
	}
	if raw.malformed {
		return Service{}, fmt.Errorf("service %q has an unexpected shape in the compose configuration", name)
	}
	svc := raw.service()
	var err error
	if svc.Port, err = envPort(name, raw.env, "PORT", DefaultPort); err != nil {
		return Service{}, err
	}
	if svc.DiagnosticsPort, err = envPort(name, raw.env, "DPORT", DefaultDiagnosticsPort); err != nil {
		return Service{}, err
	}
	// APORT is bound by auth alone (see CheckAdditional): a bad value is
	// remembered, not an error, so that edge or central with an empty
	// APORT -- `APORT: ${APORT:-}` -- still loads.
	if svc.AdditionalPort, err = envPort(name, raw.env, "APORT", DefaultAdditionalPort); err != nil {
		svc.AdditionalPort, svc.additionalBad = DefaultAdditionalPort, true
	}
	return svc, nil
}

// service is the published ports of raw, with the ports of the process
// still at their defaults. It reads nothing from the environment, so one
// service's bad PORT cannot hide what it publishes.
func (raw rawService) service() Service {
	svc := Service{hostNetwork: raw.hostNetwork}
	for _, p := range raw.ports {
		if raw.hostNetwork || (p.protocol != "" && !strings.EqualFold(p.protocol, "tcp")) {
			continue
		}
		target, ok := plainPort(p.target)
		if !ok {
			continue
		}
		switch first, last, kind := hostPorts(p.published); kind {
		case oneHostPort, hostPortRange:
			svc.published = append(svc.published, Publication{Target: target, Binding: Binding{HostIP: p.hostIP, Port: first, Last: last}})
		case noHostPort:
			svc.unfixed = append(svc.unfixed, target)
		}
	}
	return svc
}

// envPort reads a port from a service's environment: def when unset, an
// error when set to something that isn't a port.
func envPort(service string, env envVars, key string, def int) (int, error) {
	v, ok := env[key]
	if !ok {
		return def, nil
	}
	n := v.n
	if v.bad {
		// The value is not quoted, not even in part: `compose config`
		// inlines env_file values into the environment, so a PORT that is
		// not a port may well be a password that landed in the wrong
		// variable, and this error goes to the terminal.
		return 0, fmt.Errorf("service %q sets %s to something that is not a port number (1-65535)", service, key)
	}
	return n, nil
}

// portFromString reads a port from digits only: no sign, no spaces, no
// range, nothing outside 1-65535.
func portFromString(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

type hostPortKind int

const (
	badHostPort   hostPortKind = iota // not understood: the entry is ignored
	oneHostPort                       // "18081"
	hostPortRange                     // "18081-18090"
	noHostPort                        // absent or "": Compose picks the port
)

// hostPorts reads a `published` value: one port, a range of them
// (first-last), or nothing, which is Compose picking a free port.
func hostPorts(raw json.RawMessage) (first, last int, kind hostPortKind) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, 0, noHostPort
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		if n, ok := plainPort(raw); ok {
			return n, 0, oneHostPort
		}
		return 0, 0, badHostPort
	}
	if s == "" {
		return 0, 0, noHostPort
	}
	if n, ok := portFromString(s); ok {
		return n, 0, oneHostPort
	}
	lo, hi, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, badHostPort
	}
	a, aok := portFromString(lo)
	b, bok := portFromString(hi)
	switch {
	case !aok || !bok || a > b:
		return 0, 0, badHostPort
	case a == b:
		return a, 0, oneHostPort
	}
	return a, b, hostPortRange
}

// plainPort reads one port number from a compose value, which Compose
// writes as a string of digits ("8081") and some versions as a number. A
// range, an empty value (Compose picks the port), a sign, a fraction or
// anything outside 1-65535 is not one plain port.
func plainPort(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return portFromString(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f < 1 || f > 65535 || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}
