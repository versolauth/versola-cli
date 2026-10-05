package topology

import (
	"fmt"
	"strings"
	"testing"
)

// localConfig is shaped like `docker compose config --format json` for a
// local deployment: bridge network, only the diagnostics ports published.
const localConfig = `{
  "name": "versola-local",
  "services": {
    "postgres": {"image": "postgres:18"},
    "central": {
      "environment": {"PORT": "8090", "DPORT": "8091", "CONFIG_PATH": "/app/config/env.conf"},
      "ports": [{"mode": "ingress", "target": 8091, "published": "8091", "protocol": "tcp"}]
    },
    "auth": {
      "environment": {"PORT": "8080", "DPORT": "8081", "APORT": "8082"},
      "ports": [{"mode": "ingress", "host_ip": "127.0.0.1", "target": 8081, "published": "8081", "protocol": "tcp"}]
    },
    "edge": {
      "environment": {"PORT": "8095", "DPORT": "8096"},
      "ports": [{"target": 8096, "published": 8096}]
    }
  }
}`

// vpsConfig: host network, so no `ports:` at all.
const vpsConfig = `{
  "services": {
    "auth": {"environment": {"PORT": "8080", "DPORT": "8081", "APORT": "8082", "BIND_HOST": "127.0.0.1"}},
    "edge": {"environment": {"PORT": "8095", "DPORT": "8096"}}
  }
}`

func mustParse(t *testing.T, s string) Topology {
	t.Helper()
	topo, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func mustService(t *testing.T, topo Topology, name string) Service {
	t.Helper()
	s, err := topo.Service(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServicePorts(t *testing.T) {
	topo := mustParse(t, localConfig)
	for _, c := range []struct {
		name                      string
		port, diag, addl, diagURL int
	}{
		{"central", 8090, 8091, DefaultAdditionalPort, 8091},
		{"auth", 8080, 8081, 8082, 8081},
		{"edge", 8095, 8096, DefaultAdditionalPort, 8096}, // published as a number, not a string
	} {
		s := mustService(t, topo, c.name)
		if s.Port != c.port || s.DiagnosticsPort != c.diag || s.AdditionalPort != c.addl {
			t.Errorf("%s: got %d/%d/%d, want %d/%d/%d", c.name, s.Port, s.DiagnosticsPort, s.AdditionalPort, c.port, c.diag, c.addl)
		}
		if got := s.HostPort(s.DiagnosticsPort); got != c.diagURL {
			t.Errorf("%s: HostPort(DPORT) = %d, want %d", c.name, got, c.diagURL)
		}
	}
}

func TestHostPortWithoutPublish(t *testing.T) {
	s := mustService(t, mustParse(t, vpsConfig), "auth")
	// network_mode: host -- the container's port is the host's.
	if got := s.HostPort(s.DiagnosticsPort); got != 8081 {
		t.Errorf("got %d, want 8081", got)
	}
	// Local: 8080 isn't published (only DPORT is), so it stays itself.
	l := mustService(t, mustParse(t, localConfig), "auth")
	if got := l.HostPort(8080); got != 8080 {
		t.Errorf("got %d, want 8080", got)
	}
}

func TestHostPortRemapped(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"published":"18081"}]}}}`)
	s := mustService(t, topo, "auth")
	if got := s.HostPort(8081); got != 18081 {
		t.Errorf("got %d, want 18081", got)
	}
}

func TestDefaultsWhenUnset(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{},"edge":{"environment":{"PORT":null}}}}`)
	for _, name := range []string{"auth", "edge"} {
		s := mustService(t, topo, name)
		if s.Port != DefaultPort || s.DiagnosticsPort != DefaultDiagnosticsPort || s.AdditionalPort != DefaultAdditionalPort {
			t.Errorf("%s: got %d/%d/%d, want the application defaults", name, s.Port, s.DiagnosticsPort, s.AdditionalPort)
		}
	}
}

// compose config can write a port as a number, and a service's environment
// can have any shape: none of that may fail the parse or change another
// service's ports.
func TestUnusualEnvironments(t *testing.T) {
	topo := mustParse(t, `{"services":{
	  "central":{"environment":{"PORT":8090,"DPORT":8091,"OTHER":{"nested":true},"N":7}},
	  "auth":{"environment":["PORT=1234"]},
	  "edge":{"environment":"nonsense"}}}`)
	if s := mustService(t, topo, "central"); s.Port != 8090 || s.DiagnosticsPort != 8091 {
		t.Errorf("central: got %d/%d, want 8090/8091 from numeric values", s.Port, s.DiagnosticsPort)
	}
	for _, name := range []string{"auth", "edge"} {
		if s := mustService(t, topo, name); s.Port != DefaultPort {
			t.Errorf("%s: an environment that isn't an object reads as empty, got PORT %d", name, s.Port)
		}
	}
}

// compose config inlines env_file values -- secrets -- into environment.
// Only the three port variables may survive parsing.
func TestSecretsAreNotRetained(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{
	  "PORT":"8080","POSTGRES_PASSWORD":"hunter2","JWT_PRIVATE_KEY":"s3cr3t-key"}}}}`)
	dump := fmt.Sprintf("%v %+v %#v", topo, topo, topo)
	for _, secret := range []string{"hunter2", "s3cr3t-key", "POSTGRES_PASSWORD"} {
		if strings.Contains(dump, secret) {
			t.Errorf("the parsed topology still holds %q", secret)
		}
	}
}

func TestBadPortIsAnErrorForThatServiceOnly(t *testing.T) {
	topo := mustParse(t, `{"services":{
	  "auth":{"environment":{"PORT":"${AUTH_PORT}"}},
	  "edge":{"environment":{"DPORT":"70000"}},
	  "central":{"environment":{"PORT":"8090"}}}}`)
	if _, err := topo.Service("auth"); err == nil {
		t.Error("auth: want an error for an unresolved PORT")
	}
	if _, err := topo.Service("edge"); err == nil {
		t.Error("edge: want an error for an out-of-range DPORT")
	}
	if _, err := topo.Service("central"); err != nil {
		t.Errorf("central is fine: %v", err)
	}
}

func TestUnpublishedAndUDPPortsIgnored(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[
	  {"target":8081,"published":"8000-8010"},
	  {"target":8081,"published":"","protocol":"tcp"},
	  {"target":8081,"published":"9000","protocol":"udp"}]}}}`)
	s := mustService(t, topo, "auth")
	if got := s.HostPort(8081); got != 8081 {
		t.Errorf("got %d, want 8081 (none of those is a usable mapping)", got)
	}
}

func TestMissingService(t *testing.T) {
	if _, err := mustParse(t, vpsConfig).Service("central"); err == nil {
		t.Error("want an error for a service the compose file doesn't have")
	}
}

func TestParseErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not json":    "services:\n  auth: {}",
		"no services": `{"name":"x"}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestSlotService(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{1, "auth"}, {2, "auth-2"}, {9, "auth-9"}} {
		if got := SlotService("auth", c.n); got != c.want {
			t.Errorf("slot %d: got %q, want %q", c.n, got, c.want)
		}
	}
}

func TestValidSlot(t *testing.T) {
	for _, n := range []int{0, -1, MaxSlot + 1} {
		if ValidSlot(n) == nil {
			t.Errorf("slot %d should be invalid", n)
		}
	}
	for n := 1; n <= MaxSlot; n++ {
		if err := ValidSlot(n); err != nil {
			t.Errorf("slot %d: %v", n, err)
		}
	}
}

func TestForSlot(t *testing.T) {
	auth := mustService(t, mustParse(t, localConfig), "auth")

	one, err := auth.ForSlot(1)
	if err != nil {
		t.Fatal(err)
	}
	if one.Port != 8080 || one.DiagnosticsPort != 8081 || one.AdditionalPort != 8082 || one.HostPort(8081) != 8081 {
		t.Errorf("slot 1 must be the service as declared, got %+v", one)
	}

	three, err := auth.ForSlot(3)
	if err != nil {
		t.Fatal(err)
	}
	if three.Port != 8280 || three.DiagnosticsPort != 8281 || three.AdditionalPort != 8282 {
		t.Errorf("slot 3: got %d/%d/%d", three.Port, three.DiagnosticsPort, three.AdditionalPort)
	}
	if got := three.HostPort(three.DiagnosticsPort); got != 8281 {
		t.Errorf("slot 3 published DPORT: got %d, want 8281", got)
	}
	// ForSlot must not alter the service it was called on.
	if auth.HostPort(8081) != 8081 || auth.Port != 8080 {
		t.Error("ForSlot modified its receiver")
	}
}

func TestForSlotRejects(t *testing.T) {
	auth := mustService(t, mustParse(t, localConfig), "auth")
	if _, err := auth.ForSlot(0); err == nil {
		t.Error("slot 0: want error")
	}
	if _, err := auth.ForSlot(MaxSlot + 1); err == nil {
		t.Error("slot beyond MaxSlot: want error")
	}
	high := mustService(t, mustParse(t, `{"services":{"auth":{"environment":{"PORT":"65000"}}}}`), "auth")
	if _, err := high.ForSlot(9); err == nil {
		t.Error("want an error when a slot's port would exceed 65535")
	}
}

// Every slot of every Versola service has to get its own ports, or two
// replicas would fight over one.
func TestSlotsNeverCollide(t *testing.T) {
	topo := mustParse(t, localConfig)
	seen := map[int]string{}
	for _, name := range []string{"central", "auth", "edge"} {
		base := mustService(t, topo, name)
		for n := 1; n <= MaxSlot; n++ {
			s, err := base.ForSlot(n)
			if err != nil {
				t.Fatal(err)
			}
			// APORT is auth's alone (central and edge fall back to the
			// same default and never listen on it). Host ports count too:
			// they are what has to be free on the machine.
			ports := []int{s.Port, s.DiagnosticsPort, s.HostPort(s.DiagnosticsPort)}
			if name == "auth" {
				ports = append(ports, s.AdditionalPort)
			}
			who := SlotService(name, n)
			own := map[int]bool{}
			for _, p := range ports {
				if own[p] {
					continue // the same port listed twice for one replica (published == container)
				}
				own[p] = true
				if other, dup := seen[p]; dup {
					t.Errorf("port %d is used by both %s and %s", p, other, who)
				}
				seen[p] = who
			}
		}
	}
}

func TestPublished(t *testing.T) {
	topo, err := Parse([]byte(localConfig))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := topo.Service("auth")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := auth.Published(8081); !ok || p != 8081 {
		t.Errorf("DPORT is published on 8081, got %d, %v", p, ok)
	}
	// Unpublished container ports occupy nothing on the host, unlike
	// HostPort, which falls back to the container port for reaching it.
	if p, ok := auth.Published(8080); ok {
		t.Errorf("PORT is not published, got %d", p)
	}
	if got := auth.HostPort(8080); got != 8080 {
		t.Errorf("HostPort falls back to the port itself, got %d", got)
	}
	slot2, err := auth.ForSlot(2)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := slot2.Published(8181); !ok || p != 8181 {
		t.Errorf("slot 2's DPORT publishes on 8181, got %d, %v", p, ok)
	}
}

// The value in a "not a port" error can come from an env file that
// `compose config` inlined, so only a short prefix of it is quoted.
func TestBadPortErrorShortensValue(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"PORT":"`+strings.Repeat("s3cret", 20)+`"}}}}`)
	_, err := topo.Service("auth")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), strings.Repeat("s3cret", 4)) || len(err.Error()) > 120 {
		t.Errorf("the value must be shortened, got %q", err)
	}
	if !strings.Contains(err.Error(), "PORT") || !strings.Contains(err.Error(), "s3cret") {
		t.Errorf("still names the variable and the start of its value: %q", err)
	}
}
