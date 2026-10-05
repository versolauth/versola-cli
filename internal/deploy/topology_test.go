package deploy

import (
	"errors"
	"os/exec"
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
		if got := readinessURL(c.r.Ports); got != c.url {
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
	if got := readinessURL(auth[1].Ports); got != "http://localhost:8281/readiness" {
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
	if got := readinessURL(auth[0].Ports); got != "http://localhost:18081/readiness" {
		t.Errorf("slot 1: got %s", got)
	}
	if got := readinessURL(auth[1].Ports); got != "http://localhost:18181/readiness" {
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
		if err := checkNoPortClash(host, central, auth, edge); err != nil {
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
	err = checkNoPortClash(true, a, e)
	if err == nil || !strings.Contains(err.Error(), "auth (PORT)") || !strings.Contains(err.Error(), "edge (PORT)") || !strings.Contains(err.Error(), "8080") {
		t.Errorf("want auth (PORT) and edge (PORT) clashing on 8080, got %v", err)
	}

	// Several slots of one service are distinct by construction.
	many, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}, {N: 9}})
	for _, host := range []bool{true, false} {
		if err := checkNoPortClash(host, many, edge); err != nil {
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
		if err := checkNoPortClash(host, s); err == nil {
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
	err = checkNoPortClash(true, central, auth)
	if err == nil || !strings.Contains(err.Error(), "central (PORT)") || !strings.Contains(err.Error(), "auth (APORT)") || !strings.Contains(err.Error(), "8090") {
		t.Errorf("auth's APORT against central's PORT: want a clash on 8090, got %v", err)
	}
	if strings.Contains(err.Error(), "`ports:`") {
		t.Errorf("on the host network there is no ports: to check: %v", err)
	}
	// ... but not on a bridge, where nothing is published and the two
	// containers have their own network.
	if err := checkNoPortClash(false, central, auth); err != nil {
		t.Errorf("bridge, nothing published: %v", err)
	}

	// APORT equal to auth's own PORT is one process binding twice, on any network.
	own, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081","APORT":"8080"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := replicasOf(own, AuthService, one)
	for _, host := range []bool{true, false} {
		if err := checkNoPortClash(host, o); err == nil {
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
	if err := checkNoPortClash(true, c2, a2); err == nil || !strings.Contains(err.Error(), "8182") {
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
	if err := checkNoPortClash(false, auth, edge); err != nil {
		t.Errorf("same container ports behind different published host ports are fine on a bridge: %v", err)
	}
	// The same layout on the host network really would collide.
	if err := checkNoPortClash(true, auth, edge); err == nil {
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
	err = checkNoPortClash(false, a, e)
	if err == nil || !strings.Contains(err.Error(), "auth (DPORT)") || !strings.Contains(err.Error(), "edge (DPORT)") || !strings.Contains(err.Error(), "8081") {
		t.Errorf("same published host port: want auth (DPORT) and edge (DPORT) clashing on 8081, got %v", err)
	}
	if !strings.Contains(err.Error(), "`ports:`") {
		t.Errorf("on a bridge the hint should mention ports: %v", err)
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
	err = checkNoPortClash(false, central, auth)
	if err == nil || !strings.Contains(err.Error(), "auth-2 (DPORT)") || !strings.Contains(err.Error(), "8181") {
		t.Errorf("want auth-2's published DPORT (8181) to clash with central, got %v", err)
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
