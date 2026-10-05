package deploy

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

const testCompose = `{"services":{
  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"published":"8081"}]},
  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"8096"}]}}}`

func testTopology(t *testing.T) topology.Topology {
	t.Helper()
	topo, err := topology.Parse([]byte(testCompose))
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func TestReplicasOfSlotOne(t *testing.T) {
	topo := testTopology(t)
	st := &state.State{Version: "0.6.2"}

	auth, err := replicasOf(topo, AuthService, st.ActiveSlots(AuthService))
	if err != nil {
		t.Fatal(err)
	}
	if len(auth) != 1 || auth[0].Service != "auth" || auth[0].Slot != 1 || auth[0].Ports.Port != 8080 {
		t.Fatalf("a deployment with no recorded slots must be exactly one unshifted auth, got %+v", auth)
	}

	// The values this PR replaces: what up.go and the proxy used to hardcode.
	central, err := replicasOf(topo, CentralService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	edge, err := replicasOf(topo, EdgeService, st.ActiveSlots(EdgeService))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		r    replica
		url  string
	}{
		{"central", central[0], "http://localhost:8091/readiness"},
		{"auth", auth[0], "http://localhost:8081/readiness"},
		{"edge", edge[0], "http://localhost:8096/readiness"},
	} {
		if got := readyURL(t, true, c.r.Ports); got != c.url {
			t.Errorf("%s: got %s, want %s", c.name, got, c.url)
		}
	}
	if got := proxyBackends(auth); !reflect.DeepEqual(got, []proxy.Backend{{Service: "auth", Port: 8080}}) {
		t.Errorf("auth backends: %+v", got)
	}
	if got := proxyBackends(edge); !reflect.DeepEqual(got, []proxy.Backend{{Service: "edge", Port: 8095}}) {
		t.Errorf("edge backends: %+v", got)
	}
	if got := serviceNames(auth, edge); !reflect.DeepEqual(got, []string{"auth", "edge"}) {
		t.Errorf("service names: %v", got)
	}
}

func TestReplicasOfSeveralSlots(t *testing.T) {
	st := &state.State{Version: "0.6.2", Slots: map[string][]state.Slot{
		"auth": {{N: 1, Version: "0.6.2"}, {N: 3, Version: "0.6.3"}},
	}}
	auth, err := replicasOf(testTopology(t), AuthService, st.ActiveSlots(AuthService))
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(auth); !reflect.DeepEqual(got, []string{"auth", "auth-3"}) {
		t.Errorf("service names: %v", got)
	}
	if got := readyURL(t, true, auth[1].Ports); got != "http://localhost:8281/readiness" {
		t.Errorf("slot 3 readiness: %s", got)
	}
	want := []proxy.Backend{{Service: "auth", Port: 8080}, {Service: "auth-3", Port: 8280}}
	if got := proxyBackends(auth); !reflect.DeepEqual(got, want) {
		t.Errorf("backends: got %+v, want %+v", got, want)
	}
}

func TestReplicasOfErrors(t *testing.T) {
	topo := testTopology(t)
	if _, err := replicasOf(topo, "nope", []state.Slot{{N: 1}}); err == nil {
		t.Error("an unknown service: want error")
	}
	if _, err := replicasOf(topo, AuthService, []state.Slot{{N: 10}}); err == nil {
		t.Error("a slot beyond the maximum: want error")
	}
}

// Local publishes diagnostics ports on the host; when the host port differs
// from the container's, readiness has to be asked there.
func TestReadinessUsesPublishedPort(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{"auth":{
	  "environment":{"PORT":"8080","DPORT":"8081"},
	  "ports":[{"target":8081,"published":"18081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readyURL(t, false, auth[0].Ports); got != "http://localhost:18081/readiness" {
		t.Errorf("slot 1: got %s", got)
	}
	if got := readyURL(t, false, auth[1].Ports); got != "http://localhost:18181/readiness" {
		t.Errorf("slot 2: got %s", got)
	}
}

func TestCheckNoPortClash(t *testing.T) {
	topo := testTopology(t)
	one := []state.Slot{{N: 1}}
	central, _ := replicasOf(topo, CentralService, one)
	auth, _ := replicasOf(topo, AuthService, one)
	edge, _ := replicasOf(topo, EdgeService, one)
	// Both networks: Versola's own ports are distinct. (Edge and central
	// carry a default APORT of 8082 too, which is auth's -- it must not
	// count, they never bind it.)
	for _, host := range []bool{true, false} {
		if err := checkNoPortClash(host, nil, central, auth, edge); err != nil {
			t.Errorf("hostNetwork=%v: Versola's own ports are distinct: %v", host, err)
		}
	}

	// edge without PORT/DPORT in its compose entry falls back to auth's.
	bare, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"}},
	  "edge":{"environment":{"BIND_HOST":"127.0.0.1"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := replicasOf(bare, AuthService, one)
	e, _ := replicasOf(bare, EdgeService, one)
	err = checkNoPortClash(true, nil, a, e)
	if err == nil || !strings.Contains(err.Error(), "auth (PORT)") || !strings.Contains(err.Error(), "edge (PORT)") || !strings.Contains(err.Error(), "8080") {
		t.Errorf("want auth (PORT) and edge (PORT) clashing on 8080, got %v", err)
	}

	// Several slots of one service are distinct by construction.
	many, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}, {N: 9}})
	for _, host := range []bool{true, false} {
		if err := checkNoPortClash(host, nil, many, edge); err != nil {
			t.Errorf("hostNetwork=%v: slots of auth must not clash with each other or edge: %v", host, err)
		}
	}

	// PORT == DPORT is one process binding a port twice: an error on any network.
	same, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8080"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := replicasOf(same, AuthService, one)
	for _, host := range []bool{true, false} {
		if err := checkNoPortClash(host, nil, s); err == nil {
			t.Errorf("hostNetwork=%v: PORT == DPORT: want error", host)
		}
	}
}

// auth's additional listener (APORT) takes a port on the host like any
// other: on vps an APORT equal to another service's PORT cannot be bound.
func TestCheckNoPortClashAdditionalPort(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"}},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8090"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	one := []state.Slot{{N: 1}}
	central, _ := replicasOf(topo, CentralService, one)
	auth, _ := replicasOf(topo, AuthService, one)
	err = checkNoPortClash(true, nil, central, auth)
	if err == nil || !strings.Contains(err.Error(), "central (PORT)") || !strings.Contains(err.Error(), "auth (APORT)") || !strings.Contains(err.Error(), "8090") {
		t.Fatalf("auth's APORT against central's PORT: want a clash on 8090, got %v", err)
	}
	if strings.Contains(err.Error(), "`ports:`") {
		t.Errorf("on the host network there is no ports: to check: %v", err)
	}
	// ... but not on a bridge, where nothing is published and the two
	// containers have their own network.
	if err := checkNoPortClash(false, nil, central, auth); err != nil {
		t.Errorf("bridge, nothing published: %v", err)
	}

	// APORT equal to auth's own PORT is one process binding twice, on any network.
	own, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8080"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := replicasOf(own, AuthService, one)
	for _, host := range []bool{true, false} {
		if err := checkNoPortClash(host, nil, o); err == nil {
			t.Errorf("hostNetwork=%v: APORT == PORT: want error", host)
		}
	}

	// A shifted slot's APORT is checked too: auth slot 2 listens on 8182.
	shifted, err := topology.Parse([]byte(`{"services":{
	  "central":{"environment":{"PORT":"8182","DPORT":"8091"}},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := replicasOf(shifted, CentralService, one)
	a2, _ := replicasOf(shifted, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err := checkNoPortClash(true, nil, c2, a2); err == nil || !strings.Contains(err.Error(), "8182") {
		t.Errorf("auth-2's APORT (8182) against central's PORT: want a clash, got %v", err)
	}
}

// On a bridge (local) each container has its own network: auth and edge may
// both listen on 8080 inside theirs. Only host ports that `ports:` publishes
// can collide.
func TestCheckNoPortClashBridge(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"18081"}]},
	  "edge":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"18096"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	one := []state.Slot{{N: 1}}
	auth, _ := replicasOf(topo, AuthService, one)
	edge, _ := replicasOf(topo, EdgeService, one)
	if err := checkNoPortClash(false, nil, auth, edge); err != nil {
		t.Errorf("same container ports behind different published host ports are fine on a bridge: %v", err)
	}
	// The same layout on the host network really would collide.
	if err := checkNoPortClash(true, nil, auth, edge); err == nil {
		t.Error("hostNetwork: two services on 8080 must clash")
	}

	// Two services publishing the same host port do collide on a bridge.
	dup, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"8081"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"8081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := replicasOf(dup, AuthService, one)
	e, _ := replicasOf(dup, EdgeService, one)
	err = checkNoPortClash(false, nil, a, e)
	if err == nil || !strings.Contains(err.Error(), "auth (DPORT)") || !strings.Contains(err.Error(), "edge (DPORT)") || !strings.Contains(err.Error(), "8081") {
		t.Fatalf("same published host port: want auth (DPORT) and edge (DPORT) clashing on 8081, got %v", err)
	}
	if !strings.Contains(err.Error(), "`ports:`") {
		t.Errorf("on a bridge the hint should mention ports: %v", err)
	}
}

// Publishing is per address: auth on 127.0.0.1:18080 and edge on
// 127.0.0.2:18080 are different sockets. And every published port counts,
// not just PORT/DPORT/APORT.
func TestCheckNoPortClashBridgeAddresses(t *testing.T) {
	check := func(authPorts, edgePorts string) error {
		t.Helper()
		topo, err := topology.Parse([]byte(`{"services":{
		  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":` + authPorts + `},
		  "edge":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":` + edgePorts + `}}}`))
		if err != nil {
			t.Fatal(err)
		}
		one := []state.Slot{{N: 1}}
		auth, _ := replicasOf(topo, AuthService, one)
		edge, _ := replicasOf(topo, EdgeService, one)
		return checkNoPortClash(false, nil, auth, edge)
	}

	if err := check(
		`[{"target":8080,"host_ip":"127.0.0.1","published":"18080"},{"target":8081,"host_ip":"127.0.0.1","published":"18081"}]`,
		`[{"target":8080,"host_ip":"127.0.0.2","published":"18080"},{"target":8081,"host_ip":"127.0.0.2","published":"18081"}]`); err != nil {
		t.Errorf("distinct addresses on the same ports are fine: %v", err)
	}
	err := check(
		`[{"target":8080,"host_ip":"127.0.0.1","published":"18080"}]`,
		`[{"target":8080,"host_ip":"127.0.0.1","published":"18080"}]`)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:18080") {
		t.Errorf("same address and port: want a clash naming it, got %v", err)
	}
	err = check(
		`[{"target":8080,"host_ip":"127.0.0.1","published":"18080"}]`,
		`[{"target":8080,"published":"18080"}]`)
	if err == nil || !strings.Contains(err.Error(), "auth (PORT) publishes 127.0.0.1:18080") || !strings.Contains(err.Error(), "edge (PORT) publishes 18080") {
		t.Errorf("a wildcard binding covers a specific one: want a clash naming both spellings, got %v", err)
	}
	if err := check(
		`[{"target":8080,"host_ip":"0.0.0.0","published":"18080"}]`,
		`[{"target":8080,"host_ip":"::1","published":"18080"}]`); err != nil {
		t.Errorf("an IPv4 wildcard does not cover an IPv6 address: %v", err)
	}

	// One container port published on two addresses is fine, and so is
	// publishing a port that is none of the listeners (MPORT) -- until two
	// services publish it on the same address.
	if err := check(
		`[{"target":8081,"host_ip":"127.0.0.1","published":"8081"},{"target":8081,"host_ip":"192.168.1.10","published":"18081"}]`,
		`[{"target":8081,"host_ip":"127.0.0.1","published":"8096"}]`); err != nil {
		t.Errorf("several bindings of one target: %v", err)
	}
	err = check(
		`[{"target":8083,"host_ip":"127.0.0.1","published":"8083"}]`,
		`[{"target":8083,"host_ip":"127.0.0.1","published":"8083"}]`)
	if err == nil || !strings.Contains(err.Error(), "port 8083") {
		t.Errorf("a non-listener port published twice: want a clash labelled 'port 8083', got %v", err)
	}
	// Every binding of a target takes part, not only the first or the last.
	err = check(
		`[{"target":8081,"host_ip":"127.0.0.1","published":"8081"},{"target":8081,"host_ip":"192.168.1.10","published":"18081"}]`,
		`[{"target":8081,"host_ip":"192.168.1.10","published":"18081"}]`)
	if err == nil || !strings.Contains(err.Error(), "192.168.1.10:18081") {
		t.Errorf("the second binding of a target must be checked: got %v", err)
	}
	err = check(
		`[{"target":8081,"host_ip":"192.168.1.10","published":"18081"},{"target":8081,"host_ip":"127.0.0.1","published":"8081"}]`,
		`[{"target":8081,"host_ip":"127.0.0.1","published":"8081"}]`)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:8081") {
		t.Errorf("the first binding of a target must be checked: got %v", err)
	}
	// The same target published twice on one binding is Docker's error too.
	err = check(
		`[{"target":8081,"host_ip":"127.0.0.1","published":"8081"},{"target":8081,"host_ip":"127.0.0.1","published":"8081"}]`,
		`[]`)
	if err == nil || !strings.Contains(err.Error(), "auth publishes 127.0.0.1:8081 for both DPORT and DPORT") {
		t.Errorf("a target published twice on one binding: got %v", err)
	}
	// ... and one service publishing two targets on the same binding.
	err = check(
		`[{"target":8080,"host_ip":"127.0.0.1","published":"9000"},{"target":8081,"host_ip":"127.0.0.1","published":"9000"}]`,
		`[]`)
	if err == nil || !strings.Contains(err.Error(), "auth publishes 127.0.0.1:9000 for both PORT and DPORT") {
		t.Errorf("two targets on one binding: want auth (PORT) and auth (DPORT), got %v", err)
	}
}

// The wording of each kind of clash.
func TestCheckNoPortClashMessages(t *testing.T) {
	one := []state.Slot{{N: 1}}
	build := func(config string, slots []state.Slot) (auth, edge []replica) {
		t.Helper()
		topo, err := topology.Parse([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		auth, _ = replicasOf(topo, AuthService, slots)
		edge, _ = replicasOf(topo, EdgeService, one)
		return auth, edge
	}

	// A host-network listener against a publication: one listens, one publishes.
	a, e := build(`{"services":{
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081"}},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"host_ip":"127.0.0.1","published":"8080"}]}}}`, one)
	err := checkNoPortClash(false, nil, a, e)
	want := "auth (PORT) listens on 8080 and edge (DPORT) publishes 127.0.0.1:8080, which is the same port on the host -- check PORT/DPORT/APORT and `ports:` in the compose file"
	if err == nil || err.Error() != want {
		t.Errorf("listener against publication:\n got %v\nwant %s", err, want)
	}

	// Two host-network listeners.
	a, e = build(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"}},
	  "edge":{"environment":{"PORT":"8080","DPORT":"8096"}}}}`, one)
	err = checkNoPortClash(true, nil, a, e)
	want = "auth (PORT) and edge (PORT) would both use port 8080 on the host -- check PORT/DPORT/APORT in the compose file"
	if err == nil || err.Error() != want {
		t.Errorf("two listeners:\n got %v\nwant %s", err, want)
	}

	// A replica clashing with itself in slot 2: the slot is explained once.
	a, _ = build(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8080"}}}}`, []state.Slot{{N: 2}})
	err = checkNoPortClash(true, nil, a)
	want = "auth-2 uses port 8180 for both PORT and DPORT (auth-2 is slot 2: its ports are auth's in the compose file plus 100) -- check the compose file"
	if err == nil || err.Error() != want {
		t.Errorf("own duplicate in slot 2:\n got %v\nwant %s", err, want)
	}
	a, _ = build(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[
	  {"target":8080,"host_ip":"127.0.0.1","published":"9000"},{"target":8081,"host_ip":"127.0.0.1","published":"9000"}]}}}`, []state.Slot{{N: 2}})
	err = checkNoPortClash(false, nil, a)
	if err == nil || strings.Count(err.Error(), "auth-2 is slot 2") != 1 {
		t.Errorf("a service's slot is explained once, got %v", err)
	}

	// Ports that are none of the listeners: only `ports:` to check.
	a, e = build(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8083,"host_ip":"127.0.0.1","published":"8083"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8083,"host_ip":"127.0.0.1","published":"8083"}]}}}`, one)
	err = checkNoPortClash(false, nil, a, e)
	if err == nil || !strings.HasSuffix(err.Error(), "-- check `ports:` in the compose file") || !strings.Contains(err.Error(), "auth (port 8083)") {
		t.Errorf("non-listener ports: got %v", err)
	}
}

// A published host port is shifted with its slot, so auth-2 publishing 8181
// collides with a service that already publishes 8181.
func TestCheckNoPortClashBridgeSlots(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8181"}]},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"8081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	central, _ := replicasOf(topo, CentralService, []state.Slot{{N: 1}})
	auth, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	err = checkNoPortClash(false, nil, central, auth)
	if err == nil || !strings.Contains(err.Error(), "auth-2 (DPORT)") || !strings.Contains(err.Error(), "8181") ||
		!strings.Contains(err.Error(), "auth-2 is slot 2: its ports are auth's in the compose file plus 100") {
		t.Errorf("want auth-2's published DPORT (8181) to clash with central, with the slot explained, got %v", err)
	}
}

// A loopback binding wins over a LAN one whatever the order, so the
// readiness probe does not time out on an address localhost does not reach.
func TestReadinessUsesALocalBinding(t *testing.T) {
	for name, ports := range map[string]string{
		"loopback first": `[{"target":8081,"host_ip":"127.0.0.1","published":"8081"},{"target":8081,"host_ip":"192.168.1.10","published":"18081"}]`,
		"loopback last":  `[{"target":8081,"host_ip":"192.168.1.10","published":"18081"},{"target":8081,"host_ip":"127.0.0.1","published":"8081"}]`,
	} {
		topo, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":` + ports + `}}}`))
		if err != nil {
			t.Fatal(err)
		}
		auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
		if err != nil {
			t.Fatal(err)
		}
		if got := readyURL(t, false, auth[0].Ports); got != "http://127.0.0.1:8081/readiness" {
			t.Errorf("%s, slot 1: got %s", name, got)
		}
		if got := readyURL(t, false, auth[1].Ports); got != "http://127.0.0.1:8181/readiness" {
			t.Errorf("%s, slot 2: got %s", name, got)
		}
	}
}

// The same port on two loopback addresses is two services: each is probed
// at its own address.
func TestReadinessOfServicesOnDifferentLoopbacks(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"host_ip":"127.0.0.1","published":"18081"}]},
	  "edge":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"host_ip":"::1","published":"18081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	edge, _ := replicasOf(topo, EdgeService, []state.Slot{{N: 1}})
	if got := readyURL(t, false, auth[0].Ports); got != "http://127.0.0.1:18081/readiness" {
		t.Errorf("auth: %s", got)
	}
	if got := readyURL(t, false, edge[0].Ports); got != "http://[::1]:18081/readiness" {
		t.Errorf("edge: %s", got)
	}
}

// A port published only on another host IP is probed there, not at a port
// that is not on the host at all.
func TestReadinessUsesTheBindingsAddress(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[
	  {"target":8081,"host_ip":"192.168.1.10","published":"18081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readyURL(t, false, auth[0].Ports); got != "http://192.168.1.10:18081/readiness" {
		t.Errorf("slot 1: got %s", got)
	}
	if got := readyURL(t, false, auth[1].Ports); got != "http://192.168.1.10:18181/readiness" {
		t.Errorf("slot 2: got %s", got)
	}
}

// The proxy holds host ports too; a service must not take them.
func TestCheckNoPortClashProxy(t *testing.T) {
	one := []state.Slot{{N: 1}}
	replicas := func(config string) ([]replica, []replica) {
		topo, err := topology.Parse([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := replicasOf(topo, AuthService, one)
		e, _ := replicasOf(topo, EdgeService, one)
		return a, e
	}
	local := proxyReservations([]int{2821}, proxy.ModeLocal)

	// bridge (local): a service publishing the proxy's address
	a, e := replicas(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"host_ip":"127.0.0.1","published":"2821"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"}}}}`)
	err := checkNoPortClash(false, local, a, e)
	if err == nil || !strings.Contains(err.Error(), "reverse proxy") || !strings.Contains(err.Error(), "auth (DPORT)") || !strings.Contains(err.Error(), "127.0.0.1:2821") {
		t.Errorf("auth publishing the proxy's port: want a clash naming the proxy, got %v", err)
	}
	// ... on another address it does not
	a, e = replicas(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"host_ip":"127.0.0.2","published":"2821"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"}}}}`)
	if err := checkNoPortClash(false, local, a, e); err != nil {
		t.Errorf("another address: %v", err)
	}

	// host network (vps): a service listening on the proxy's port
	a, e = replicas(`{"services":{
	  "auth":{"environment":{"PORT":"2821","DPORT":"8081"}},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"}}}}`)
	err = checkNoPortClash(true, proxyReservations([]int{2821}, proxy.ModeExternal), a, e)
	if err == nil || !strings.Contains(err.Error(), "reverse proxy") || !strings.Contains(err.Error(), "auth (PORT)") || !strings.Contains(err.Error(), "2821") {
		t.Errorf("PORT 2821 next to the external proxy: want a clash naming both, got %v", err)
	}
	if err := checkNoPortClash(true, proxyReservations([]int{80, 443}, proxy.ModeNginx), a, e); err != nil {
		t.Errorf("nginx mode holds 80 and 443, not 2821: %v", err)
	}
	a, e = replicas(`{"services":{
	  "auth":{"environment":{"PORT":"443","DPORT":"8081"}},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"}}}}`)
	err = checkNoPortClash(true, proxyReservations([]int{80, 443}, proxy.ModeNginx), a, e)
	if err == nil || !strings.Contains(err.Error(), "reverse proxy") || !strings.Contains(err.Error(), "auth (PORT)") || !strings.Contains(err.Error(), "443") {
		t.Errorf("PORT 443 next to the nginx proxy: want a clash naming both, got %v", err)
	}

	// A replica in slot 2 listens on its base port + 100: the message says
	// so, or 2821 would not be a number in the compose file.
	topo, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"2721","DPORT":"8081"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	two, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	err = checkNoPortClash(true, proxyReservations([]int{2821}, proxy.ModeExternal), two)
	if err == nil || !strings.Contains(err.Error(), "auth-2 (PORT)") || !strings.Contains(err.Error(), "auth-2 is slot 2: its ports are auth's in the compose file plus 100") {
		t.Errorf("slot 2 onto the proxy's port: want the slot explained, got %v", err)
	}
	// A reservation against a host-network listener: PORT/DPORT/APORT to
	// check, no `ports:`.
	if err == nil || strings.Contains(err.Error(), "`ports:`") || !strings.Contains(err.Error(), "check PORT/DPORT/APORT in the compose file") {
		t.Errorf("hint for a listener against the proxy: %v", err)
	}
	// A reservation against a wildcard publication on a bridge.
	topo, err = topology.Parse([]byte(`{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[{"target":8081,"published":"2821"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	wild, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	err = checkNoPortClash(false, proxyReservations([]int{2821}, proxy.ModeLocal), wild)
	if err == nil || !strings.Contains(err.Error(), "auth (DPORT) would use 2821 on the host, which Versola's reverse proxy holds") {
		t.Errorf("a wildcard publication of the proxy's port: got %v", err)
	}
}

// A service the compose file puts on the host network is checked as one
// whatever the target, and its slots too; a stray `ports:` on it is not
// what it occupies.
func TestCheckNoPortClashHostNetworkFromCompose(t *testing.T) {
	replicasFor := func(config string, slots []state.Slot) ([]replica, []replica) {
		topo, err := topology.Parse([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		a, err := replicasOf(topo, AuthService, slots)
		if err != nil {
			t.Fatal(err)
		}
		e, err := replicasOf(topo, EdgeService, []state.Slot{{N: 1}})
		if err != nil {
			t.Fatal(err)
		}
		return a, e
	}
	one := []state.Slot{{N: 1}}

	// Two host-network services on 8080 clash even though the target says bridge.
	a, e := replicasFor(`{"services":{
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081"}},
	  "edge":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8096"}}}}`, one)
	if err := checkNoPortClash(false, nil, a, e); err == nil || !strings.Contains(err.Error(), "would both use port 8080") {
		t.Errorf("want a host-network clash on 8080, got %v", err)
	}

	// A stray `ports:` on a host-network service is not what it occupies:
	// auth "publishes" 9999, edge listens on 9999, and that is no clash.
	a, e = replicasFor(`{"services":{
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"9999"}]},
	  "edge":{"network_mode":"host","environment":{"PORT":"8095","DPORT":"9999"}}}}`, one)
	if err := checkNoPortClash(false, nil, a, e); err != nil {
		t.Errorf("a discarded ports: entry must not count: %v", err)
	}

	// Slot 2 of a host-network service stays one (this was once lost in ForSlot).
	a, e = replicasFor(`{"services":{
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081"}},
	  "edge":{"network_mode":"host","environment":{"PORT":"8180","DPORT":"8096"}}}}`, []state.Slot{{N: 1}, {N: 2}})
	if err := checkNoPortClash(false, nil, a, e); err == nil || !strings.Contains(err.Error(), "auth-2 (PORT)") {
		t.Errorf("auth-2 (8180) against edge on 8180: want a clash, got %v", err)
	}
}

// A zone in a link-local host_ip must survive into a URL that parses.
func TestReadinessURLWithZone(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"DPORT":"8081"},"ports":[
	  {"target":8081,"host_ip":"fe80::1%eth0","published":"18081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	u, err := url.Parse(readyURL(t, false, auth[0].Ports))
	if err != nil {
		t.Fatalf("readinessURL is not a URL: %v", err)
	}
	if u.Hostname() != "fe80::1%eth0" || u.Port() != "18081" || u.Path != "/readiness" {
		t.Errorf("got %s", u)
	}
}

func TestDeployedProxyReservations(t *testing.T) {
	if got := deployedProxyReservations(&state.State{Target: "local"}); got != nil {
		t.Errorf("a deployment from before the CLI's proxy holds nothing: %+v", got)
	}
	ports := func(st state.State) string {
		var out []string
		for _, r := range deployedProxyReservations(&st) {
			out = append(out, r.Binding.String())
		}
		return strings.Join(out, ",")
	}
	for name, c := range map[string]struct {
		st   state.State
		want string
	}{
		"local":                  {state.State{Target: "local", ProxyMode: proxy.ModeLocal, AuthURL: "http://localhost:2821"}, "127.0.0.1:2821"},
		"external":               {state.State{Target: "vps", ProxyMode: proxy.ModeExternal, AuthURL: "https://id.example.com"}, "127.0.0.1:2821"},
		"nginx https":            {state.State{Target: "vps", ProxyMode: proxy.ModeNginx, AuthURL: "https://id.example.com"}, "80,443"},
		"nginx http":             {state.State{Target: "vps", ProxyMode: proxy.ModeNginx, AuthURL: "http://id.example.com"}, "80"},
		"nginx, unusable URL":    {state.State{Target: "vps", ProxyMode: proxy.ModeNginx, AuthURL: "not a url"}, "80,443"},
		"external, empty URL":    {state.State{Target: "vps", ProxyMode: proxy.ModeExternal}, "127.0.0.1:2821"},
		"external, unusable URL": {state.State{Target: "vps", ProxyMode: proxy.ModeExternal, AuthURL: "::"}, "127.0.0.1:2821"},
	} {
		if got := ports(c.st); got != c.want {
			t.Errorf("%s: reserved %q, want %q", name, got, c.want)
		}
	}
	// nginx serves 80/443 on every interface, the others on loopback.
	for _, r := range deployedProxyReservations(&state.State{Target: "vps", ProxyMode: proxy.ModeNginx, AuthURL: "https://id.example.com"}) {
		if r.Binding.HostIP != "" {
			t.Errorf("nginx binds every interface, got %+v", r)
		}
	}
	// configure asks the same place.
	auth, err := proxy.ParseAuthURL("https://id.example.com", proxy.ModeNginx)
	if err != nil {
		t.Fatal(err)
	}
	if got := reservationsFor(auth, proxy.ModeNginx); len(got) != 2 {
		t.Errorf("reservationsFor: %+v", got)
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"compose", "-f", `C:\Users\Some One\compose.yml`, "config"})
	if got != `compose -f "C:\Users\Some One\compose.yml" config` {
		t.Errorf("got %s", got)
	}
	if got := shellJoin([]string{"compose", "-f", "/home/me/c.yml"}); got != "compose -f /home/me/c.yml" {
		t.Errorf("plain args stay bare: %s", got)
	}
}

func TestAppUpArgs(t *testing.T) {
	if got := appUpArgs([]string{"auth", "edge"}, false); !reflect.DeepEqual(got, []string{"up", "-d", "auth", "edge"}) {
		t.Errorf("got %v", got)
	}
	if got := appUpArgs([]string{"auth", "auth-2", "edge"}, true); !reflect.DeepEqual(got, []string{"up", "-d", "auth", "auth-2", "edge", "nginx"}) {
		t.Errorf("got %v", got)
	}
	// Must not write through the caller's slice.
	in := make([]string, 2, 10)
	in[0], in[1] = "auth", "edge"
	_ = appUpArgs(in, true)
	if in[:3][2] != "" {
		t.Error("appUpArgs wrote into the caller's backing array")
	}
}

func TestComposeConfigError(t *testing.T) {
	const path = "/home/me/.versola/active/compose.yml"
	exit := func(stderr string) error { return &exec.ExitError{Stderr: []byte(stderr)} }

	old := composeConfigError(path, exit("unknown flag: --format\nSee 'docker compose config --help'.\n"))
	if !strings.Contains(old.Error(), "update the Compose plugin") {
		t.Errorf("an old Compose should be told to update: %v", old)
	}

	down := composeConfigError(path, exit("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"))
	if !strings.Contains(down.Error(), "is it running") {
		t.Errorf("an unreachable daemon should be named: %v", down)
	}

	// Compose's dotenv errors repeat the text they choke on. None of
	// stderr may reach the message, however short or long the line.
	for _, stderr := range []string{
		"failed to read /x/auth.secrets.env: unterminated quoted value \"hunter2\n",
		"line one: " + strings.Repeat("x", 1000) + "\nsecond line password=hunter2\n",
		"hunter2",
	} {
		msg := composeConfigError(path, exit(stderr)).Error()
		if strings.Contains(msg, "hunter2") || strings.Contains(msg, "unterminated") || strings.Contains(msg, "xxxx") {
			t.Errorf("stderr leaked into the message: %q", msg)
		}
		if !strings.Contains(msg, "docker compose -f "+path+" config") {
			t.Errorf("the user must be told how to see the reason themselves: %q", msg)
		}
	}

	plain := composeConfigError(path, errors.New("docker: executable file not found"))
	if !strings.Contains(plain.Error(), "executable file not found") {
		t.Errorf("a non-exit error is passed along: %v", plain)
	}
}

// readyURL is readinessURL for a test that expects an answer.
func readyURL(t *testing.T, hostNetwork bool, r topology.Service) string {
	t.Helper()
	u, err := readinessURL(hostNetwork, r)
	if err != nil {
		t.Fatalf("readinessURL: %v", err)
	}
	return u
}

// A bridge-network service whose diagnostics port is not published cannot
// be asked from the host. Probing the same number there would reach
// whatever else publishes it -- auth's 8081, say, answering for edge.
func TestReadinessOfUnpublishedPortIsAnError(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"8081"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8081"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	st := &state.State{}
	auth, err := replicasOf(topo, AuthService, st.ActiveSlots(AuthService))
	if err != nil {
		t.Fatal(err)
	}
	edge, err := replicasOf(topo, EdgeService, st.ActiveSlots(EdgeService))
	if err != nil {
		t.Fatal(err)
	}
	if got := readyURL(t, false, auth[0].Ports); got != "http://localhost:8081/readiness" {
		t.Errorf("auth publishes it: got %s", got)
	}
	_, err = readinessURL(false, edge[0].Ports)
	if err == nil || !strings.Contains(err.Error(), "8081 is not published") {
		t.Fatalf("edge does not publish it: got %v", err)
	}
	// On the host's network the port is the host's own.
	if got := readyURL(t, true, edge[0].Ports); got != "http://localhost:8081/readiness" {
		t.Errorf("host network: got %s", got)
	}

	// Naming the replica, so the operator knows which to look at.
	_, err = readinessURLs(false, auth, edge)
	if err == nil || !strings.HasPrefix(err.Error(), "edge: ") {
		t.Errorf("readinessURLs: got %v", err)
	}
	urls, err := readinessURLs(true, auth, edge)
	if err != nil || urls["auth"] != urls["edge"] {
		t.Errorf("host network: %v, %v", urls, err)
	}
	// A slot's own published port is found there.
	two, err := replicasOf(topo, AuthService, []state.Slot{{N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readinessURL(false, two[0].Ports); err != nil {
		t.Errorf("slot 2 publishes 8181: %v", err)
	}
}

// OpenBao runs on this machine at 8200 for as long as the stack does.
func TestOpenBaoPortIsReserved(t *testing.T) {
	for name, cfg := range map[string]struct {
		hostNetwork bool
		json        string
	}{
		"vps APORT":   {true, `{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8200"}}}}`},
		"vps PORT":    {true, `{"services":{"auth":{"environment":{"PORT":"8200","DPORT":"8081","APORT":"8082"}}}}`},
		"local ports": {false, `{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"published":"8200"}]}}}`},
		"local addr":  {false, `{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[{"target":8081,"host_ip":"127.0.0.1","published":"8200"}]}}}`},
	} {
		topo, err := topology.Parse([]byte(cfg.json))
		if err != nil {
			t.Fatal(err)
		}
		auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
		if err != nil {
			t.Fatal(err)
		}
		err = checkNoPortClash(cfg.hostNetwork, nil, auth)
		if err == nil || !strings.Contains(err.Error(), "Versola's OpenBao") || !strings.Contains(err.Error(), "8200") {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// The bridge's own 8200 inside the container is not the host's.
	topo, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"8200","DPORT":"8081"},"ports":[{"target":8081,"published":"18081"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNoPortClash(false, nil, auth); err != nil {
		t.Errorf("an unpublished 8200 on a bridge: %v", err)
	}
}

// state.json is a file people edit and old CLIs rewrite: a slot recorded
// twice is reported as that, and slots come out in order.
func TestReplicasOfSlotsFromState(t *testing.T) {
	topo := testTopology(t)
	_, err := replicasOf(topo, AuthService, []state.Slot{{N: 2}, {N: 1}, {N: 2}})
	if err == nil || !strings.Contains(err.Error(), "lists slot 2 twice") {
		t.Errorf("duplicate slots: got %v", err)
	}
	got, err := replicasOf(topo, AuthService, []state.Slot{{N: 3}, {N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if names := serviceNames(got); !reflect.DeepEqual(names, []string{"auth", "auth-2", "auth-3"}) {
		t.Errorf("order: %v", names)
	}
	in := []state.Slot{{N: 3}, {N: 1}}
	if _, err := replicasOf(topo, AuthService, in); err != nil || in[0].N != 3 {
		t.Errorf("the caller's slice is not reordered: %v, %v", in, err)
	}
}

// Only auth binds APORT. An empty one on edge -- `APORT: ${APORT:-}` --
// is nothing; on auth it is a bad port.
func TestAPORTIsAnErrorForAuthOnly(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"APORT":""}},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096","APORT":""}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replicasOf(topo, EdgeService, []state.Slot{{N: 1}, {N: 2}}); err != nil {
		t.Errorf("edge: %v", err)
	}
	if _, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}}); err == nil || !strings.Contains(err.Error(), "APORT") || strings.Contains(err.Error(), "hunter") {
		t.Errorf("auth: got %v", err)
	}
}

func replicasFor(t *testing.T, topo topology.Topology) (central, auth, edge []replica) {
	t.Helper()
	var err error
	if central, err = replicasOf(topo, CentralService, []state.Slot{{N: 1}}); err != nil {
		t.Fatal(err)
	}
	if auth, err = replicasOf(topo, AuthService, []state.Slot{{N: 1}}); err != nil {
		t.Fatal(err)
	}
	if edge, err = replicasOf(topo, EdgeService, []state.Slot{{N: 1}}); err != nil {
		t.Fatal(err)
	}
	return
}

// What up and configure both ask: ports, then readiness URLs, by service.
func TestCheckTopology(t *testing.T) {
	topo := testTopology(t)
	c, a, e := replicasFor(t, topo)
	urls, err := checkTopology(topo, true, nil, c, a, e)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"central": "http://localhost:8091/readiness",
		"auth":    "http://localhost:8081/readiness",
		"edge":    "http://localhost:8096/readiness",
	}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("got %v, want %v", urls, want)
	}
	if _, err := checkTopology(topo, false, nil, c, a, e); err != nil {
		t.Errorf("local publishes every diagnostics port: %v", err)
	}
}

// The rest of the compose file holds host ports too.
func TestCheckTopologyReservesOtherServices(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "postgres":{"ports":[{"target":5432,"host_ip":"127.0.0.1","published":"5432"}]},
	  "gateway":{"ports":[{"target":80,"published":"18090-18100"}]},
	  "host":{"network_mode":"host","ports":[{"target":1,"published":"9"}]},
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"published":"8081"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"19095"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e := replicasFor(t, topo)
	if _, err := checkTopology(topo, false, nil, c, a, e); err != nil {
		t.Fatalf("no clash: %v", err)
	}
	// edge's published port is the gateway's.
	topo2, err := topology.Parse([]byte(`{"services":{
	  "gateway":{"ports":[{"target":80,"published":"18095"}]},
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"published":"8081"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"18095"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e = replicasFor(t, topo2)
	_, err = checkTopology(topo2, false, nil, c, a, e)
	if err == nil || !strings.Contains(err.Error(), `the compose file's "gateway" service`) || !strings.Contains(err.Error(), "18095") {
		t.Errorf("got %v", err)
	}
	// Another service's own listeners are not host ports on a vps.
	topo3, err := topology.Parse([]byte(`{"services":{
	  "mailer":{"network_mode":"host","environment":{"PORT":"8080"}},
	  "central":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091"}},
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"}},
	  "edge":{"network_mode":"host","environment":{"PORT":"8095","DPORT":"8096"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e = replicasFor(t, topo3)
	if _, err := checkTopology(topo3, true, nil, c, a, e); err != nil {
		t.Errorf("vps: %v", err)
	}
}

// A local proxy reaches the replicas by name over Docker's network; a
// service on the host's network is not there.
func TestCheckTopologyRefusesHostNetworkOnLocal(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"}},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"8096"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e := replicasFor(t, topo)
	_, err = checkTopology(topo, false, nil, c, a, e)
	if err == nil || !strings.Contains(err.Error(), "auth has `network_mode: host`") {
		t.Errorf("local: got %v", err)
	}
	if _, err := checkTopology(topo, true, nil, c, a, e); err != nil {
		t.Errorf("vps: %v", err)
	}
}

func TestReadinessOfRangeAndOfSlots(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"host_ip":"127.0.0.1","published":"18081-18090"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 3}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = readinessURLs(false, auth)
	if err == nil || !strings.Contains(err.Error(), "range of host ports") || strings.Contains(err.Error(), "is not published") {
		t.Errorf("a range: got %v", err)
	}
	edge, err := replicasOf(topo, EdgeService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readinessURLs(false, edge); err == nil || !strings.Contains(err.Error(), "one Docker picks") {
		t.Errorf("no host port: got %v", err)
	}
	// For a slot, the base service's entry is what to change.
	_, err = readinessURLs(false, auth[1:])
	if err == nil || !strings.Contains(err.Error(), "auth-3: ") || !strings.Contains(err.Error(), "the fix is in auth's entry (DPORT 8081)") {
		t.Errorf("slot 3: got %v", err)
	}
}

// The same checks on real Compose output (see topology's testdata).
func TestCheckTopologyOnRealComposeOutput(t *testing.T) {
	load := func(name string) topology.Topology {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "topology", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		topo, err := topology.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		return topo
	}
	local := load("compose-local.json")
	c, a, e := replicasFor(t, local)
	urls, err := checkTopology(local, false, nil, c, a, e)
	if err != nil {
		t.Fatalf("local: %v", err)
	}
	want := map[string]string{
		"central": "http://localhost:8091/readiness",
		"auth":    "http://127.0.0.1:18081/readiness",
		"edge":    "http://[::1]:18096/readiness",
	}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("local: got %v, want %v", urls, want)
	}

	vps := load("compose-vps.json")
	c, a, e = replicasFor(t, vps)
	if _, err := checkTopology(vps, true, nil, c, a, e); err != nil {
		t.Errorf("vps: %v", err)
	}
	// The same compose file is no local one: its services are on the host.
	if _, err := checkTopology(vps, false, nil, c, a, e); err == nil {
		t.Error("vps services on a local target: want an error")
	}
}

// Docker takes whichever port of a range is free: a range clashes with
// nothing it contains, nor with another range.
func TestRangesDoNotClash(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "other":{"ports":[{"target":80,"published":"18000-18100"},{"target":81,"published":"8000-8300"}]},
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"published":"18000-18050"},{"target":8082,"published":"18050"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"18020"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e := replicasFor(t, topo)
	_, err = checkTopology(topo, false, nil, c, a, e)
	// Only the readiness check has anything to say: auth's diagnostics port
	// is on a range, with no address to ask.
	if err == nil || !strings.Contains(err.Error(), "auth: ") || !strings.Contains(err.Error(), "range of host ports") {
		t.Errorf("got %v", err)
	}
	if err := checkNoPortClash(false, nil, a, e); err != nil {
		t.Errorf("ranges: %v", err)
	}
}

// A service up never starts holds no port, and what another service's
// environment says does not hide what it publishes.
func TestOtherServicesReservations(t *testing.T) {
	const base = `
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"published":"8081"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"8096"}]}`
	for name, other := range map[string]string{
		"scale 0":          `"old":{"scale":0,"ports":[{"target":80,"published":"8096"}]},`,
		"replicas 0":       `"old":{"deploy":{"replicas":0},"ports":[{"target":80,"published":"8096"}]},`,
		"udp":              `"dns":{"ports":[{"target":53,"published":"8096","protocol":"udp"}]},`,
		"a null service":   `"old":null,`,
		"a string service": `"old":"x",`,
	} {
		topo, err := topology.Parse([]byte(`{"services":{` + other + base + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		c, a, e := replicasFor(t, topo)
		if _, err := checkTopology(topo, false, nil, c, a, e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A bad PORT on a service we only read the publications of.
	topo, err := topology.Parse([]byte(`{"services":{
	  "mailer":{"environment":{"PORT":"${MAILER_PORT:-}"},"ports":[{"target":25,"published":"8096"}]},` + base + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e := replicasFor(t, topo)
	if _, err := checkTopology(topo, false, nil, c, a, e); err == nil || !strings.Contains(err.Error(), `"mailer"`) {
		t.Errorf("mailer holds 8096: got %v", err)
	}
	// Several services holding the same port: the first by name is named.
	topo, err = topology.Parse([]byte(`{"services":{
	  "zeta":{"ports":[{"target":25,"published":"8096"}]},
	  "alpha":{"ports":[{"target":25,"published":"8096"}]},` + base + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e = replicasFor(t, topo)
	for i := 0; i < 20; i++ {
		if _, err := checkTopology(topo, false, nil, c, a, e); err == nil || !strings.Contains(err.Error(), `"alpha"`) {
			t.Fatalf("got %v", err)
		}
	}
}

// A compose file that already declares auth-2 (a replica's own service)
// does not hold its ports against that replica, nor does auth against
// auth-2.
func TestReplicasAreNotOtherServices(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{
	  "central":{"environment":{"PORT":"8090","DPORT":"8091"},"ports":[{"target":8091,"published":"8091"}]},
	  "auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"},"ports":[{"target":8081,"published":"8081"}]},
	  "auth-2":{"environment":{"PORT":"8180","DPORT":"8181","APORT":"8182"},"ports":[{"target":8181,"published":"8181"}]},
	  "edge":{"environment":{"PORT":"8095","DPORT":"8096"},"ports":[{"target":8096,"published":"8096"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	central, err := replicasOf(topo, CentralService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	edge, err := replicasOf(topo, EdgeService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for name, slots := range map[string][]state.Slot{"slots 1 and 2": {{N: 1}, {N: 2}}, "slot 2 only": {{N: 2}}} {
		auth, err := replicasOf(topo, AuthService, slots)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkTopology(topo, false, nil, central, auth, edge); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
