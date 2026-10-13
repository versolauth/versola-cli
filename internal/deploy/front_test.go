package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// The vps compose file of Versola since mutual TLS (#440): auth has MPORT.
const vpsComposeMTLS = `name: versola-test
services:
  central:
    image: central:1
    network_mode: host
    environment: {PORT: "8090", DPORT: "8091"}
  auth:
    image: auth:1
    container_name: versola-auth
    network_mode: host
    environment: {PORT: "8080", DPORT: "8081", APORT: "8082", MPORT: "8083"}
  edge:
    image: edge:1
    network_mode: host
    environment: {PORT: "8095", DPORT: "8096"}
`

func vpsTopology(t *testing.T) topology.Topology {
	t.Helper()
	topo, err := topology.Parse([]byte(`{"services":{
	  "central":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091"}},
	  "auth":{"network_mode":"host","container_name":"versola-auth","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}},
	  "edge":{"network_mode":"host","environment":{"PORT":"8095","DPORT":"8096"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func TestFrontAuth(t *testing.T) {
	topo := vpsTopology(t)
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := frontAuth(auth, true)
	if err != nil {
		t.Fatal(err)
	}
	type ports struct{ p, d, a, m int }
	for i, want := range []ports{{8080, 8081, 9082, 9083}, {8180, 8181, 9182, 9183}} {
		r := got[i]
		if (ports{r.Ports.Port, r.Ports.DiagnosticsPort, r.Ports.AdditionalPort, r.Ports.MutualTLSPort}) != want || !r.Fronted {
			t.Errorf("slot %d: %+v fronted=%v, want %+v", r.Slot, r.Ports, r.Fronted, want)
		}
	}
	if auth[0].Ports.AdditionalPort != 8082 || auth[0].Fronted {
		t.Error("frontAuth changed its argument")
	}
	// Local: every container has its own ports, nothing moves.
	local, err := frontAuth(auth, false)
	if err != nil || !reflect.DeepEqual(local, auth) {
		t.Errorf("local: %+v %v", local, err)
	}
	// A slot whose moved ports do not fit is refused, not wrapped around.
	high, err := topology.Parse([]byte(`{"services":{"auth":{"network_mode":"host","environment":{"APORT":"64600","MPORT":"64601"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := replicasOf(high, AuthService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frontAuth(a, true); err == nil {
		t.Error("APORT 65600 must be refused")
	}
}

// On vps the proxy holds auth's APORT/MPORT as the compose file sets them:
// an auth replica still listening there is a clash, a fronted one is not.
func TestCheckTopologyReservesTheFronts(t *testing.T) {
	topo := vpsTopology(t)
	c, a, e := replicasFor(t, topo)
	_, err := checkTopology(topo, true, nil, c, a, e)
	if err == nil || !strings.Contains(err.Error(), "in front of auth's APORT") {
		t.Errorf("unfronted auth on vps: %v", err)
	}
	if _, err := checkTopology(topo, true, nil, c, fronted(t, a), e); err != nil {
		t.Errorf("fronted: %v", err)
	}
	// Another service on a fronted address clashes with the proxy.
	clash, err := topology.Parse([]byte(`{"services":{
	  "central":{"network_mode":"host","environment":{"PORT":"8083","DPORT":"8091"}},
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}},
	  "edge":{"network_mode":"host","environment":{"PORT":"8095","DPORT":"8096"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, a, e = replicasFor(t, clash)
	if _, err := checkTopology(clash, true, nil, c, fronted(t, a), e); err == nil || !strings.Contains(err.Error(), "MPORT") {
		t.Errorf("central on the MPORT front: %v", err)
	}
}

func TestOverridesYAML(t *testing.T) {
	topo := vpsTopology(t)
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, any, err := overridesYAML(fronted(t, auth))
	if err != nil || !any {
		t.Fatalf("%v %v", any, err)
	}
	want := "services:\n  auth:\n    environment:\n      APORT: \"9082\"\n      MPORT: \"9083\"\n"
	if !strings.HasSuffix(string(b), want) {
		t.Errorf("got\n%s", b)
	}
	if strings.Contains(string(b), "auth-2") {
		t.Error("slots beyond 1 are replicas.yml's")
	}
	// Nothing fronted (local), or no slot 1: nothing to override.
	if _, any, _ := overridesYAML(auth); any {
		t.Error("unfronted: want no file")
	}
	if _, any, _ := overridesYAML(fronted(t, auth)[1:]); any {
		t.Error("no slot 1: want no file")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, state.OverridesFile)
	if err := writeOverridesFile(dir, fronted(t, auth)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("not written: %v", err)
	}
	if err := writeOverridesFile(dir, auth); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a stale overrides.yml is left: %v", err)
	}
}

// Through the real docker compose: overrides.yml moves slot 1, replicas.yml
// gives slot 2 its own, and the compose file without overrides.yml -- what
// the CLI reads the base from -- still says 8082/8083.
func TestFrontedFilesThroughRealCompose(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(compose, []byte(vpsComposeMTLS), 0o644); err != nil {
		t.Fatal(err)
	}
	base := composeTopology(t, compose)
	auth, err := replicasOf(base, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	auth = fronted(t, auth)
	if err := writeOverridesFile(dir, auth); err != nil {
		t.Fatal(err)
	}
	if err := writeReplicasFile(dir, "compose.yml", auth); err != nil {
		t.Fatal(err)
	}
	all := composeTopology(t, compose, filepath.Join(dir, state.OverridesFile), filepath.Join(dir, state.ReplicasFile))
	for svc, want := range map[string][2]int{"auth": {9082, 9083}, "auth-2": {9182, 9183}} {
		s, err := all.Service(svc)
		if err != nil {
			t.Fatal(err)
		}
		if s.AdditionalPort != want[0] || s.MutualTLSPort != want[1] {
			t.Errorf("%s: APORT %d MPORT %d, want %v", svc, s.AdditionalPort, s.MutualTLSPort, want)
		}
	}
	// What loadTopology passes (TopologyComposeArgs) leaves overrides.yml out.
	args := state.TopologyComposeArgs(compose)
	if strings.Contains(strings.Join(args, " "), state.OverridesFile) {
		t.Errorf("topology args include overrides.yml: %v", args)
	}
	again := composeTopology(t, compose, filepath.Join(dir, state.ReplicasFile))
	if a, _ := again.Service("auth"); a.AdditionalPort != 8082 || a.MutualTLSPort != 8083 {
		t.Errorf("base auth read back as %d/%d", a.AdditionalPort, a.MutualTLSPort)
	}
}

func TestProxyBackendsAndRouting(t *testing.T) {
	topo := vpsTopology(t)
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	got := proxyBackends(fronted(t, auth))
	want := []proxy.Backend{{Service: "auth", Port: 8080, AdditionalPort: 9082, MutualTLSPort: 9083}, {Service: "auth-2", Port: 8180, AdditionalPort: 9182, MutualTLSPort: 9183}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	// Unfronted (local): the main port only.
	if b := proxyBackends(auth); b[0].AdditionalPort != 0 || b[0].MutualTLSPort != 0 {
		t.Errorf("local: %+v", b)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	fronts, dir, err := proxyRouting(topo, true)
	if err != nil {
		t.Fatal(err)
	}
	if fronts != (proxy.Fronts{Additional: 8082, MutualTLS: 8083}) || dir != filepath.Join(home, ".versola", "active", "proxy-vps") {
		t.Errorf("vps routing: %+v %s", fronts, dir)
	}
	if fronts, dir, err := proxyRouting(topo, false); err != nil || fronts != (proxy.Fronts{}) || dir != "" {
		t.Errorf("local routing: %+v %q %v", fronts, dir, err)
	}
}

// startProxy, with every docker call replaced: what it runs, in order.
type fakeProxy struct {
	events     []string
	ids        []string // containerID answers, in turn
	running    bool
	applyErr   error
	reloadErr  error
	previous   string
	readyErr   error
	frontsErr  error
	frontsSeen *proxy.Fronts
}

func (f *fakeProxy) install(t *testing.T) {
	old := proxyOps
	t.Cleanup(func() { proxyOps = old })
	proxyOps.containerID = func(name string) (string, bool, error) {
		id := f.ids[0]
		if len(f.ids) > 1 {
			f.ids = f.ids[1:]
		}
		f.events = append(f.events, "inspect "+name)
		return id, id != "" && f.running, nil
	}
	proxyOps.compose = func(_ string, args ...string) error {
		f.events = append(f.events, "compose "+strings.Join(args, " "))
		return nil
	}
	proxyOps.apply = func(_ string, c proxy.Config) (string, error) {
		f.events = append(f.events, "apply")
		return f.previous, f.applyErr
	}
	proxyOps.writeLocal = func(string, proxy.Config) error {
		f.events = append(f.events, "write local")
		return nil
	}
	proxyOps.reload = func(string) error {
		f.events = append(f.events, "reload")
		return f.reloadErr
	}
	proxyOps.rollBack = func(_, previous string) error {
		f.events = append(f.events, "rollback "+previous)
		return nil
	}
	proxyOps.waitReady = func(string, proxy.AuthURL) error {
		f.events = append(f.events, "ready")
		return f.readyErr
	}
	proxyOps.waitFronts = func(fr proxy.Fronts) error {
		f.events = append(f.events, "fronts")
		f.frontsSeen = &fr
		return f.frontsErr
	}
}

func vpsProxyState() *state.State {
	return &state.State{Target: "vps", Version: "0.7.0", ProxyMode: proxy.ModeExternal, AuthURL: "https://id.example.com"}
}

func vpsRouted() proxy.Config {
	return proxy.Config{Mode: proxy.ModeExternal, GenerationsDir: "/home/v/.versola/active/proxy-vps",
		Fronts: proxy.Fronts{Additional: 8082, MutualTLS: 8083}}
}

func TestStartProxyReloadsARunningContainer(t *testing.T) {
	f := &fakeProxy{ids: []string{"abc", "abc"}, running: true, previous: "gen-000003"}
	f.install(t)
	if err := startProxy("/b/compose.yml", vpsProxyState(), vpsRouted()); err != nil {
		t.Fatal(err)
	}
	want := []string{"inspect versola-proxy", "apply", "compose up -d proxy", "inspect versola-proxy", "reload", "ready", "fronts"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events:\n got %v\nwant %v", f.events, want)
	}
	for _, e := range f.events {
		if strings.Contains(e, "force-recreate") {
			t.Error("vps must not force a recreate")
		}
	}
}

// Compose recreated it (its definition changed -- the first move to config
// generations): the new container read `current` itself, no reload.
func TestStartProxyNewContainerIsNotReloaded(t *testing.T) {
	for name, f := range map[string]*fakeProxy{
		"recreated":   {ids: []string{"old", "new"}, running: true},
		"was stopped": {ids: []string{"abc", "abc"}, running: false},
		"none yet":    {ids: []string{"", "new"}, running: true},
	} {
		f.install(t)
		if err := startProxy("/b/compose.yml", vpsProxyState(), vpsRouted()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, e := range f.events {
			if e == "reload" {
				t.Errorf("%s: reloaded: %v", name, f.events)
			}
		}
	}
}

func TestStartProxyRollsBackARefusedReload(t *testing.T) {
	f := &fakeProxy{ids: []string{"abc", "abc"}, running: true, previous: "gen-000003", reloadErr: errors.New("nginx: [emerg] bad")}
	f.install(t)
	err := startProxy("/b/compose.yml", vpsProxyState(), vpsRouted())
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v", err)
	}
	if last := f.events[len(f.events)-1]; last != "rollback gen-000003" {
		t.Errorf("events: %v", f.events)
	}
}

// A config nginx refuses never reaches compose or the running proxy.
func TestStartProxyStopsWhenTheConfigIsRefused(t *testing.T) {
	f := &fakeProxy{ids: []string{"abc"}, running: true, applyErr: errors.New("nginx rejected")}
	f.install(t)
	if err := startProxy("/b/compose.yml", vpsProxyState(), vpsRouted()); err == nil {
		t.Fatal("want an error")
	}
	for _, e := range f.events {
		if strings.HasPrefix(e, "compose") || e == "reload" {
			t.Errorf("went on after the refusal: %v", f.events)
		}
	}
}

func TestStartProxyChecksTheFronts(t *testing.T) {
	f := &fakeProxy{ids: []string{"abc", "abc"}, running: true, frontsErr: errors.New("502")}
	f.install(t)
	err := startProxy("/b/compose.yml", vpsProxyState(), vpsRouted())
	if err == nil || !strings.Contains(err.Error(), "APORT/MPORT") {
		t.Errorf("err = %v", err)
	}
	if f.frontsSeen == nil || *f.frontsSeen != (proxy.Fronts{Additional: 8082, MutualTLS: 8083}) {
		t.Errorf("fronts asked: %v", f.frontsSeen)
	}
}

// local keeps what it did: the bundle's upstreams, a forced recreate, and
// no fronts to check.
func TestStartProxyLocal(t *testing.T) {
	f := &fakeProxy{ids: []string{""}}
	f.install(t)
	st := &state.State{Target: "local", Version: "0.7.0", ProxyMode: proxy.ModeLocal, AuthURL: "http://localhost:2821"}
	if err := startProxy("/b/compose.yml", st, proxy.Config{Mode: proxy.ModeLocal}); err != nil {
		t.Fatal(err)
	}
	want := []string{"write local", "compose up -d --force-recreate proxy", "ready"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events:\n got %v\nwant %v", f.events, want)
	}
}
