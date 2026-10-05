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
	// Compose refuses `ports:` together with any other network_mode that
	// shares or removes the network, so those services have none.
	published []Publication

	hostNetwork bool
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
}

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
	if b.HostIP == "" {
		return strconv.Itoa(b.Port)
	}
	return net.JoinHostPort(strings.Trim(b.HostIP, "[]"), strconv.Itoa(b.Port))
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

// Overlaps reports whether two bindings would compete for the same socket:
// the same port, and addresses that are equal or one of which covers the
// other (a wildcard of the same IP family, or an empty one, which covers
// both). 127.0.0.1:8080 and 127.0.0.2:8080 do not overlap. Compose only
// writes IP addresses; a host_ip that is not one is compared by its text
// with another such, and is assumed to overlap with an address.
func (b Binding) Overlaps(o Binding) bool {
	if b.Port != o.Port {
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
// this container port: localhost and the first published port that
// localhost can reach (loopback or every interface; local, Docker
// Desktop), else the address of a binding to some other host IP, else
// localhost and the port itself (a host-network service on vps, or a
// container port that is not published). A container port published only
// on a LAN IP is therefore probed there, not at a port that is not on the
// host at all.
func (s Service) ProbeAddr(containerPort int) string {
	var other Binding // the first binding that is not localhost's
	found := false
	for _, p := range s.published {
		if p.Target != containerPort {
			continue
		}
		if p.local() {
			return net.JoinHostPort("localhost", strconv.Itoa(p.Port))
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
		published:       make([]Publication, 0, len(s.published)),
		hostNetwork:     s.hostNetwork,
	}
	for _, p := range s.published {
		target, host := p.Target+off, p.Port+off
		if target > 65535 {
			return Service{}, fmt.Errorf("slot %d would need container port %d, which is out of range", slot, target)
		}
		if host > 65535 {
			return Service{}, fmt.Errorf("slot %d would need host port %d (for container port %d), which is out of range", slot, host, target)
		}
		out.published = append(out.published, Publication{Target: target, Binding: Binding{HostIP: p.HostIP, Port: host}})
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
	ports       []rawPort
}

// envVars holds just PORT, DPORT and APORT of a service's environment, as
// strings. Anything else in the environment is dropped while decoding, and
// an environment that isn't an object at all reads as empty, so an odd
// service cannot fail the whole configuration.
type envVars map[string]string

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
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out[key] = s
			continue
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil && f == math.Trunc(f) && math.Abs(f) < 1e9 {
			out[key] = strconv.Itoa(int(f))
			continue
		}
		// Not a string or a whole number (8080.5, true, an object): kept as
		// its JSON text so that envPort rejects it, rather than falling
		// back to the default port unnoticed.
		out[key] = string(raw)
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
		if json.Unmarshal(body, &s) != nil {
			t.services[name] = rawService{malformed: true}
			continue
		}
		raw := rawService{env: s.Environment, hostNetwork: jsonString(s.NetworkMode) == "host"}
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
	svc := Service{hostNetwork: raw.hostNetwork}
	var err error
	if svc.Port, err = envPort(name, raw.env, "PORT", DefaultPort); err != nil {
		return Service{}, err
	}
	if svc.DiagnosticsPort, err = envPort(name, raw.env, "DPORT", DefaultDiagnosticsPort); err != nil {
		return Service{}, err
	}
	if svc.AdditionalPort, err = envPort(name, raw.env, "APORT", DefaultAdditionalPort); err != nil {
		return Service{}, err
	}
	for _, p := range raw.ports {
		if raw.hostNetwork || (p.protocol != "" && !strings.EqualFold(p.protocol, "tcp")) {
			continue
		}
		target, ok := plainPort(p.target)
		if !ok {
			continue
		}
		// A range, an empty value (Compose picks the port) or anything
		// else that isn't one plain port can't be reached at a known
		// address, so it is treated as not published.
		if host, ok := plainPort(p.published); ok {
			svc.published = append(svc.published, Publication{Target: target, Binding: Binding{HostIP: p.hostIP, Port: host}})
		}
	}
	return svc, nil
}

// envPort reads a port from a service's environment: def when unset, an
// error when set to something that isn't a port.
func envPort(service string, env envVars, key string, def int) (int, error) {
	v, ok := env[key]
	if !ok {
		return def, nil
	}
	n, ok := portFromString(v)
	if !ok {
		// The value is quoted shortened: compose config inlines env_file
		// values, so this one could in principle come from a secrets file.
		if r := []rune(v); len(r) > 16 {
			v = string(r[:16]) + "..."
		}
		return 0, fmt.Errorf("service %q sets %s=%q, which is not a port", service, key, v)
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
