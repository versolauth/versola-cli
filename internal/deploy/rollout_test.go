package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// rolloutCompose is the new version's compose configuration, as on vps.
const rolloutCompose = `{"name":"versola-vps","services":{
  "central":{"image":"ghcr.io/versolauth/central:0.7.0","network_mode":"host","environment":{"PORT":"8090","DPORT":"8091"}},
  "auth":{"image":"ghcr.io/versolauth/auth:0.7.0","network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}},
  "edge":{"image":"ghcr.io/versolauth/edge:0.7.0","network_mode":"host","environment":{"PORT":"8095","DPORT":"8096"}}}}`

const (
	fromV = "0.6.2"
	toV   = "0.7.0"
)

func imageOf(base, version string) string {
	return "ghcr.io/versolauth/" + base + ":" + version
}

// errCrash is the fake's way of cutting a run off, as a kill would.
var errCrash = errors.New("crash")

type fakeContainer struct {
	image          string
	workDir        string
	running, ready bool
}

const (
	oldBundleDir = "/home/v/.versola/active/bundle-old"
	newBundleDir = "/home/v/.versola/active/bundle-new"
)

// world is a stateful fake of everything a rollout touches: containers,
// the proxy's upstreams, replicas.yml and state.json (kept as JSON, so the
// engine's own copy is never shared with it). It checks, on every reload,
// that the proxy only routes to ready replicas and -- with a stand-in --
// never to fewer than the operator's count.
type world struct {
	t            *testing.T
	topo         topology.Topology
	stateJSON    []byte
	containers   map[string]*fakeContainer
	upstream     map[string][]string // auth/edge -> compose services routed to
	central      []proxy.Backend     // what the last routing sent to central's front
	centralFront int                 // the front the last routing carried
	replicas     []string            // services in replicas.yml
	events       []string
	callNames    []string          // the event of each counted call
	urls         map[string]string // readiness URL -> service

	minUp        map[string]int // with a stand-in: never fewer in the upstream
	never        map[string]bool
	waits        map[string]time.Duration // the last limit a service was waited for with
	coldOnce     map[string]bool          // fails warm once
	calls        int
	crashAt      int
	crashAfter   bool // the crashing call takes effect first
	crashed      bool
	failOnce     map[string]error // a call that fails once, without a crash
	violations   []string
	startedFrom  bool
	ranFromNew   bool
	proxyRunning bool
	// targetV and targetDir are what `compose up` creates containers of
	// and from: the new version and bundle, or an abort's way back.
	targetV, targetDir string
	restored           string          // the bundle restorePrevious made active
	oneOffs            []string        // running `compose run` containers
	memShort           map[string]bool // standInMemory refuses for these services
	memChecked         []string
	// centralStandIn: the proxy fronts central (CurrentFrontsCentral), so a
	// rollout with surge moves central through a stand-in.
	centralStandIn bool
}

func newWorld(t *testing.T, st *state.State) *world {
	t.Helper()
	topo, err := topology.Parse([]byte(rolloutCompose))
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, topo: topo, containers: map[string]*fakeContainer{}, upstream: map[string][]string{},
		urls: map[string]string{}, failOnce: map[string]error{}, minUp: map[string]int{}, never: map[string]bool{}, coldOnce: map[string]bool{}, proxyRunning: true,
		targetV: toV, targetDir: newBundleDir}
	for _, base := range []string{CentralService, AuthService, EdgeService} {
		for n := 1; n <= topology.MaxSlot; n++ {
			r, err := standInReplica(topo, base, n, true)
			if err != nil {
				t.Fatal(err)
			}
			u, err := readinessURL(true, r.Ports)
			if err != nil {
				t.Fatal(err)
			}
			w.urls[u] = r.Service
		}
	}
	for _, svc := range rolloutServices {
		var names []string
		for _, sl := range st.ActiveSlots(svc) {
			name := topology.SlotService(svc, sl.N)
			w.containers[name] = &fakeContainer{image: imageOf(svc, sl.Version), workDir: oldBundleDir, running: true, ready: true}
			names = append(names, name)
			if sl.N > 1 {
				w.replicas = append(w.replicas, name)
			}
		}
		w.upstream[svc] = names
		w.minUp[svc] = len(names)
	}
	w.persist(st)
	return w
}

func (w *world) persist(st *state.State) {
	b, err := json.Marshal(st)
	if err != nil {
		w.t.Fatal(err)
	}
	w.stateJSON = b
}

func (w *world) load() *state.State {
	var s state.State
	if err := json.Unmarshal(w.stateJSON, &s); err != nil {
		w.t.Fatal(err)
	}
	return &s
}

// step counts one call that can fail; it returns errCrash at crashAt (and
// whether the call's effect applies before it does).
func (w *world) step(ev string, effect func() error) error {
	if w.crashed {
		return errCrash
	}
	w.events = append(w.events, ev)
	w.callNames = append(w.callNames, ev)
	w.calls++
	if w.crashAt > 0 && w.calls == w.crashAt {
		w.crashed = true
		if w.crashAfter && effect != nil {
			_ = effect()
		}
		return errCrash
	}
	var err error
	if effect != nil {
		err = effect()
	}
	// A failure after the call took effect (nginx reloaded, then its check
	// failed): the effect stays.
	if f, ok := w.failOnce[ev]; ok {
		delete(w.failOnce, ev)
		return f
	}
	return err
}

func baseOf(service string) string {
	base, _, _ := strings.Cut(service, "-")
	return base
}

func (w *world) isRunning(string) (bool, error) { return w.proxyRunning, nil }

func (w *world) compose(args ...string) error {
	return w.step("compose "+strings.Join(args, " "), func() error {
		if len(args) != 4 || args[0] != "up" || args[1] != "-d" || args[2] != "--no-deps" {
			return fmt.Errorf("unexpected compose call %v", args)
		}
		svc := args[3]
		defined := svc == CentralService || svc == AuthService || svc == EdgeService
		for _, r := range w.replicas {
			defined = defined || r == svc
		}
		if !defined {
			return fmt.Errorf("no such service: %s", svc)
		}
		want := imageOf(baseOf(svc), w.targetV)
		c := w.containers[svc]
		if c != nil && c.image == want && c.workDir == w.targetDir && c.running {
			return nil // same definition, running: compose leaves it
		}
		w.containers[svc] = &fakeContainer{image: want, workDir: w.targetDir, running: true, ready: !w.never[svc]}
		return nil
	})
}

func (w *world) waitReady(u string) error { return w.waitReadyWithin(u, readyTimeout) }

// waitReadyWithin records the limit each service was waited for with.
func (w *world) waitReadyWithin(u string, limit time.Duration) error {
	svc := w.urls[u]
	if w.waits == nil {
		w.waits = map[string]time.Duration{}
	}
	w.waits[svc] = limit
	return w.step("ready "+svc, func() error {
		if c := w.containers[svc]; c != nil && c.running && c.ready {
			return nil
		}
		return fmt.Errorf("%s not ready", svc)
	})
}

func (w *world) urlOf(svc string) string {
	for u, s := range w.urls {
		if s == svc {
			return u
		}
	}
	return ""
}

func (w *world) readyNow(u string) bool {
	c := w.containers[w.urls[u]]
	return c != nil && c.running && c.ready
}

func (w *world) warm(r replica) error {
	return w.step("warm "+r.Service, func() error {
		if w.coldOnce[r.Service] {
			delete(w.coldOnce, r.Service)
			return errors.New("cold")
		}
		return nil
	})
}

func (w *world) applyUpstreams(c proxy.Config) error {
	next := map[string][]string{}
	for svc, bs := range map[string][]proxy.Backend{CentralService: c.Central, AuthService: c.Auth, EdgeService: c.Edge} {
		ready := 0
		for _, b := range bs {
			next[svc] = append(next[svc], b.Service)
			if ct := w.containers[b.Service]; ct != nil && ct.running && ct.ready {
				ready++
			} else {
				w.violations = append(w.violations, fmt.Sprintf("routed to %s, which is not ready", b.Service))
			}
		}
		if ready < w.minUp[svc] {
			w.violations = append(w.violations, fmt.Sprintf("%s: %d ready in the upstream, fewer than %d", svc, ready, w.minUp[svc]))
		}
		if svc == AuthService {
			for _, b := range bs {
				if b.AdditionalPort == 0 || b.MutualTLSPort == 0 {
					w.violations = append(w.violations, b.Service+" routed without its APORT/MPORT")
				}
			}
		}
	}
	w.upstream = next
	w.central, w.centralFront = c.Central, c.Fronts.Central
	return nil
}

func (w *world) reload(c proxy.Config) error {
	return w.step(fmt.Sprintf("reload auth=%s edge=%s", strings.Join(names(c.Auth), ","), strings.Join(names(c.Edge), ",")), func() error {
		return w.applyUpstreams(c)
	})
}

func names(bs []proxy.Backend) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Service)
	}
	return out
}

func (w *world) waitFronts(proxy.Fronts) error { return w.step("fronts", nil) }
func (w *world) writeUpstreams(c proxy.Config) error {
	return w.step("stage", func() error { return w.applyUpstreams(c) })
}
func (w *world) waitDrained()        { w.events = append(w.events, "drained") }
func (w *world) portFree(int) error  { return nil }
func (w *world) markRunning() error  { return w.step("markRunning", nil) }
func (w *world) pull() error         { return w.step("pull", nil) }
func (w *world) migrate() error      { return w.step("migrate", nil) }
func (w *world) markStarting() error { return w.step("markStarting", nil) }

func (w *world) save(service string, slots []state.Slot) error {
	w.t.Errorf("the rollout must record through update, not save")
	return nil
}

func (w *world) writeReplicas(central, auth, edge []replica) error {
	var files []string
	for _, r := range append(append(append([]replica{}, central...), auth...), edge...) {
		if r.Slot > 1 {
			files = append(files, r.Service)
		}
	}
	return w.step("file "+strings.Join(files, ","), func() error {
		w.replicas = files
		return nil
	})
}

func (w *world) inspect(service string) (containerInfo, error) {
	c := w.containers[service]
	if c == nil {
		return containerInfo{}, nil
	}
	return containerInfo{found: true, image: c.image, workDir: c.workDir, running: c.running}, nil
}

func (w *world) restorePrevious(bundle string, _ time.Time) error {
	return w.step("restore "+bundle, func() error {
		w.restored = bundle
		return nil
	})
}

func (w *world) oneOffsRunning() ([]string, error) { return w.oneOffs, nil }

func (w *world) standInCheck(_ *state.State, svc string) error {
	w.memChecked = append(w.memChecked, svc)
	if w.memShort[svc] {
		return fmt.Errorf("not enough free memory for a stand-in %s replica", svc)
	}
	return nil
}

func (w *world) removeContainer(service string) error {
	return w.step("rm "+service, func() error {
		delete(w.containers, service)
		return nil
	})
}

func (w *world) update(f func(*state.State)) error {
	s := w.load()
	before := fmt.Sprint(s.Surge)
	return w.step("state", func() error {
		f(s)
		if after := fmt.Sprint(s.Surge); after != before {
			w.events = append(w.events, "surge "+after)
		}
		w.persist(s)
		return nil
	})
}

func preparedState(auth, edge []state.Slot) *state.State {
	st := &state.State{Target: "vps", Version: toV, PreviousVersion: fromV, ProxyMode: proxy.ModeExternal,
		AuthURL: "https://id.example.com", PostgresHost: "127.0.0.1:5432", BundleDir: "bundle-new",
		MountedBundleDirs: []string{"bundle-old"}}
	// What the staged configure records: central's replica at the version
	// it runs.
	st.SetSlots(CentralService, old(1))
	st.SetSlots(AuthService, auth)
	st.SetSlots(EdgeService, edge)
	return st
}

func engineFor(w *world, surge, downtime bool) *rolloutEngine {
	st := w.load()
	// As centralStandsIn decides it.
	_, recorded := st.Surge[CentralService]
	inPlace := !recorded && (!surge || !w.centralStandIn)
	return &rolloutEngine{st: st, topo: w.topo, bundleDir: w.targetDir, ops: w, out: func(string, ...any) {},
		fronts: proxy.Fronts{Additional: 8082, MutualTLS: 8083, Central: 8090}, genDir: "/gen", surge: surge, allowDowntime: downtime, centralInPlace: inPlace}
}

func old(ns ...int) []state.Slot {
	var out []state.Slot
	for _, n := range ns {
		out = append(out, state.Slot{N: n, Version: fromV})
	}
	return out
}

// done checks the world a finished rollout leaves.
func (w *world) done(t *testing.T, authSlots, edgeSlots []int) {
	t.Helper()
	st := w.load()
	if st.RolloutUnderway() || st.MixedVersions() != "" || st.MigratedAt == nil {
		t.Errorf("state after the rollout: underway=%v mixed=%q migrated=%v", st.RolloutUnderway(), st.MixedVersions(), st.MigratedAt)
	}
	want := map[string]bool{}
	for svc, ns := range map[string][]int{CentralService: {1}, AuthService: authSlots, EdgeService: edgeSlots} {
		if got := slotNumbers(st.ActiveSlots(svc)); !reflect.DeepEqual(got, ns) {
			t.Errorf("%s slots %v, want %v", svc, got, ns)
		}
		var up []string
		for _, n := range ns {
			want[topology.SlotService(svc, n)] = true
			up = append(up, topology.SlotService(svc, n))
		}
		got := append([]string(nil), w.upstream[svc]...)
		sort.Strings(got)
		sort.Strings(up)
		if !reflect.DeepEqual(got, up) {
			t.Errorf("%s upstream %v, want %v", svc, got, up)
		}
	}
	for name, c := range w.containers {
		if !want[name] {
			t.Errorf("container %s left behind", name)
		} else if c.image != imageOf(baseOf(name), w.targetV) || c.workDir != w.targetDir || !c.running {
			t.Errorf("%s: %+v", name, c)
		}
	}
	for name := range want {
		if w.containers[name] == nil {
			t.Errorf("%s has no container", name)
		}
	}
	// MarkRunning (which deletes the old bundle) comes after every container
	// is the new bundle's; only the record of the rollout being over follows.
	if i := lastIndexOf(w.events, "markRunning"); i < 0 || i < len(w.events)-2 {
		t.Errorf("markRunning must be the last step, events end with %v", w.events[max(0, len(w.events)-4):])
	}
	if len(w.violations) > 0 {
		t.Errorf("violations: %v", w.violations)
	}
}

func lastIndexOf(events []string, ev string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i] == ev {
			return i
		}
	}
	return -1
}

func indexOf(events []string, ev string) int {
	for i, e := range events {
		if e == ev {
			return i
		}
	}
	return -1
}

func TestRolloutOrderWithStandIn(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1, 2}, []int{1})

	var got []string
	for _, e := range w.events {
		if strings.HasPrefix(e, "compose") || strings.HasPrefix(e, "rm ") || e == "pull" || e == "migrate" || e == "markStarting" || strings.HasPrefix(e, "surge") {
			got = append(got, e)
		}
	}
	want := []string{
		"pull", "migrate", "markStarting",
		"compose up -d --no-deps central",
		"surge map[auth:3]", "compose up -d --no-deps auth-3",
		"compose up -d --no-deps auth", "compose up -d --no-deps auth-2",
		"rm auth-3", "surge map[]",
		"surge map[edge:2]", "compose up -d --no-deps edge-2",
		"compose up -d --no-deps edge",
		"rm edge-2", "surge map[]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps:\n got %v\nwant %v", got, want)
	}
	// Each slot leaves the proxy, is drained, then recreated.
	for _, svc := range []string{"auth", "auth-2", "edge"} {
		i := indexOf(w.events, "compose up -d --no-deps "+svc)
		if i < 2 || w.events[i-1] != "drained" || !strings.HasPrefix(w.events[i-2], "fronts") && !strings.HasPrefix(w.events[i-2], "reload") {
			t.Errorf("%s was not taken out of the proxy and drained right before it was recreated: %v", svc, w.events[max(0, i-3):i+1])
		}
	}
	// central is waited for before auth or edge are touched.
	if indexOf(w.events, "ready central") > indexOf(w.events, "compose up -d --no-deps auth-3") {
		t.Error("auth started before central was ready")
	}
	// A stand-in is warmed before it is routed to.
	if indexOf(w.events, "warm auth-3") > indexOf(w.events, "reload auth=auth,auth-2,auth-3 edge=edge") {
		t.Error("auth-3 routed before it was warmed")
	}
}

// The stand-in marker is in state.json before the stand-in starts, and only
// leaves it after the container is gone.
func TestRolloutStandInMarkerBracketsTheContainer(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{AuthService, EdgeService} {
		marked := indexOf(w.events, "surge map["+svc+":2]")
		started := indexOf(w.events, "compose up -d --no-deps "+svc+"-2")
		removed := indexOf(w.events, "rm "+svc+"-2")
		cleared := -1
		for i := removed; i >= 0 && i < len(w.events); i++ {
			if w.events[i] == "surge map[]" {
				cleared = i
				break
			}
		}
		if marked < 0 || started < marked || removed < started || cleared < removed {
			t.Errorf("%s: marked %d, started %d, removed %d, cleared %d", svc, marked, started, removed, cleared)
		}
	}
	w.done(t, []int{1}, []int{1})
}

// A rollout cut off at any call -- before or after that call takes effect --
// is finished by running it again, with the proxy never routing to a
// replica that isn't ready and never to fewer than before.
func TestRolloutResumesAfterACrashAnywhere(t *testing.T) {
	for _, layout := range []struct {
		name           string
		auth, edge     []state.Slot
		centralStandIn bool
	}{
		{"one replica each", old(1), old(1), false},
		{"two auth, one edge", old(1, 2), old(1), false},
		{"one replica each, central stand-in", old(1), old(1), true},
		{"two auth, one edge, central stand-in", old(1, 2), old(1), true},
	} {
		// How many calls a clean run makes.
		clean := newWorld(t, preparedState(layout.auth, layout.edge))
		clean.centralStandIn = layout.centralStandIn
		if err := engineFor(clean, true, false).run(); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= clean.calls; i++ {
			for _, after := range []bool{false, true} {
				name := fmt.Sprintf("%s/crash at call %d (%s, after=%v)", layout.name, i, clean.callNames[i-1], after)
				t.Run(name, func(t *testing.T) {
					w := newWorld(t, preparedState(layout.auth, layout.edge))
					w.centralStandIn = layout.centralStandIn
					w.crashAt, w.crashAfter = i, after
					err := engineFor(w, true, false).run()
					if !errors.Is(err, errCrash) && err != nil {
						t.Fatalf("first run: %v", err)
					}
					w.crashed, w.crashAt = false, 0
					// The next run starts from state.json, like a new process.
					if err := engineFor(w, true, false).run(); err != nil {
						t.Fatalf("resumed run: %v\nevents: %v", err, w.events)
					}
					var as, es []int
					for _, sl := range layout.auth {
						as = append(as, sl.N)
					}
					for _, sl := range layout.edge {
						es = append(es, sl.N)
					}
					w.done(t, as, es)
				})
			}
		}
	}
}

// A stand-in that never becomes ready is taken away again; nothing else
// changed.
func TestRolloutDropsAStandInThatDoesNotComeUp(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	w.never["auth-3"] = true
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "stand-in auth-3 never became ready") {
		t.Fatalf("error: %v", err)
	}
	st := w.load()
	if st.Surge != nil || !reflect.DeepEqual(slotNumbers(st.ActiveSlots(AuthService)), []int{1, 2}) {
		t.Errorf("state: surge %v, auth slots %v", st.Surge, st.ActiveSlots(AuthService))
	}
	if w.containers["auth-3"] != nil {
		t.Error("auth-3's container was left")
	}
	for _, svc := range []string{"auth", "auth-2"} {
		if w.containers[svc].image != imageOf(AuthService, fromV) {
			t.Errorf("%s was touched", svc)
		}
	}
	if !reflect.DeepEqual(w.replicas, []string{"auth-2"}) {
		t.Errorf("replicas.yml: %v", w.replicas)
	}
	if len(w.violations) > 0 {
		t.Errorf("violations: %v", w.violations)
	}
}

// central is a single instance, so its restart is the longest unavailable
// window of a rollout: it gets its own, longer limit; replicas keep the
// normal one.
func TestRolloutWaitsForCentralLongerThanForReplicas(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	if got := w.waits[CentralService]; got != centralReadyTimeout {
		t.Errorf("central waited for %s, want %s", got, centralReadyTimeout)
	}
	if centralReadyTimeout <= readyTimeout {
		t.Errorf("centralReadyTimeout %s must exceed readyTimeout %s", centralReadyTimeout, readyTimeout)
	}
	for _, svc := range []string{"auth-3", "auth", "auth-2", "edge-2", "edge"} {
		if got := w.waits[svc]; got != readyTimeout {
			t.Errorf("%s waited for %s, want %s", svc, got, readyTimeout)
		}
	}
}

// central that never comes up stops the rollout before auth or edge are
// touched, tells how to go on, and a rerun only waits for central again.
func TestRolloutCentralNeverReadyThenRetried(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	w.never[CentralService] = true
	err := engineFor(w, true, false).run()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"central never became ready", "waited 5m0s", "versola rollout --abort", "run the same `versola rollout` again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	for _, e := range w.events {
		if strings.Contains(e, "auth") || strings.Contains(e, "edge") {
			if strings.HasPrefix(e, "compose") || strings.HasPrefix(e, "surge") {
				t.Fatalf("auth/edge touched while central is down: %s", e)
			}
		}
	}
	// Fixed (or it was only slow); the same command again.
	w.containers[CentralService].ready = true
	w.events = nil
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1, 2}, []int{1})
	if i := indexOf(w.events, "compose up -d --no-deps central"); i >= 0 {
		t.Errorf("central was restarted again on the rerun")
	}
}

// During an abort the hint must point at --abort, not at a new rollout.
func TestRolloutCentralNeverReadyDuringAbortHint(t *testing.T) {
	st := preparedState(old(1, 2), old(1))
	st.RollingBack = true
	w := newWorld(t, st)
	w.never[CentralService] = true
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "central never became ready") {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(err.Error(), "run `versola rollout --abort` again") || strings.Contains(err.Error(), "goes back as well") {
		t.Errorf("wrong hint during an abort: %v", err)
	}
}

// A replica recreated on the new version that doesn't come up stays out of
// the proxy; the stand-in keeps the capacity; a later run picks it up.
func TestRolloutReplacedReplicaNotReadyThenRetried(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	w.never["auth-2"] = true
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "auth-2 never became ready") {
		t.Fatalf("error: %v", err)
	}
	for _, s := range w.upstream[AuthService] {
		if s == "auth-2" {
			t.Error("auth-2 is in the proxy")
		}
	}
	if st := w.load(); st.Surge[AuthService] != 3 {
		t.Errorf("the stand-in must stay: %v", st.Surge)
	}
	// Fixed (say, a config problem on the host); the same command again.
	w.containers["auth-2"].ready = true
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1, 2}, []int{1})
}

func TestRolloutWarmUpFailureIsRetried(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.coldOnce["auth"] = true
	if err := engineFor(w, true, false).run(); err == nil || !strings.Contains(err.Error(), "doesn't answer requests yet") {
		t.Fatalf("error: %v", err)
	}
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1}, []int{1})
}

// Without a stand-in, two replicas: one fewer serves meanwhile, never zero.
func TestRolloutNoSurge(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1, 2)))
	w.minUp = map[string]int{AuthService: 1, EdgeService: 1}
	if err := engineFor(w, false, false).run(); err != nil {
		t.Fatal(err)
	}
	for _, e := range w.events {
		if strings.HasPrefix(e, "surge") {
			t.Errorf("a stand-in was recorded: %s", e)
		}
	}
	w.done(t, []int{1, 2}, []int{1, 2})
}

// One replica without a stand-in: refused, unless downtime is accepted.
func TestRolloutOneReplicaNoSurge(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	err := engineFor(w, false, false).run()
	if err == nil || !strings.Contains(err.Error(), "--allow-downtime") {
		t.Fatalf("error: %v", err)
	}
	if w.containers["auth"].image != imageOf(AuthService, fromV) {
		t.Error("auth was recreated anyway")
	}

	w = newWorld(t, preparedState(old(1), old(1)))
	w.minUp = map[string]int{}
	if err := engineFor(w, false, true).run(); err != nil {
		t.Fatal(err)
	}
	w.violations = nil // the in-place recreation routes to a replica that is restarting, by design
	w.done(t, []int{1}, []int{1})
}

// Ctrl-C stops between steps, and the same command continues.
func TestRolloutInterruptStopsBetweenSteps(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	e := engineFor(w, true, false)
	n := 0
	e.interrupted = func() bool { n++; return n > 4 }
	if err := e.run(); !errors.Is(err, errInterrupted) {
		t.Fatalf("error: %v", err)
	}
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1, 2}, []int{1})
}

// A run cut off after taking a slot out of the proxy, before recreating it:
// the next run puts it back first (reconcile), then replaces it properly.
func TestRolloutReconcileRestoresTheUpstream(t *testing.T) {
	st := preparedState(old(1, 2), old(1))
	w := newWorld(t, st)
	w.upstream[AuthService] = []string{"auth"} // auth-2 was taken out, then the run died
	e := engineFor(w, false, false)
	e.st.MigratedAt = nil
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), w.upstream[AuthService]...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"auth", "auth-2"}) {
		t.Errorf("upstream after reconcile: %v", got)
	}
}

// A slot recorded at the old version whose container is already the new
// version's (recreated, then the run died before recording it) is read back
// as the new version.
func TestRolloutReconcileReadsVersionsFromContainers(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	w.containers["auth-2"].image = imageOf(AuthService, toV)
	w.containers["auth-2"].workDir = newBundleDir
	e := engineFor(w, true, false)
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	got := w.load().ActiveSlots(AuthService)
	if got[1].Version != toV || got[0].Version != fromV {
		t.Errorf("slots: %v", got)
	}
}

// ---- CheckRollout ----

func rolloutGuardState(t *testing.T) *state.State {
	st, _ := guardBundle(t, true)
	st.Version = fromV
	st.PostgresHost = "127.0.0.1:5432"
	return st
}

func TestCheckRollout(t *testing.T) {
	st := rolloutGuardState(t)
	if done, err := CheckRollout(st, toV, RolloutOptions{}); done || err != nil {
		t.Fatalf("a plain rollout: %v %v", done, err)
	}
	if done, err := CheckRollout(st, fromV, RolloutOptions{}); !done || err != nil {
		t.Errorf("the version that runs: %v %v", done, err)
	}
	refused := map[string]struct {
		mutate  func(s *state.State)
		version string
		opts    RolloutOptions
		want    string
	}{
		"local":                     {func(s *state.State) { s.Target = "local" }, toV, RolloutOptions{}, "throwaway"},
		"v prefix":                  {func(s *state.State) {}, "v0.7.0", RolloutOptions{}, "without a leading"},
		"before PR-1":               {func(s *state.State) { s.PostgresHost = "" }, toV, RolloutOptions{}, "configure it once"},
		"not up yet":                {func(s *state.State) { s.MountedBundleDirs = []string{"bundle-0"} }, toV, RolloutOptions{}, "versola up"},
		"one replica, no surge":     {func(s *state.State) {}, toV, RolloutOptions{NoSurge: true}, "--allow-downtime"},
		"downtime without no-surge": {func(s *state.State) {}, toV, RolloutOptions{AllowDowntime: true}, "only goes with --no-surge"},
		"another version under way": {func(s *state.State) {
			s.PreviousVersion, s.Version = fromV, toV
			s.SetSlots(AuthService, old(1))
		}, "0.8.0", RolloutOptions{}, "finish it with `versola rollout 0.7.0`"},
		"back while under way": {func(s *state.State) {
			s.PreviousVersion, s.Version = fromV, toV
			s.SetSlots(AuthService, old(1))
		}, fromV, RolloutOptions{}, "--abort"},
		"mixed without a rollout": {func(s *state.State) {
			s.SetSlots(AuthService, []state.Slot{{N: 1, Version: "0.5.0"}})
		}, toV, RolloutOptions{}, "edited by hand"},
	}
	for name, c := range refused {
		s := *st
		s.Slots = nil
		c.mutate(&s)
		_, err := CheckRollout(&s, c.version, c.opts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}

	// Under way to the same version: continued, even though the stack is not
	// "running from the current bundle" (the new bundle is not up yet).
	s := *st
	s.PreviousVersion, s.Version = fromV, toV
	s.SetSlots(AuthService, old(1))
	s.MountedBundleDirs = []string{"bundle-0", "bundle-1"}
	if done, err := CheckRollout(&s, toV, RolloutOptions{}); done || err != nil {
		t.Errorf("continuing: %v %v", done, err)
	}
	// Two replicas: --no-surge is fine.
	s2 := *st
	s2.SetSlots(AuthService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: fromV}})
	s2.SetSlots(EdgeService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: fromV}})
	if _, err := CheckRollout(&s2, toV, RolloutOptions{NoSurge: true}); err != nil {
		t.Errorf("no-surge with two replicas: %v", err)
	}
}

func TestConfigureRefusedDuringRollout(t *testing.T) {
	st := preparedState(old(1), old(1))
	if err := replicasBlockConfigure(st); err == nil || !strings.Contains(err.Error(), "versola rollout 0.7.0") {
		t.Errorf("got %v", err)
	}
}

func TestRolloutPlanAndSummary(t *testing.T) {
	st := rolloutGuardState(t)
	st.SetSlots(AuthService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: fromV}})
	plan := RolloutPlan(st, toV, RolloutOptions{})
	for _, want := range []string{"0.6.2 -> 0.7.0", "Postgres at 127.0.0.1:5432", "auth: a stand-in on 0.7.0 in slot 3", "edge: a stand-in on 0.7.0 in slot 2", "Both are checked before it starts", "central: stopped and started"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if s := RolloutSummary(st); s != "" {
		t.Errorf("no rollout, summary %q", s)
	}
	p := preparedState(old(1), old(1))
	p.Surge = map[string]int{AuthService: 2}
	p.SetSlots(AuthService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: toV}})
	sum := RolloutSummary(p)
	for _, want := range []string{"0.6.2 -> 0.7.0", "slot 2 at 0.7.0 (stand-in)", "migrations: not applied"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
}

func TestProbeReplica(t *testing.T) {
	// Only shapes the request; the HTTP side is exercised on the test stand.
	r, err := standInReplica(testTopologyFrom(t, rolloutCompose), AuthService, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ports.Port != 8180 {
		t.Fatalf("auth-2 PORT %d", r.Ports.Port)
	}
	u := url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(r.Ports.Port), Path: replicaProbes[AuthService].path}
	if u.String() != "http://127.0.0.1:8180/.well-known/openid-configuration" {
		t.Error(u.String())
	}
}

func testTopologyFrom(t *testing.T, js string) topology.Topology {
	t.Helper()
	topo, err := topology.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

// A container of the new image created from the old bundle (two releases
// naming the same image) is not taken for the new version: it would still
// mount the bundle the end of the rollout deletes.
func TestRolloutSameImageFromTheOldBundleIsRecreated(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.containers[CentralService].image = imageOf(CentralService, toV)
	w.containers["auth"].image = imageOf(AuthService, toV)
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	if indexOf(w.events, "compose up -d --no-deps central") < 0 || indexOf(w.events, "compose up -d --no-deps auth") < 0 {
		t.Errorf("central and auth were not recreated from the new bundle: %v", w.events)
	}
	w.done(t, []int{1}, []int{1})
}

// nginx reloaded with the stand-in, then the check of its fronts failed: the
// stand-in is not stopped (nginx may route to it), the upstreams are put
// back, and the next run carries on.
func TestRolloutStandInKeptWhenRoutingItFails(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	w.failOnce["reload auth=auth,auth-2,auth-3 edge=edge"] = errors.New("fronts not answering")
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "keeps running") {
		t.Fatalf("error: %v", err)
	}
	if w.containers["auth-3"] == nil {
		t.Fatal("the stand-in was removed while nginx may route to it")
	}
	if got := w.upstream[AuthService]; !reflect.DeepEqual(got, []string{"auth", "auth-2"}) {
		t.Errorf("the upstreams were not put back: %v", got)
	}
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1, 2}, []int{1})
}

func TestSamePorts(t *testing.T) {
	oldTopo := testTopologyFrom(t, rolloutCompose)
	slots := map[string][]state.Slot{AuthService: old(1, 2), EdgeService: old(1)}
	if err := samePorts(oldTopo, oldTopo, slots); err != nil {
		t.Fatal(err)
	}
	moved := testTopologyFrom(t, strings.Replace(rolloutCompose, `"DPORT":"8096"`, `"DPORT":"8097"`, 1))
	if err := samePorts(oldTopo, moved, slots); err == nil || !strings.Contains(err.Error(), "moves edge's ports") {
		t.Errorf("got %v", err)
	}
}

// After the last replica moved but before the rollout recorded itself as
// done, nothing is mixed any more -- `up` still refuses: PreviousVersion says
// the rollout is not finished.
func TestUpRefusedUntilTheRolloutIsFinished(t *testing.T) {
	st, bundle := guardBundle(t, true)
	st.PreviousVersion = fromV
	if err := checkRunnable(st, bundle); err == nil || !strings.Contains(err.Error(), "versola rollout") {
		t.Errorf("got %v", err)
	}
}

// On vps central sits behind the proxy's front on its shifted ports, and
// every routing of the rollout carries it, so the proxy never loses it.
func TestRolloutRoutesCentralThroughItsFront(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	if w.centralFront != 8090 {
		t.Errorf("central front %d, want 8090", w.centralFront)
	}
	if want := []proxy.Backend{{Service: "central", Port: 28090}}; !reflect.DeepEqual(w.central, want) {
		t.Errorf("routed central: %+v, want %+v", w.central, want)
	}
	if got := w.urlOf(CentralService); got != "http://localhost:28091/readiness" {
		t.Errorf("central is waited for at %q", got)
	}
}

// With the proxy in front of central and a stand-in wanted, central is moved
// like a replicated service: a stand-in of the new version first, then the
// one replica, then the stand-in goes -- and the front never routes to fewer
// than one ready central. Both are waited for as long as central is.
func TestRolloutCentralThroughAStandIn(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.centralStandIn = true
	w.minUp[CentralService] = 1
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1}, []int{1})
	var got []string
	for _, e := range w.events {
		if strings.HasPrefix(e, "compose up -d --no-deps central") || strings.HasPrefix(e, "rm central") || strings.HasPrefix(e, "surge map[central") || e == "surge map[]" && len(got) == 4 {
			got = append(got, e)
		}
	}
	want := []string{"surge map[central:2]", "compose up -d --no-deps central-2", "compose up -d --no-deps central", "rm central-2", "surge map[]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("central's steps:\n got %v\nwant %v", got, want)
	}
	if indexOf(w.events, "rm central-2") > indexOf(w.events, "surge map[auth:2]") {
		t.Error("auth started before central was done")
	}
	for _, svc := range []string{"central", "central-2"} {
		if w.waits[svc] != centralReadyTimeout {
			t.Errorf("%s waited for %s, want %s", svc, w.waits[svc], centralReadyTimeout)
		}
	}
	if indexOf(w.memChecked, CentralService) < 0 {
		t.Error("no memory/connections check before central's stand-in")
	}
}

// Without a stand-in (--no-surge) central is restarted in place as before,
// whatever the proxy fronts -- and needs no --allow-downtime.
func TestRolloutCentralInPlaceWithoutSurge(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1, 2)))
	w.centralStandIn = true
	w.minUp[AuthService], w.minUp[EdgeService] = 1, 1 // one replica fewer while each is replaced
	if err := engineFor(w, false, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1, 2}, []int{1, 2})
	for _, e := range w.events {
		if strings.Contains(e, "central-2") || strings.HasPrefix(e, "surge map[central") {
			t.Errorf("a central stand-in without surge: %s", e)
		}
	}
}

// A central stand-in that never becomes ready is taken away again, and the
// running central keeps serving: nothing else changed.
func TestRolloutDropsACentralStandInThatDoesNotComeUp(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.centralStandIn = true
	w.never["central-2"] = true
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "stand-in central-2 never became ready") {
		t.Fatalf("got %v", err)
	}
	st := w.load()
	if _, ok := st.Surge[CentralService]; ok || len(st.ActiveSlots(CentralService)) != 1 {
		t.Errorf("state: surge %v, central slots %v", st.Surge, st.ActiveSlots(CentralService))
	}
	if w.containers["central-2"] != nil {
		t.Error("the stand-in's container is left")
	}
	if c := w.containers[CentralService]; c == nil || c.image != imageOf(CentralService, fromV) || !c.running {
		t.Errorf("the running central was touched: %+v", c)
	}
	if got := w.upstream[CentralService]; !reflect.DeepEqual(got, []string{CentralService}) {
		t.Errorf("central's upstream %v", got)
	}
}

// The plan says how central moves: through a stand-in once the running proxy
// fronts it, in place otherwise or without surge; and the summary shows
// central's slots once the rollout records them.
func TestRolloutPlanCentral(t *testing.T) {
	st := rolloutGuardState(t)
	inPlace := "central: stopped and started"
	standIn := "central: a stand-in on the new version (central-2)"
	if plan := RolloutPlan(st, toV, RolloutOptions{}); !strings.Contains(plan, inPlace) || !strings.Contains(plan, "does not front central yet") {
		t.Errorf("no front yet:\n%s", plan)
	}
	dir, err := proxyGenerationsDir()
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, proxy.CurrentGeneration, "conf.d")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "central.conf"), []byte("server {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !centralStandsIn(st, RolloutOptions{}) || centralStandsIn(st, RolloutOptions{NoSurge: true}) {
		t.Error("centralStandsIn: fronted with surge only")
	}
	if plan := RolloutPlan(st, toV, RolloutOptions{}); !strings.Contains(plan, standIn) {
		t.Errorf("fronted:\n%s", plan)
	}
	if plan := RolloutPlan(st, toV, RolloutOptions{NoSurge: true}); !strings.Contains(plan, inPlace) || strings.Contains(plan, "does not front") {
		t.Errorf("--no-surge:\n%s", plan)
	}
	// A recorded stand-in is carried on, with or without surge.
	p := preparedState(old(1), old(1))
	p.Surge = map[string]int{CentralService: 2}
	p.SetSlots(CentralService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: toV}})
	if !centralStandsIn(p, RolloutOptions{NoSurge: true}) {
		t.Error("a recorded central stand-in must go through the stand-in path")
	}
	if plan := RolloutPlan(p, toV, RolloutOptions{NoSurge: true}); !strings.Contains(plan, "stand-in central-2 already started") {
		t.Errorf("recorded stand-in:\n%s", plan)
	}
	sum := RolloutSummary(p)
	if !strings.Contains(sum, "central: slot 1 at 0.6.2, slot 2 at 0.7.0 (stand-in)") || strings.Contains(sum, "see the image") {
		t.Errorf("summary:\n%s", sum)
	}
}

// A run that restarted central in place and gave up waiting for it, then a
// run that would move central through a stand-in (the operator dropped
// --no-surge): central is current but not ready, and is waited for -- the
// rollout does not carry on to auth and edge, or finish, on its word.
func TestRolloutWaitsForACentralItRoutedOnlyForWantOfAnother(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.centralStandIn = true
	w.never[CentralService] = true
	if err := engineFor(w, false, false).run(); err == nil || !strings.Contains(err.Error(), "central never became ready") {
		t.Fatalf("in place: %v", err)
	}
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "central never became ready") {
		t.Fatalf("second run went on with central not ready: %v\n%v", err, w.events)
	}
	if indexOf(w.events, "compose up -d --no-deps auth-2") >= 0 || indexOf(w.events, "compose up -d --no-deps auth") >= 0 {
		t.Error("auth was touched while central was not ready")
	}
	// Once central comes up, the same command finishes.
	delete(w.never, CentralService)
	w.containers[CentralService].ready = true
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatalf("third run: %v", err)
	}
	w.violations = nil // routed to central while it was down, for want of any other
	w.done(t, []int{1}, []int{1})
}

// A rollout prepared by an older versola-cli records no central slots: the
// running (old) central is read back from its container, and moved through
// a stand-in all the same.
func TestRolloutCentralWithoutRecordedSlots(t *testing.T) {
	st := preparedState(old(1), old(1))
	delete(st.Slots, CentralService)
	w := newWorld(t, st)
	w.containers[CentralService].image = imageOf(CentralService, fromV) // what runs
	w.centralStandIn = true
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	w.done(t, []int{1}, []int{1})
	if indexOf(w.events, "compose up -d --no-deps central-2") < 0 {
		t.Errorf("no central stand-in: %v", w.events)
	}
}
