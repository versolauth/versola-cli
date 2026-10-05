package topology

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// localConfig is shaped like `docker compose config --format json` for a
// local deployment: bridge network, only the diagnostics ports published
// (auth's with the loopback host_ip Compose reports for it, the others as
// the short syntax without one, one of them with a numeric published).
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

// remappedConfig is localConfig with every diagnostics port published on a
// different host port, so that a host port can't be mistaken for the
// container's.
const remappedConfig = `{
  "services": {
    "central": {
      "environment": {"PORT": "8090", "DPORT": "8091"},
      "ports": [{"target": 8091, "host_ip": "127.0.0.1", "published": "18091"}]
    },
    "auth": {
      "environment": {"PORT": "8080", "DPORT": "8081", "APORT": "8082"},
      "ports": [{"target": 8081, "host_ip": "127.0.0.1", "published": "18081"}]
    },
    "edge": {
      "environment": {"PORT": "8095", "DPORT": "8096"},
      "ports": [{"target": 8096, "host_ip": "127.0.0.1", "published": "18096"}]
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
		name             string
		port, diag, addl int
	}{
		{"central", 8090, 8091, DefaultAdditionalPort},
		{"auth", 8080, 8081, 8082},
		{"edge", 8095, 8096, DefaultAdditionalPort}, // published as a number, not a string
	} {
		s := mustService(t, topo, c.name)
		if s.Port != c.port || s.DiagnosticsPort != c.diag || s.AdditionalPort != c.addl {
			t.Errorf("%s: got %d/%d/%d, want %d/%d/%d", c.name, s.Port, s.DiagnosticsPort, s.AdditionalPort, c.port, c.diag, c.addl)
		}
		if got, want := s.ProbeAddr(s.DiagnosticsPort), fmt.Sprintf("localhost:%d", c.diag); got != want {
			t.Errorf("%s: ProbeAddr(DPORT) = %s, want %s", c.name, got, want)
		}
	}
}

func TestProbeAddrWithoutPublish(t *testing.T) {
	s := mustService(t, mustParse(t, vpsConfig), "auth")
	// network_mode: host -- the container's port is the host's.
	if got := s.ProbeAddr(s.DiagnosticsPort); got != "localhost:8081" {
		t.Errorf("got %s, want localhost:8081", got)
	}
	// Local: 8080 isn't published (only DPORT is), so it stays itself.
	l := mustService(t, mustParse(t, localConfig), "auth")
	if got := l.ProbeAddr(8080); got != "localhost:8080" {
		t.Errorf("got %s, want localhost:8080", got)
	}
}

func TestProbeAddrRemapped(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"published":"18081"}]}}}`)
	s := mustService(t, topo, "auth")
	if got := s.ProbeAddr(8081); got != "localhost:18081" {
		t.Errorf("got %s, want localhost:18081", got)
	}
}

// With network_mode: host Docker ignores `ports:`, so they must be too: a
// stray entry would send the readiness probe to a port nobody listens on.
func TestHostNetworkIgnoresPorts(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"network_mode":"host","environment":{"DPORT":"8081"},"ports":[{"target":8081,"published":"18081"}]}}}`)
	s := mustService(t, topo, "auth")
	if got := s.Publications(); len(got) != 0 {
		t.Errorf("want no publications, got %+v", got)
	}
	if got := s.ProbeAddr(8081); got != "localhost:8081" {
		t.Errorf("got %s, want localhost:8081", got)
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
	if got := s.Publications(); len(got) != 0 {
		t.Errorf("none of those is a usable mapping, got %+v", got)
	}
	if got := s.ProbeAddr(8081); got != "localhost:8081" {
		t.Errorf("got %s, want localhost:8081", got)
	}
}

// Odd entries are skipped one by one, not allowed to fail the whole
// configuration, and the checks are not case-sensitive.
func TestOddPortEntries(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[
	  {"target":"8081","host_ip":"127.0.0.1","published":"18081"},
	  {"target":8081,"published":"70000"},
	  {"target":8081,"published":"0"},
	  {"target":0,"published":"9000"},
	  {"target":70000,"published":"9001"},
	  {"target":[1],"published":"9002"},
	  {"target":8081,"host_ip":"10.0.0.1","published":"18082","protocol":"TCP"},
	  {"target":8081,"published":"9003","protocol":"UDP"}]}}}`)
	s := mustService(t, topo, "auth")
	want := []Publication{
		{Target: 8081, Binding: Binding{HostIP: "127.0.0.1", Port: 18081}},
		{Target: 8081, Binding: Binding{HostIP: "10.0.0.1", Port: 18082}},
	}
	if got := s.Publications(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
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
	auth := mustService(t, mustParse(t, remappedConfig), "auth")

	one, err := auth.ForSlot(1)
	if err != nil {
		t.Fatal(err)
	}
	if one.Port != 8080 || one.DiagnosticsPort != 8081 || one.AdditionalPort != 8082 || one.ProbeAddr(8081) != "localhost:18081" {
		t.Errorf("slot 1 must be the service as declared, got %+v", one)
	}

	three, err := auth.ForSlot(3)
	if err != nil {
		t.Fatal(err)
	}
	if three.Port != 8280 || three.DiagnosticsPort != 8281 || three.AdditionalPort != 8282 {
		t.Errorf("slot 3: got %d/%d/%d", three.Port, three.DiagnosticsPort, three.AdditionalPort)
	}
	if got := three.ProbeAddr(three.DiagnosticsPort); got != "localhost:18281" {
		t.Errorf("slot 3 published DPORT: got %s, want localhost:18281", got)
	}
	// ForSlot must not alter the service it was called on.
	if auth.ProbeAddr(8081) != "localhost:18081" || auth.Port != 8080 {
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
	// Either side of a publication can be what runs out; the message names it.
	for name, ports := range map[string]string{
		"host":      `[{"target":8081,"published":"64900"}]`,
		"container": `[{"target":64900,"published":"8081"}]`,
	} {
		svc := mustService(t, mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":`+ports+`}}}`), "auth")
		_, err := svc.ForSlot(2)
		if err != nil {
			t.Errorf("%s: slot 2 fits: %v", name, err)
		}
		_, err = svc.ForSlot(9)
		want := map[string]string{
			"host":      "host port 65700 (for container port 8881)",
			"container": "container port 65700",
		}[name]
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("%s: want an error naming %q, got %v", name, want, err)
		}
	}
}

// Every slot of every Versola service has to get its own ports, or two
// replicas would fight over one.
func TestSlotsNeverCollide(t *testing.T) {
	topo := mustParse(t, remappedConfig)
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
			ports := []int{s.Port, s.DiagnosticsPort}
			for _, p := range s.Publications() {
				ports = append(ports, p.Port)
			}
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

func TestPublications(t *testing.T) {
	topo, err := Parse([]byte(remappedConfig))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := topo.Service("auth")
	if err != nil {
		t.Fatal(err)
	}
	want := []Publication{{Target: 8081, Binding: Binding{HostIP: "127.0.0.1", Port: 18081}}}
	if got := auth.Publications(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// A copy: changing it must not change the service.
	auth.Publications()[0].Port = 1
	if got := auth.ProbeAddr(8081); got != "localhost:18081" {
		t.Errorf("Publications must return a copy, ProbeAddr is now %s", got)
	}
	// Unpublished container ports occupy nothing on the host, unlike
	// ProbeAddr, which says where to reach them.
	for _, p := range auth.Publications() {
		if p.Target == 8080 {
			t.Errorf("PORT is not published, got %+v", p)
		}
	}
	if got := auth.ProbeAddr(8080); got != "localhost:8080" {
		t.Errorf("ProbeAddr falls back to the port itself, got %s", got)
	}
	slot2, err := auth.ForSlot(2)
	if err != nil {
		t.Fatal(err)
	}
	want = []Publication{{Target: 8181, Binding: Binding{HostIP: "127.0.0.1", Port: 18181}}}
	if got := slot2.Publications(); !reflect.DeepEqual(got, want) {
		t.Errorf("slot 2 shifts the port and keeps the address: got %+v, want %+v", got, want)
	}
}

// One container port can be published several times. The one localhost
// reaches decides where it is probed, whatever the order; failing that, a
// binding to another host IP is probed at that IP.
func TestSeveralBindingsForOneTarget(t *testing.T) {
	for _, c := range []struct {
		name  string
		ports string
		want  string
	}{
		{"loopback first", `[{"target":8081,"host_ip":"127.0.0.1","published":"8081"},{"target":8081,"host_ip":"192.168.1.10","published":"18081"}]`, "localhost:8081"},
		{"loopback last", `[{"target":8081,"host_ip":"192.168.1.10","published":"18081"},{"target":8081,"host_ip":"127.0.0.1","published":"8081"}]`, "localhost:8081"},
		{"every interface", `[{"target":8081,"host_ip":"192.168.1.10","published":"18081"},{"target":8081,"host_ip":"0.0.0.0","published":"28081"}]`, "localhost:28081"},
		{"no host_ip", `[{"target":8081,"published":"18081"}]`, "localhost:18081"},
		{"ipv6 loopback", `[{"target":8081,"host_ip":"::1","published":"18081"}]`, "localhost:18081"},
		{"only a LAN address", `[{"target":8081,"host_ip":"192.168.1.10","published":"18081"}]`, "192.168.1.10:18081"},
		{"other loopback address", `[{"target":8081,"host_ip":"127.0.0.2","published":"18081"}]`, "127.0.0.2:18081"},
		{"a name instead of an address", `[{"target":8081,"host_ip":"myhost","published":"18081"}]`, "myhost:18081"},
		{"link-local with a zone", `[{"target":8081,"host_ip":"fe80::1%eth0","published":"18081"}]`, "[fe80::1%eth0]:18081"},
		{"only an IPv6 LAN address", `[{"target":8081,"host_ip":"fd00::5","published":"18081"}]`, "[fd00::5]:18081"},
		{"first of several LAN addresses", `[{"target":8081,"host_ip":"192.168.1.10","published":"18081"},{"target":8081,"host_ip":"192.168.1.11","published":"28081"}]`, "192.168.1.10:18081"},
		{"another target's binding is not this one's", `[{"target":8082,"host_ip":"127.0.0.1","published":"18082"}]`, "localhost:8081"},
	} {
		topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":`+c.ports+`}}}`)
		s := mustService(t, topo, "auth")
		if got := s.ProbeAddr(8081); got != c.want {
			t.Errorf("%s: ProbeAddr = %s, want %s", c.name, got, c.want)
		}
		if got := len(s.Publications()); got != strings.Count(c.ports, `"target"`) {
			t.Errorf("%s: every binding must be kept, got %d", c.name, got)
		}
	}
}

func TestBinding(t *testing.T) {
	b := func(ip string, port int) Binding { return Binding{HostIP: ip, Port: port} }
	for _, c := range []struct {
		name string
		a, b Binding
		want bool
	}{
		{"same", b("127.0.0.1", 80), b("127.0.0.1", 80), true},
		{"other port", b("127.0.0.1", 80), b("127.0.0.1", 81), false},
		{"other loopback address", b("127.0.0.1", 80), b("127.0.0.2", 80), false},
		{"empty covers any", b("", 80), b("127.0.0.2", 80), true},
		{"empty covers any, reversed", b("10.0.0.1", 80), b("", 80), true},
		{"v4 wildcard covers v4", b("0.0.0.0", 80), b("10.0.0.1", 80), true},
		{"v4 wildcard does not cover v6", b("0.0.0.0", 80), b("::1", 80), false},
		{"v6 wildcard covers v6", b("::", 80), b("::1", 80), true},
		{"v6 wildcard does not cover v4", b("::", 80), b("127.0.0.1", 80), false},
		{"bracketed v6", b("[::1]", 80), b("::1", 80), true},
		{"v4-mapped v6 is v4", b("::ffff:127.0.0.1", 80), b("127.0.0.1", 80), true},
		{"not an IP: equal text", b("eth0", 80), b("eth0", 80), true},
		{"not an IP: different text", b("eth0", 80), b("eth1", 80), false},
		{"a name against an address: assume the worst", b("localhost", 80), b("127.0.0.1", 80), true},
		{"a name is not case-sensitive", b("LocalHost", 80), b("localhost", 80), true},
		{"a zone on one side only is ignored", b("fe80::1%eth0", 80), b("fe80::1", 80), true},
		{"same zone", b("fe80::1%eth0", 80), b("fe80::1%eth0", 80), true},
		{"different zones are different sockets", b("fe80::1%eth0", 80), b("fe80::1%eth1", 80), false},
	} {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Errorf("%s: Overlaps = %v, want %v", c.name, got, c.want)
		}
		if got := c.b.Overlaps(c.a); got != c.want {
			t.Errorf("%s: Overlaps is not symmetric", c.name)
		}
	}
	for ip, want := range map[string]bool{
		"": true, "localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true,
		"0.0.0.0": true, "::": true, "127.0.0.2": false, "192.168.1.10": false, "eth0": false,
		"LOCALHOST": true, "::1%lo": true, "[::ffff:127.0.0.1]": true,
	} {
		if got := b(ip, 1).local(); got != want {
			t.Errorf("local(%q) = %v, want %v", ip, got, want)
		}
	}
	if got := b("127.0.0.1", 80).String(); got != "127.0.0.1:80" {
		t.Errorf("got %s", got)
	}
	if got := b("::1", 80).String(); got != "[::1]:80" {
		t.Errorf("got %s", got)
	}
	if got := b("[::1]", 80).String(); got != "[::1]:80" {
		t.Errorf("brackets must not be doubled: %s", got)
	}
	if got := b("", 80).String(); got != "80" {
		t.Errorf("got %s", got)
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

// Compose never writes these shapes, but a service or entry that has one
// must cost only itself, not the whole configuration.
func TestOddShapesDoNotFailParse(t *testing.T) {
	for name, cfg := range map[string]string{
		"ports null":          `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":null}}}`,
		"ports a string":      `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":"x"}}}`,
		"ports an object":     `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":{"a":1}}}}`,
		"an entry a string":   `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":["x",null,5]}}}`,
		"host_ip a number":    `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"host_ip":5,"published":"18081"}]}}}`,
		"network_mode number": `{"services":{"auth":{"environment":{"DPORT":"8081"},"network_mode":5}}}`,
		"protocol a number":   `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"published":"18081","protocol":7}]}}}`,
		"the service null":    `{"services":{"auth":null}}`,
	} {
		topo, err := Parse([]byte(cfg))
		if err != nil {
			t.Errorf("%s: Parse failed: %v", name, err)
			continue
		}
		if _, err := topo.Service("auth"); err != nil {
			t.Errorf("%s: Service failed: %v", name, err)
		}
	}
	topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"published":"18081","protocol":7}]}}}`)
	if got := len(mustService(t, topo, "auth").Publications()); got != 1 {
		t.Errorf("a protocol that is not a string counts as the default (tcp), got %d publications", got)
	}
	if _, err := Parse([]byte(`{"services":5}`)); err == nil {
		t.Error("services that is not an object: want an error")
	}
	// A service that is not an object costs only itself: the others are
	// read, and asking for it is an error, not the default ports.
	odd := mustParse(t, `{"services":{"auth":"x","edge":{"environment":{"PORT":"8095"}}}}`)
	if _, err := odd.Service("auth"); err == nil || !strings.Contains(err.Error(), "unexpected shape") {
		t.Errorf("want an error for a service that is not an object, got %v", err)
	}
	if s := mustService(t, odd, "edge"); s.Port != 8095 {
		t.Errorf("the other services are unaffected, got %d", s.Port)
	}
}

// What counts as one plain port, however Compose spells it.
func TestPlainPort(t *testing.T) {
	for in, want := range map[string]int{
		`"8081"`: 8081, `8081`: 8081, `8081.0`: 8081, `1.8081e4`: 18081, `"018081"`: 18081, `"1"`: 1, `65535`: 65535,
		`"+8081"`: 0, `"-1"`: 0, `""`: 0, `"8000-8010"`: 0, `"0"`: 0, `0`: 0, `65536`: 0, `8081.5`: 0, `-8081`: 0,
		`null`: 0, `[1]`: 0, `true`: 0, `"80 "`: 0, `"0x50"`: 0, `"99999999999999999999"`: 0,
	} {
		got, ok := plainPort(json.RawMessage(in))
		if (want != 0) != ok || (ok && got != want) {
			t.Errorf("plainPort(%s) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := plainPort(nil); ok {
		t.Error("absent: not a port")
	}
}

func TestHostNetworkFlag(t *testing.T) {
	topo := mustParse(t, `{"services":{"a":{"network_mode":"host"},"b":{},"c":{"network_mode":"bridge"}}}`)
	for name, want := range map[string]bool{"a": true, "b": false, "c": false} {
		if got := mustService(t, topo, name).HostNetwork(); got != want {
			t.Errorf("%s: HostNetwork = %v, want %v", name, got, want)
		}
	}
	a, err := mustService(t, topo, "a").ForSlot(2)
	if err != nil || !a.HostNetwork() {
		t.Errorf("a slot keeps the network mode: %v, %v", a.HostNetwork(), err)
	}
}

// An environment value that is not a port is an error, never a quiet
// fall-back to the default.
func TestOddEnvironmentValues(t *testing.T) {
	for _, c := range []struct {
		value string
		ok    bool
		port  int
	}{
		{`"8080"`, true, 8080}, {`8080`, true, 8080}, {`8080.0`, true, 8080},
		{`8080.5`, false, 0}, {`true`, false, 0}, {`{"a":1}`, false, 0}, {`[8080]`, false, 0},
		{`"+8080"`, false, 0}, {`"0"`, false, 0}, {`"70000"`, false, 0}, {`""`, false, 0}, {`1e12`, false, 0},
	} {
		topo := mustParse(t, `{"services":{"auth":{"environment":{"PORT":`+c.value+`}}}}`)
		s, err := topo.Service("auth")
		if (err == nil) != c.ok || err == nil && s.Port != c.port {
			t.Errorf("PORT=%s: port %d, err %v; want ok=%v port %d", c.value, s.Port, err, c.ok, c.port)
		}
	}
}

func TestPublicationString(t *testing.T) {
	p := Publication{Target: 8081, Binding: Binding{HostIP: "127.0.0.1", Port: 18081}}
	if got, want := fmt.Sprintf("%v|%+v", p, p), "8081 published on 127.0.0.1:18081|8081 published on 127.0.0.1:18081"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// An entry with no host port (Compose picks one) publishes nothing known.
func TestEntryWithoutPublished(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081},{"target":8081,"published":null}]}}}`)
	s := mustService(t, topo, "auth")
	if got := s.Publications(); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
	if got := s.ProbeAddr(8081); got != "localhost:8081" {
		t.Errorf("got %s", got)
	}
}
