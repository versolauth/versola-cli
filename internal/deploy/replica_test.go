package deploy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// fakeOps records what a run does, in order, and fails the calls it is told
// to. Nothing here touches Docker.
type fakeOps struct {
	events       []string
	failOn       map[string]error // by event text, e.g. "compose up -d --no-deps auth-3"
	proxyRunning bool
	saved        map[string][]state.Slot
	files        []string // services in the last replicas.yml
	busy         map[int]error
}

func newFakeOps() *fakeOps {
	return &fakeOps{failOn: map[string]error{}, proxyRunning: true, saved: map[string][]state.Slot{}, busy: map[int]error{}}
}

func (f *fakeOps) do(ev string) error {
	f.events = append(f.events, ev)
	return f.failOn[ev]
}

func (f *fakeOps) isRunning(c string) (bool, error) { return f.proxyRunning, nil }
func (f *fakeOps) compose(args ...string) error     { return f.do("compose " + strings.Join(args, " ")) }
func (f *fakeOps) waitReady(url string) error       { return f.do("ready " + url) }
func (f *fakeOps) portFree(port int) error {
	f.events = append(f.events, fmt.Sprintf("port %d", port))
	return f.busy[port]
}
func (f *fakeOps) waitDrained() { f.events = append(f.events, "drained") }
func (f *fakeOps) reload(c proxy.Config) error {
	return f.do("reload " + backends(c))
}
func (f *fakeOps) writeUpstreams(c proxy.Config) error {
	return f.do("upstreams " + backends(c))
}
func (f *fakeOps) save(service string, slots []state.Slot) error {
	f.saved[service] = slotNumbersOf(slots)
	return f.do(fmt.Sprintf("save %s %v", service, slotNumbers(slots)))
}
func (f *fakeOps) writeReplicas(auth, edge []replica) error {
	f.files = nil
	for _, r := range append(append([]replica{}, auth...), edge...) {
		if r.Slot > 1 {
			f.files = append(f.files, r.Service)
		}
	}
	return f.do("file " + strings.Join(f.files, ","))
}

func slotNumbersOf(s []state.Slot) []state.Slot { return append([]state.Slot(nil), s...) }

func backends(c proxy.Config) string {
	var a, e []string
	for _, b := range c.Auth {
		a = append(a, b.Service)
	}
	for _, b := range c.Edge {
		e = append(e, b.Service)
	}
	return "auth=" + strings.Join(a, ",") + " edge=" + strings.Join(e, ",")
}

func testScaler(t *testing.T, ops *fakeOps, st *state.State) *replicaScaler {
	t.Helper()
	if st == nil {
		st = &state.State{Target: "vps", Version: "0.6.2", ProxyMode: proxy.ModeNginx, AuthURL: "http://1.2.3.4"}
	}
	s := newReplicaScaler(st, testTopology(t), ops)
	s.out = func(string, ...any) {}
	return s
}

func TestFreeAndHighestSlots(t *testing.T) {
	have := []state.Slot{{N: 1}, {N: 3}}
	got, err := freeSlots(have, 3)
	if err != nil || !reflect.DeepEqual(got, []int{2, 4, 5}) {
		t.Errorf("freeSlots: %v, %v", got, err)
	}
	if _, err := freeSlots(have, 8); err == nil {
		t.Error("more than the 9 slots: want an error")
	}
	if got, err := highestSlots([]state.Slot{{N: 1}, {N: 2}, {N: 4}}, 2); err != nil || !reflect.DeepEqual(got, []int{4, 2}) {
		t.Errorf("highestSlots: %v, %v", got, err)
	}
	if _, err := highestSlots([]state.Slot{{N: 1}, {N: 2}}, 2); err == nil {
		t.Error("removing every replica: want an error")
	}
	if _, err := highestSlots([]state.Slot{{N: 1}}, 1); err == nil {
		t.Error("removing the only replica: want an error")
	}
}

func TestAddOrdersStartReadyThenRoute(t *testing.T) {
	ops := newFakeOps()
	s := testScaler(t, ops, nil)
	if err := s.add(AuthService, 2); err != nil {
		t.Fatal(err)
	}
	// The order that matters: recorded, started, awaited, then routed --
	// and the next replica only after that.
	var got []string
	for _, e := range ops.events {
		switch {
		case strings.HasPrefix(e, "save"):
			got = append(got, "save")
		case strings.HasPrefix(e, "file"):
			got = append(got, e)
		case strings.HasPrefix(e, "compose"):
			got = append(got, "compose "+lastWord(e))
		case strings.HasPrefix(e, "ready"):
			got = append(got, "ready")
		case strings.HasPrefix(e, "reload"):
			got = append(got, e)
		}
	}
	wantGot := []string{"save", "file auth-2", "compose auth-2", "ready", "reload auth=auth,auth-2 edge=edge",
		"save", "file auth-2,auth-3", "compose auth-3", "ready", "reload auth=auth,auth-2,auth-3 edge=edge"}
	if !reflect.DeepEqual(got, wantGot) {
		t.Errorf("events:\n got %v\nwant %v", got, wantGot)
	}
	if n := slotNumbers(s.slots[AuthService]); !reflect.DeepEqual(n, []int{1, 2, 3}) {
		t.Errorf("slots: %v", n)
	}
}

func lastWord(s string) string {
	f := strings.Fields(s)
	return f[len(f)-1]
}

func TestAddStartCommandUsesNoDeps(t *testing.T) {
	ops := newFakeOps()
	if err := testScaler(t, ops, nil).add(EdgeService, 1); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ops.events {
		if e == "compose up -d --no-deps edge-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("events: %v", ops.events)
	}
}

// A replica that does not become ready is stopped, removed and forgotten;
// the ones before it stay; the proxy never saw it.
func TestAddUndoesAReplicaThatDoesNotComeUp(t *testing.T) {
	ops := newFakeOps()
	ops.failOn["compose up -d --no-deps auth-3"] = errors.New("boom")
	s := testScaler(t, ops, nil)
	err := s.add(AuthService, 2)
	if err == nil || !strings.Contains(err.Error(), "added 1 replica(s) of auth") || !strings.Contains(err.Error(), "couldn't start auth-3") {
		t.Fatalf("error: %v", err)
	}
	if n := slotNumbers(s.slots[AuthService]); !reflect.DeepEqual(n, []int{1, 2}) {
		t.Errorf("slots after the failure: %v", n)
	}
	if !reflect.DeepEqual(ops.files, []string{"auth-2"}) {
		t.Errorf("replicas.yml lists %v", ops.files)
	}
	tail := ops.events[len(ops.events)-5:]
	wantTail := []string{"compose up -d --no-deps auth-3", "compose stop -t 20 auth-3", "compose rm -f auth-3", "save auth [1 2]", "file auth-2"}
	if !reflect.DeepEqual(tail, wantTail) {
		t.Errorf("undo order:\n got %v\nwant %v", tail, wantTail)
	}
	for _, e := range ops.events {
		if strings.HasPrefix(e, "reload") && strings.Contains(e, "auth-3") {
			t.Errorf("the proxy was told about auth-3: %s", e)
		}
	}
}

func TestAddNeverReadyIsUndone(t *testing.T) {
	ops := newFakeOps()
	s := testScaler(t, ops, nil)
	// The readiness URL of auth-2: its DPORT is 8181 on slot 2.
	ops.failOn["ready http://localhost:8181/readiness"] = errors.New("timeout")
	err := s.add(AuthService, 1)
	if err == nil || !strings.Contains(err.Error(), "auth-2 never became ready") {
		t.Fatalf("error: %v", err)
	}
	if n := slotNumbers(s.slots[AuthService]); !reflect.DeepEqual(n, []int{1}) {
		t.Errorf("slots: %v", n)
	}
	if ops.files != nil && len(ops.files) != 0 {
		t.Errorf("replicas.yml still lists %v", ops.files)
	}
}

// Nothing is started when the last replica could not be: the check is for
// the final layout.
func TestAddChecksTheWholeLayoutFirst(t *testing.T) {
	ops := newFakeOps()
	s := testScaler(t, ops, nil)
	if err := s.add(AuthService, 9); err == nil {
		t.Fatal("9 more auth replicas do not fit in 9 slots")
	}
	if len(ops.events) != 0 {
		t.Errorf("something ran: %v", ops.events)
	}
}

func TestAddWithProxyDownOnlyWritesTheFile(t *testing.T) {
	ops := newFakeOps()
	ops.proxyRunning = false
	if err := testScaler(t, ops, nil).add(AuthService, 1); err != nil {
		t.Fatal(err)
	}
	var reloaded, wrote bool
	for _, e := range ops.events {
		reloaded = reloaded || strings.HasPrefix(e, "reload")
		wrote = wrote || strings.HasPrefix(e, "upstreams")
	}
	if reloaded || !wrote {
		t.Errorf("reloaded=%v wrote=%v: %v", reloaded, wrote, ops.events)
	}
}

func TestRemoveDrainsBeforeStoppingAndRecordsLast(t *testing.T) {
	ops := newFakeOps()
	st := &state.State{Target: "vps", Version: "0.6.2", ProxyMode: proxy.ModeNginx, AuthURL: "http://1.2.3.4",
		Slots: map[string][]state.Slot{AuthService: {{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}, {N: 3, Version: "0.6.2"}}}}
	s := testScaler(t, ops, st)
	if err := s.remove(AuthService, 2); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"reload auth=auth,auth-2 edge=edge", "drained", "compose stop -t 20 auth-3", "compose rm -f auth-3", "save auth [1 2]", "file auth-2",
		"reload auth=auth edge=edge", "drained", "compose stop -t 20 auth-2", "compose rm -f auth-2", "save auth [1]", "file ",
	}
	if !reflect.DeepEqual(ops.events, want) {
		t.Errorf("events:\n got %v\nwant %v", ops.events, want)
	}
	if n := slotNumbers(s.slots[AuthService]); !reflect.DeepEqual(n, []int{1}) {
		t.Errorf("slots: %v", n)
	}
}

func TestRemoveKeepsTheRecordWhenStopFails(t *testing.T) {
	ops := newFakeOps()
	ops.failOn["compose stop -t 20 auth-2"] = errors.New("boom")
	st := &state.State{Target: "vps", Version: "0.6.2", ProxyMode: proxy.ModeNginx, AuthURL: "http://1.2.3.4",
		Slots: map[string][]state.Slot{AuthService: {{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}}}}
	s := testScaler(t, ops, st)
	err := s.remove(AuthService, 1)
	if err == nil || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("error: %v", err)
	}
	for _, e := range ops.events {
		if strings.HasPrefix(e, "save") || strings.HasPrefix(e, "file") {
			t.Errorf("recorded despite the failure: %s", e)
		}
	}
	if n := slotNumbers(s.slots[AuthService]); !reflect.DeepEqual(n, []int{1, 2}) {
		t.Errorf("slots: %v", n)
	}
}

func TestRemoveLastReplicaRefused(t *testing.T) {
	ops := newFakeOps()
	if err := testScaler(t, ops, nil).remove(EdgeService, 1); err == nil {
		t.Fatal("the only edge replica cannot be removed")
	}
	if len(ops.events) != 0 {
		t.Errorf("something ran: %v", ops.events)
	}
}

func TestRemoveWithProxyDownDoesNotWaitForDrain(t *testing.T) {
	ops := newFakeOps()
	ops.proxyRunning = false
	st := &state.State{Target: "vps", Version: "0.6.2", ProxyMode: proxy.ModeNginx, AuthURL: "http://1.2.3.4",
		Slots: map[string][]state.Slot{EdgeService: {{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}}}}
	if err := testScaler(t, ops, st).remove(EdgeService, 1); err != nil {
		t.Fatal(err)
	}
	for _, e := range ops.events {
		if e == "drained" || strings.HasPrefix(e, "reload") {
			t.Errorf("proxy is down but: %s", e)
		}
	}
}

func TestReplicasBlockConfigure(t *testing.T) {
	st := &state.State{Version: "0.6.2"}
	if err := replicasBlockConfigure(st); err != nil {
		t.Errorf("one replica each: %v", err)
	}
	st.SetSlots(EdgeService, []state.Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}})
	err := replicasBlockConfigure(st)
	if err == nil || !strings.Contains(err.Error(), "edge has 2 replicas") || !strings.Contains(err.Error(), "replica remove") {
		t.Errorf("two edge replicas: %v", err)
	}
}

func TestReplicaSummary(t *testing.T) {
	st := &state.State{Version: "0.6.2"}
	if got := ReplicaSummary(st); got != "Replicas: auth x1 (slot 1), edge x1 (slot 1)" {
		t.Errorf("got %q", got)
	}
	st.SetSlots(AuthService, []state.Slot{{N: 1, Version: "0.6.2"}, {N: 3, Version: "0.6.2"}})
	if got := ReplicaSummary(st); got != "Replicas: auth x2 (slots 1, 3), edge x1 (slot 1)" {
		t.Errorf("got %q", got)
	}
}

// A port some other process holds is found before anything is changed.
func TestAddRefusesBusyHostPort(t *testing.T) {
	ops := newFakeOps()
	ops.busy[8181] = errors.New("already used by something")
	err := testScaler(t, ops, nil).add(AuthService, 1)
	if err == nil || !strings.Contains(err.Error(), "auth-2 needs port 8181") {
		t.Fatalf("error: %v", err)
	}
	for _, e := range ops.events {
		if !strings.HasPrefix(e, "port") {
			t.Errorf("changed something before the port check passed: %s", e)
		}
	}
}

func TestHostPortsOf(t *testing.T) {
	topo := testTopology(t)
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := hostPortsOf(auth[0], true); !reflect.DeepEqual(got, []int{8180, 8181, 8182}) {
		t.Errorf("vps: %v", got)
	}
	if got := hostPortsOf(auth[0], false); !reflect.DeepEqual(got, []int{8181}) {
		t.Errorf("local publishes only the diagnostics port: %v", got)
	}
}

func TestParseComposeVersion(t *testing.T) {
	for in, want := range map[string][2]int{"2.29.1\n": {2, 29}, "v5.0.2": {5, 0}, "5.0.2-desktop.1": {5, 0}, "2.24": {2, 24}} {
		maj, min, ok := parseComposeVersion(in)
		if !ok || maj != want[0] || min != want[1] {
			t.Errorf("%q: %d.%d %v", in, maj, min, ok)
		}
	}
	if _, _, ok := parseComposeVersion("dev"); ok {
		t.Error("garbage parsed")
	}
}

func TestCheckReplicaRequestRefusals(t *testing.T) {
	if err := CheckReplicaRequest(nil, AuthService, 1, true); err == nil {
		t.Error("nothing configured")
	}
	st := &state.State{Version: "0.6.2"}
	if err := CheckReplicaRequest(st, CentralService, 1, true); err == nil || !strings.Contains(err.Error(), "can't have replicas") {
		t.Errorf("central: %v", err)
	}
	if err := CheckReplicaRequest(st, AuthService, 0, true); err == nil {
		t.Error("count 0")
	}
}

// A replica that cannot be stopped is not forgotten: it stays recorded so
// `replica remove` can still reach it, and nothing is rewritten.
func TestAddKeepsTheRecordWhenCleanupCannotStop(t *testing.T) {
	ops := newFakeOps()
	ops.failOn["compose up -d --no-deps auth-2"] = errors.New("boom")
	ops.failOn["compose stop -t 20 auth-2"] = errors.New("cannot stop")
	s := testScaler(t, ops, nil)
	err := s.add(AuthService, 1)
	if err == nil || !strings.Contains(err.Error(), "stays recorded") {
		t.Fatalf("error: %v", err)
	}
	if n := slotNumbers(s.slots[AuthService]); !reflect.DeepEqual(n, []int{1, 2}) {
		t.Errorf("slot 2 must stay recorded: %v", n)
	}
	for _, e := range ops.events {
		if strings.HasPrefix(e, "compose rm") {
			t.Errorf("removed a container that did not stop: %s", e)
		}
	}
	if !reflect.DeepEqual(ops.files, []string{"auth-2"}) {
		t.Errorf("replicas.yml lists %v", ops.files)
	}
}

func TestHostPortsOfSkipsOtherAddresses(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{"auth":{"environment":{"PORT":"8080","DPORT":"8081"},"ports":[
		{"target":8081,"published":"8081","host_ip":"127.0.0.2"},
		{"target":8080,"published":"8080","host_ip":"127.0.0.1"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := hostPortsOf(auth[0], false); !reflect.DeepEqual(got, []int{8180}) {
		t.Errorf("only the loopback publication can be probed: %v", got)
	}
}
