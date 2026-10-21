package deploy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// A vps topology of a release whose central serves edges on a mutual-TLS
// listener: its compose file sets MPORT 8093 for central (versola#607).
const centralMTLSTopology = `{"services":{
  "central":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091","MPORT":"8093","MBIND_HOST":"0.0.0.0"}},
  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}},
  "edge":{"network_mode":"host","environment":{"PORT":"8095","DPORT":"8096"}}}}`

func centralMTLSReplicas(t *testing.T, topo topology.Topology, slots ...int) []replica {
	t.Helper()
	var ss []state.Slot
	for _, n := range slots {
		ss = append(ss, state.Slot{N: n})
	}
	rs, err := replicasOf(topo, CentralService, ss)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// central's MPORT moves with PORT and DPORT -- the proxy takes over 8093 --
// and each slot adds the usual stride; a release without the listener has
// nothing there to move.
func TestFrontCentralMovesMutualTLS(t *testing.T) {
	topo := mustParseTopology(t, centralMTLSTopology)
	got := frontedCentral(t, centralMTLSReplicas(t, topo, 1, 2))
	want := map[string][3]int{"central": {28090, 28091, 28093}, "central-2": {28190, 28191, 28193}}
	for _, r := range got {
		if w := want[r.Service]; [3]int{r.Ports.Port, r.Ports.DiagnosticsPort, r.Ports.MutualTLSPort} != w {
			t.Errorf("%s: %d/%d/%d, want %v", r.Service, r.Ports.Port, r.Ports.DiagnosticsPort, r.Ports.MutualTLSPort, w)
		}
		var names []string
		for _, l := range r.listeners() {
			names = append(names, l.Name)
		}
		if !reflect.DeepEqual(names, []string{"PORT", "DPORT", "MPORT"}) {
			t.Errorf("%s listens on %v", r.Service, names)
		}
	}

	// 0.7.x: no MPORT for central in the compose file, nothing is bound there.
	for _, r := range frontedCentral(t, centralMTLSReplicas(t, vpsTopology(t), 1)) {
		for _, l := range r.listeners() {
			if l.Name == "MPORT" {
				t.Errorf("%s: MPORT %d listed, but its compose file sets none", r.Service, l.Port)
			}
		}
		if len(r.extraEnv()) != 0 {
			t.Errorf("%s: %v", r.Service, r.extraEnv())
		}
	}

	// A shifted port that does not fit is named.
	high := mustParseTopology(t, `{"services":{"central":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091","MPORT":"50000"}}}}`)
	if _, err := frontCentral(centralMTLSReplicas(t, high, 1), true); err == nil || !strings.Contains(err.Error(), "MPORT") {
		t.Errorf("MPORT 50000 + 20000: %v", err)
	}
	// Local: nothing moves.
	local := centralMTLSReplicas(t, topo, 1)
	if same, err := frontCentral(local, false); err != nil || !reflect.DeepEqual(same, local) {
		t.Errorf("local: %+v %v", same, err)
	}
}

func TestProxyFrontsIncludeCentralMutualTLS(t *testing.T) {
	f, err := proxyFronts(mustParseTopology(t, centralMTLSTopology))
	if err != nil {
		t.Fatal(err)
	}
	if f.CentralMutualTLS != 8093 || f.Central != 8090 {
		t.Errorf("%+v", f)
	}
	var found bool
	for _, r := range frontReservations(f) {
		// Every interface: edges on other hosts use it.
		if r.Binding.Port == 8093 && r.Binding.HostIP == "" && strings.Contains(r.Name, "central's MPORT") {
			found = true
		}
	}
	if !found {
		t.Errorf("no reservation of 8093: %+v", frontReservations(f))
	}
	old, err := proxyFronts(vpsTopology(t))
	if err != nil {
		t.Fatal(err)
	}
	if old.CentralMutualTLS != 0 {
		t.Errorf("a central without MPORT has no front for it: %+v", old)
	}
	for _, r := range frontReservations(old) {
		if r.Binding.Port == 8093 {
			t.Errorf("8093 reserved without a listener: %+v", r)
		}
	}
}

// overrides.yml moves central's slot 1 (MPORT, and MBIND_HOST onto the
// loopback: the compose file binds it on every interface); replicas.yml
// gives slot 2 its own. A central without the listener gets neither.
func TestOverridesAndReplicasCarryCentralMutualTLS(t *testing.T) {
	central := frontedCentral(t, centralMTLSReplicas(t, mustParseTopology(t, centralMTLSTopology), 1, 2))
	b, any, err := overridesYAML(central, nil)
	if err != nil || !any {
		t.Fatalf("%v %v", any, err)
	}
	want := "services:\n  central:\n    environment:\n      PORT: \"28090\"\n      DPORT: \"28091\"\n      MPORT: \"28093\"\n      MBIND_HOST: \"127.0.0.1\"\n"
	if !strings.HasSuffix(string(b), want) {
		t.Errorf("overrides.yml:\n%s", b)
	}
	r, any, err := replicasYAML("compose.yml", central)
	if err != nil || !any {
		t.Fatalf("%v %v", any, err)
	}
	for _, w := range []string{"  central-2:\n", `PORT: "28190"`, `DPORT: "28191"`, `MPORT: "28193"`, `MBIND_HOST: "127.0.0.1"`} {
		if !strings.Contains(string(r), w) {
			t.Errorf("replicas.yml lacks %q:\n%s", w, r)
		}
	}

	oldCentral := frontedCentral(t, centralMTLSReplicas(t, vpsTopology(t), 1, 2))
	for name, f := range map[string]func() ([]byte, bool, error){
		"overrides": func() ([]byte, bool, error) { return overridesYAML(oldCentral, nil) },
		"replicas":  func() ([]byte, bool, error) { return replicasYAML("compose.yml", oldCentral) },
	} {
		b, _, err := f()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "      MPORT:") || strings.Contains(string(b), "MBIND_HOST") {
			t.Errorf("%s of a central without MPORT:\n%s", name, b)
		}
	}
}

// The replicas the rollout starts next to the running central, and the
// preflight's view of them: the stand-in's mutual-TLS port is its own, never
// the proxy's 8093 nor the running replica's.
func TestStandInCentralTakesItsOwnMutualTLSPort(t *testing.T) {
	topo := mustParseTopology(t, centralMTLSTopology)
	r, err := standInReplica(topo, CentralService, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostPortsOf(r, true); !reflect.DeepEqual(got, []int{28190, 28191, 28193}) {
		t.Errorf("the stand-in takes %v", got)
	}
	// Without the listener in the compose file the stand-in takes no more
	// than before.
	r, err = standInReplica(vpsTopology(t), CentralService, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostPortsOf(r, true); !reflect.DeepEqual(got, []int{28190, 28191}) {
		t.Errorf("a central without MPORT: %v", got)
	}
}

// 8093 is the proxy's: another service on it, or an unfronted central slot
// 1 (which holds it itself), clash.
func TestCheckTopologyReservesCentralMutualTLSFront(t *testing.T) {
	topo := mustParseTopology(t, centralMTLSTopology)
	c, a, e := replicasFor(t, topo)
	if _, err := checkTopology(topo, true, nil, frontedCentral(t, c), fronted(t, a), e); err != nil {
		t.Errorf("fronted: %v", err)
	}
	_, err := checkTopology(topo, true, nil, c, fronted(t, a), e)
	if err == nil {
		t.Error("an unfronted central on 8090/8093 must clash with the proxy's fronts")
	}
	clash := mustParseTopology(t, strings.Replace(centralMTLSTopology, `"PORT":"8095"`, `"PORT":"8093"`, 1))
	c, a, e = replicasFor(t, clash)
	if _, err := checkTopology(clash, true, nil, frontedCentral(t, c), fronted(t, a), e); err == nil || !strings.Contains(err.Error(), "in front of central's MPORT") {
		t.Errorf("edge on 8093: %v", err)
	}
	// Two fronted slots do not clash.
	two := frontedCentral(t, centralMTLSReplicas(t, topo, 1, 2))
	_, a, e = replicasFor(t, topo)
	if _, err := checkTopology(topo, true, nil, two, fronted(t, a), e); err != nil {
		t.Errorf("two slots: %v", err)
	}
}

func TestRoutedConfigCarriesCentralMutualTLS(t *testing.T) {
	topo := mustParseTopology(t, centralMTLSTopology)
	fr, err := proxyFronts(topo)
	if err != nil {
		t.Fatal(err)
	}
	c, a, e := replicasFor(t, topo)
	cfg := routedConfig(proxy.ModeExternal, fr, "/g", frontedCentral(t, c), fronted(t, a), e)
	if want := []proxy.Backend{{Service: "central", Port: 28090, MutualTLSPort: 28093}}; !reflect.DeepEqual(cfg.Central, want) {
		t.Errorf("%+v, want %+v", cfg.Central, want)
	}
	if cfg.Fronts.CentralMutualTLS != 8093 {
		t.Errorf("%+v", cfg.Fronts)
	}
	// And the proxy's files are valid for it.
	if _, err := cfg.DynamicFiles(); err != nil {
		t.Error(err)
	}

	// 0.7.x: backends carry no MPORT and the proxy has no front for it.
	old := vpsTopology(t)
	ofr, err := proxyFronts(old)
	if err != nil {
		t.Fatal(err)
	}
	c, a, e = replicasFor(t, old)
	cfg = routedConfig(proxy.ModeExternal, ofr, "/g", frontedCentral(t, c), fronted(t, a), e)
	if want := []proxy.Backend{{Service: "central", Port: 28090}}; !reflect.DeepEqual(cfg.Central, want) {
		t.Errorf("%+v, want %+v", cfg.Central, want)
	}
	files, err := cfg.DynamicFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[proxy.CentralMutualTLSFile]; ok {
		t.Error("a front of central's MPORT for a central without one")
	}
}

// A rolling upgrade may bring a central that has the listener (or lose it);
// moving the listener both have is refused like any other moved port.
func TestSamePortsCentralMutualTLS(t *testing.T) {
	without, with := mustParseTopology(t, strings.Replace(centralMTLSTopology, `,"MPORT":"8093","MBIND_HOST":"0.0.0.0"`, "", 1)), mustParseTopology(t, centralMTLSTopology)
	slots := map[string][]state.Slot{AuthService: old(1), EdgeService: old(1)}
	for name, pair := range map[string][2]topology.Topology{"gained": {without, with}, "lost": {with, without}, "kept": {with, with}} {
		if err := samePorts(pair[0], pair[1], slots); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	moved := mustParseTopology(t, strings.Replace(centralMTLSTopology, `"MPORT":"8093"`, `"MPORT":"9093"`, 1))
	if err := samePorts(with, moved, slots); err == nil || !strings.Contains(err.Error(), "moves central's ports") {
		t.Errorf("moved MPORT: %v", err)
	}
}
