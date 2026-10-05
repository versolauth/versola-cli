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
	if err := checkNoPortClash(central, auth, edge); err != nil {
		t.Errorf("Versola's own ports are distinct: %v", err)
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
	err = checkNoPortClash(a, e)
	if err == nil || !strings.Contains(err.Error(), "8080") {
		t.Errorf("want a clash on 8080, got %v", err)
	}

	// Several slots of one service are distinct by construction.
	many, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}, {N: 9}})
	if err := checkNoPortClash(many, edge); err != nil {
		t.Errorf("slots of auth must not clash with each other or edge: %v", err)
	}

	same, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8080"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := replicasOf(same, AuthService, one)
	if err := checkNoPortClash(s); err == nil {
		t.Error("PORT == DPORT: want error")
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
	old := composeConfigError(&exec.ExitError{Stderr: []byte("unknown flag: --format\nSee 'docker compose config --help'.\n")})
	if !strings.Contains(old.Error(), "update the Compose plugin") {
		t.Errorf("an old Compose should be told to update: %v", old)
	}

	long := composeConfigError(&exec.ExitError{Stderr: []byte("line one: " + strings.Repeat("x", 1000) + "\nsecond line password=hunter2\n")})
	if strings.Contains(long.Error(), "hunter2") || len(long.Error()) > 600 {
		t.Errorf("only a short first line of stderr may be quoted, got %d chars", len(long.Error()))
	}

	plain := composeConfigError(errors.New("docker: executable file not found"))
	if !strings.Contains(plain.Error(), "executable file not found") {
		t.Errorf("a non-exit error is passed along: %v", plain)
	}
}

func TestFirstLine(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"", 10, ""},
		{"\n\n  \n", 10, ""},
		{"\n  first \nsecond", 10, "first"},
		{"abcdefghij", 4, "abcd..."},
		{"приветмир", 6, "привет..."},
	} {
		if got := firstLine(c.in, c.max); got != c.want {
			t.Errorf("firstLine(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
