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
	"strconv"
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
	// every slot of every service clear of the others (Versola's base
	// ports are all below 8100, 100 apart at most within a service).
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

	// published maps a container port to the host port Compose publishes
	// it on (`ports:`). Empty on vps, where containers use the host's
	// network and a port is the host's port.
	published map[int]int
}

// HostPort returns the port on the host that reaches this container port:
// the published one when `ports:` maps it (local, Docker Desktop), else
// the port itself (vps, network_mode: host).
func (s Service) HostPort(containerPort int) int {
	if p, ok := s.published[containerPort]; ok {
		return p
	}
	return containerPort
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
		published:       make(map[int]int, len(s.published)),
	}
	for container, host := range s.published {
		out.published[container+off] = host + off
	}
	for _, p := range []int{out.Port, out.DiagnosticsPort, out.AdditionalPort} {
		if p > 65535 {
			return Service{}, fmt.Errorf("slot %d would need port %d, which is out of range", slot, p)
		}
	}
	for _, p := range out.published {
		if p > 65535 {
			return Service{}, fmt.Errorf("slot %d would need published port %d, which is out of range", slot, p)
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
	env   envVars
	ports []rawPort
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
		var n int
		if json.Unmarshal(raw, &n) == nil {
			out[key] = strconv.Itoa(n)
		}
	}
	*e = out
	return nil
}

type rawPort struct {
	target    int
	published json.RawMessage
	protocol  string
}

// Parse reads the output of `docker compose config --format json`.
// Nothing is validated per service here: a service's ports are checked
// when it is asked for (Service), so one odd service that nothing here
// cares about -- Postgres, OpenBao -- cannot fail a deployment.
func Parse(data []byte) (Topology, error) {
	var cfg struct {
		Services map[string]struct {
			Environment envVars `json:"environment"`
			Ports       []struct {
				Target    int             `json:"target"`
				Published json.RawMessage `json:"published"`
				Protocol  string          `json:"protocol"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Topology{}, fmt.Errorf("couldn't read the compose configuration: %w", err)
	}
	if len(cfg.Services) == 0 {
		return Topology{}, fmt.Errorf("the compose configuration declares no services")
	}
	t := Topology{services: make(map[string]rawService, len(cfg.Services))}
	for name, s := range cfg.Services {
		raw := rawService{env: s.Environment}
		for _, p := range s.Ports {
			raw.ports = append(raw.ports, rawPort{target: p.Target, published: p.Published, protocol: p.Protocol})
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
	svc := Service{published: map[int]int{}}
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
		if p.protocol != "" && p.protocol != "tcp" {
			continue
		}
		// A range, an empty value (Compose picks the port) or anything
		// else that isn't one plain port can't be reached at a known
		// address, so it is treated as not published.
		if host, ok := publishedPort(p.published); ok {
			svc.published[p.target] = host
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
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("service %q sets %s=%q, which is not a port", service, key, v)
	}
	return n, nil
}

// publishedPort reads the `published` value of a compose port, which
// Compose writes as a string ("8081") and some versions as a number.
func publishedPort(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, err := strconv.Atoi(s)
		return n, err == nil && n > 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, n > 0
	}
	return 0, false
}
