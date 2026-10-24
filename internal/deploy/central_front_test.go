package deploy

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

func TestFrontCentral(t *testing.T) {
	topo := vpsTopology(t)
	central, err := replicasOf(topo, CentralService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := frontCentral(central, true)
	if err != nil {
		t.Fatal(err)
	}
	type ports struct{ p, d int }
	want := map[string]ports{"central": {28090, 28091}, "central-2": {28190, 28191}}
	for _, r := range got {
		if !r.Fronted {
			t.Errorf("%s: not marked fronted", r.Service)
		}
		if w := want[r.Service]; (ports{r.Ports.Port, r.Ports.DiagnosticsPort}) != w {
			t.Errorf("%s: %d/%d, want %v", r.Service, r.Ports.Port, r.Ports.DiagnosticsPort, w)
		}
	}
	// The input is not changed, and the base keeps the compose file's ports.
	if central[0].Ports.Port != 8090 || central[0].Fronted {
		t.Errorf("input modified: %+v", central[0])
	}
	// Local: every container has its own ports, nothing moves.
	same, err := frontCentral(central, false)
	if err != nil || !reflect.DeepEqual(same, central) {
		t.Errorf("local: %+v %v", same, err)
	}
	// Out of range names the problem.
	high := mustParseTopology(t, `{"services":{"central":{"network_mode":"host","environment":{"PORT":"50000","DPORT":"50001"}}}}`)
	rs, err := replicasOf(high, CentralService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frontCentral(rs, true); err == nil {
		t.Error("PORT 50000 + 20000 must be refused")
	}
}

func mustParseTopology(t *testing.T, s string) topology.Topology {
	t.Helper()
	topo, err := topology.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func TestProxyFrontsIncludeCentral(t *testing.T) {
	f, err := proxyFronts(vpsTopology(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Central != 8090 || f.Additional != 8082 || f.MutualTLS != 8083 {
		t.Errorf("%+v", f)
	}
	res := frontReservations(f)
	var found bool
	for _, r := range res {
		if r.Binding.Port == 8090 && r.Binding.HostIP == "127.0.0.1" && strings.Contains(r.Name, "central") {
			found = true
		}
	}
	if !found {
		t.Errorf("no reservation of 127.0.0.1:8090: %+v", res)
	}
}

// The proxy holds 8090: an unfronted central there is a clash, a fronted one
// is not -- and neither is anything else on that port.
func TestCheckTopologyReservesCentralFront(t *testing.T) {
	topo := vpsTopology(t)
	c, a, e := replicasFor(t, topo)
	_, err := checkTopology(topo, true, nil, c, fronted(t, a), e)
	if err == nil || !strings.Contains(err.Error(), "in front of central's PORT") {
		t.Errorf("unfronted central on vps: %v", err)
	}
	if _, err := checkTopology(topo, true, nil, frontedCentral(t, c), fronted(t, a), e); err != nil {
		t.Errorf("fronted: %v", err)
	}
	// Edge on 28090 would clash with the fronted central.
	clash := mustParseTopology(t, `{"services":{
	  "central":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091"}},
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}},
	  "edge":{"network_mode":"host","environment":{"PORT":"28090","DPORT":"8096"}}}}`)
	c, a, e = replicasFor(t, clash)
	if _, err := checkTopology(clash, true, nil, frontedCentral(t, c), fronted(t, a), e); err == nil {
		t.Error("edge on central's moved port: want a clash")
	}
}

func TestOverridesYAMLMovesCentral(t *testing.T) {
	topo := vpsTopology(t)
	central, err := replicasOf(topo, CentralService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, any, err := overridesYAML(frontedCentral(t, central), nil)
	if err != nil || !any {
		t.Fatalf("%v %v", any, err)
	}
	want := "services:\n  central:\n    environment:\n      PORT: \"28090\"\n      DPORT: \"28091\"\n"
	if !strings.HasSuffix(string(b), want) {
		t.Errorf("got\n%s", b)
	}
	if strings.Contains(string(b), "central-2") {
		t.Error("slots beyond 1 are replicas.yml's")
	}
	if _, any, _ := overridesYAML(central, nil); any {
		t.Error("unfronted central: want no file")
	}
	// Both services in one file.
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err = overridesYAML(frontedCentral(t, central), fronted(t, auth))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"  auth:", "  central:", `PORT: "28090"`, `APORT: "9082"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("lacks %q:\n%s", w, b)
		}
	}
}

// Through the real docker compose when it is there: overrides.yml moves
// central's slot 1 and replicas.yml gives slot 2 its own, while the compose
// file alone -- what the CLI reads the base from -- still says 8090/8091.
func TestCentralFrontedFilesThroughRealCompose(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(compose, []byte(vpsComposeMTLS), 0o644); err != nil {
		t.Fatal(err)
	}
	base := composeTopology(t, compose)
	central, err := replicasOf(base, CentralService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	central = frontedCentral(t, central)
	if err := writeOverridesFile(dir, central, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReplicasFile(dir, "compose.yml", central); err != nil {
		t.Fatal(err)
	}
	all := composeTopology(t, compose, filepath.Join(dir, state.OverridesFile), filepath.Join(dir, state.ReplicasFile))
	for svc, want := range map[string][2]int{"central": {28090, 28091}, "central-2": {28190, 28191}} {
		s, err := all.Service(svc)
		if err != nil {
			t.Fatal(err)
		}
		if s.Port != want[0] || s.DiagnosticsPort != want[1] {
			t.Errorf("%s: PORT %d DPORT %d, want %v", svc, s.Port, s.DiagnosticsPort, want)
		}
	}
	b, err := base.Service(CentralService)
	if err != nil {
		t.Fatal(err)
	}
	if b.Port != 8090 {
		t.Errorf("the base moved: %d", b.Port)
	}
}

func TestRoutedConfigCarriesCentral(t *testing.T) {
	topo := vpsTopology(t)
	c, a, e := replicasFor(t, topo)
	fr, err := proxyFronts(topo)
	if err != nil {
		t.Fatal(err)
	}
	cfg := routedConfig(proxy.ModeExternal, fr, "/g", frontedCentral(t, c), fronted(t, a), e)
	if !reflect.DeepEqual(cfg.Central, []proxy.Backend{{Service: "central", Port: 28090}}) || cfg.Fronts.Central != 8090 {
		t.Errorf("%+v", cfg)
	}
	// No central front, no central backends (local).
	local := routedConfig(proxy.ModeLocal, proxy.Fronts{}, "", c, a, e)
	if len(local.Central) != 0 {
		t.Errorf("local: %+v", local.Central)
	}
}

// startProxyForCentral starts the proxy and waits for central's front alone;
// waitProxy then checks the rest once auth and edge are up.
func TestStartProxyForCentralThenWait(t *testing.T) {
	f := &fakeProxy{ids: []string{"abc", "abc"}, running: true, previous: "gen-000003"}
	f.install(t)
	var central []int
	proxyOps.waitCentral = func(port int) error {
		f.events = append(f.events, "central front")
		central = append(central, port)
		return f.frontsErr
	}
	routed := vpsRouted()
	routed.Fronts.Central = 8090
	if err := startProxyForCentral("/b/compose.yml", vpsProxyState(), routed); err != nil {
		t.Fatal(err)
	}
	want := []string{"inspect versola-proxy", "apply", "compose up -d proxy", "inspect versola-proxy", "reload", "central front"}
	if !reflect.DeepEqual(f.events, want) || !reflect.DeepEqual(central, []int{8090}) {
		t.Errorf("events:\n got %v\nwant %v (%v)", f.events, want, central)
	}
	f.events = nil
	if err := waitProxy(vpsProxyState(), routed); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ready", "fronts"}; !reflect.DeepEqual(f.events, want) {
		t.Errorf("waitProxy: got %v, want %v", f.events, want)
	}
	if f.frontsSeen == nil || f.frontsSeen.Central != 8090 {
		t.Errorf("fronts waited for: %+v", f.frontsSeen)
	}
}

func TestStartProxyForCentralFailures(t *testing.T) {
	// The front never serves: the error names it.
	f := &fakeProxy{ids: []string{"abc", "abc"}, running: true}
	f.install(t)
	proxyOps.waitCentral = func(int) error { return errors.New("502") }
	routed := vpsRouted()
	routed.Fronts.Central = 8090
	err := startProxyForCentral("/b/compose.yml", vpsProxyState(), routed)
	if err == nil || !strings.Contains(err.Error(), "front of central's PORT") {
		t.Errorf("got %v", err)
	}
	// A refused reload stops before waiting for anything.
	f = &fakeProxy{ids: []string{"abc", "abc"}, running: true, reloadErr: errors.New("nginx: [emerg] bad")}
	f.install(t)
	called := false
	proxyOps.waitCentral = func(int) error { called = true; return nil }
	if err := startProxyForCentral("/b/compose.yml", vpsProxyState(), routed); err == nil || called {
		t.Errorf("refused reload: err=%v waited=%v", err, called)
	}
}

// A deployment from before central was fronted is moved by the rollout after
// the old central stops: a foreign listener on its new ports is found first.
func TestCentralMovePreflight(t *testing.T) {
	topo := vpsTopology(t)
	st, bundle := guardBundle(t, true)
	// Already moved: nothing to check, whatever listens there (it is central).
	ln, err := net.Listen("tcp", "127.0.0.1:28091")
	if err != nil {
		t.Skipf("port 28091 is taken on this machine: %v", err)
	}
	defer ln.Close()
	if err := centralMovePreflight(st, topo, bundle); err != nil {
		t.Errorf("central already moved: %v", err)
	}
	// Not moved yet, 28091 taken: refused, naming the port.
	if err := os.Remove(filepath.Join(bundle, state.OverridesFile)); err != nil {
		t.Fatal(err)
	}
	err = centralMovePreflight(st, topo, bundle)
	if err == nil || !strings.Contains(err.Error(), "28091") {
		t.Errorf("port taken: %v", err)
	}
	// Not moved yet, ports free: fine.
	ln.Close()
	if err := centralMovePreflight(st, topo, bundle); err != nil {
		t.Errorf("ports free: %v", err)
	}
}
