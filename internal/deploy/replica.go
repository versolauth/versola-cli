package deploy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
	"github.com/versolauth/versola-cli/internal/wait"
)

// stopTimeoutSeconds is how long Docker waits for a replica to stop before
// killing it. The application's own graceful shutdown is bounded at 15s
// (VersolaApp.gracefulShutdownTimeout) and Docker's default is 10s, which
// would kill a replica that is still finishing its requests.
const stopTimeoutSeconds = "20"

// Scalable says whether a service can have replicas: auth and edge. central
// runs migrations and is not replicated.
func Scalable(service string) bool { return service == AuthService || service == EdgeService }

// replicaOps is what adding and removing a replica does to the world,
// apart from what it records in state.json and replicas.yml. A seam so the
// order of those steps, and what is undone when one fails, can be tested
// without Docker.
type replicaOps interface {
	isRunning(container string) (bool, error)
	// compose runs `docker compose -f ... <args>` against the deployment.
	compose(args ...string) error
	waitReady(url string) error
	// reload makes the running proxy route to c's backends.
	reload(c proxy.Config) error
	// writeUpstreams only rewrites the file, when there is no proxy to reload.
	writeUpstreams(c proxy.Config) error
	waitDrained()
	// portFree says whether nothing on this machine listens on the port.
	portFree(port int) error
	// save records a service's slots in state.json.
	save(service string, slots []state.Slot) error
	// writeReplicas rewrites replicas.yml for these replicas.
	writeReplicas(auth, edge []replica) error
}

// replicaScaler holds one add/remove run.
type replicaScaler struct {
	st          *state.State
	topo        topology.Topology
	hostNetwork bool
	ops         replicaOps
	out         func(format string, args ...any)

	// slots are the slots each scalable service has now, kept in step with
	// state.json as the run goes.
	slots map[string][]state.Slot
}

func newReplicaScaler(st *state.State, topo topology.Topology, ops replicaOps) *replicaScaler {
	s := &replicaScaler{st: st, topo: topo, hostNetwork: st.Target == "vps", ops: ops,
		out: func(f string, a ...any) { fmt.Printf(f, a...) }, slots: map[string][]state.Slot{}}
	for _, svc := range []string{AuthService, EdgeService} {
		s.slots[svc] = st.ActiveSlots(svc)
	}
	return s
}

// layout builds the replicas of auth and edge for the given slots (central
// is always its single slot-1 replica).
func (s *replicaScaler) layout(slots map[string][]state.Slot) (central, auth, edge []replica, err error) {
	if central, err = replicasOf(s.topo, CentralService, []state.Slot{{N: 1, Version: s.st.Version}}); err != nil {
		return
	}
	if auth, err = replicasOf(s.topo, AuthService, slots[AuthService]); err != nil {
		return
	}
	edge, err = replicasOf(s.topo, EdgeService, slots[EdgeService])
	return
}

func (s *replicaScaler) proxyConfig(auth, edge []replica) proxy.Config {
	return proxy.Config{Mode: s.st.ProxyMode, Auth: proxyBackends(auth), Edge: proxyBackends(edge)}
}

// record makes slots the service's slots, in state.json and replicas.yml
// both: state first (it is the intent, and what `up` converges to), then the
// file compose reads.
func (s *replicaScaler) record(service string, slots []state.Slot) error {
	next := map[string][]state.Slot{AuthService: s.slots[AuthService], EdgeService: s.slots[EdgeService]}
	next[service] = slots
	_, auth, edge, err := s.layout(next)
	if err != nil {
		return err
	}
	if err := s.ops.save(service, slots); err != nil {
		return fmt.Errorf("couldn't record the replicas in state.json: %w", err)
	}
	if err := s.ops.writeReplicas(auth, edge); err != nil {
		return err
	}
	s.slots[service] = slots
	return nil
}

func contains(ns []int, n int) bool {
	for _, x := range ns {
		if x == n {
			return true
		}
	}
	return false
}

// hostPortsOf are the ports a replica takes on the host: its listeners
// on a host network (vps), the host ports it publishes on a bridge (local;
// ranges and ports Compose picks are not one port to look at).
func hostPortsOf(r replica, hostNetwork bool) []int {
	var ports []int
	if hostNetwork || r.Ports.HostNetwork() {
		for _, l := range r.listeners() {
			ports = append(ports, l.Port)
		}
		return ports
	}
	for _, p := range r.Ports.Publications() {
		if !p.Binding.IsRange() {
			ports = append(ports, p.Binding.Port)
		}
	}
	return ports
}

func slotNumbers(slots []state.Slot) []int {
	ns := make([]int, len(slots))
	for i, sl := range slots {
		ns[i] = sl.N
	}
	sort.Ints(ns)
	return ns
}

// freeSlots returns the count lowest slot numbers the service does not use.
func freeSlots(have []state.Slot, count int) ([]int, error) {
	used := map[int]bool{}
	for _, sl := range have {
		used[sl.N] = true
	}
	var free []int
	for n := 1; n <= topology.MaxSlot && len(free) < count; n++ {
		if !used[n] {
			free = append(free, n)
		}
	}
	if len(free) < count {
		return nil, fmt.Errorf("can't add %d: it has %d replicas and at most %d are supported", count, len(have), topology.MaxSlot)
	}
	return free, nil
}

// highestSlots returns the count highest slot numbers, highest first.
// The last replica of a service cannot be removed.
func highestSlots(have []state.Slot, count int) ([]int, error) {
	if count >= len(have) {
		return nil, fmt.Errorf("can't remove %d: it has %d replicas and at least one has to stay", count, len(have))
	}
	ns := slotNumbers(have)
	out := make([]int, 0, count)
	for i := len(ns) - 1; len(out) < count; i-- {
		out = append(out, ns[i])
	}
	return out, nil
}

func withSlot(slots []state.Slot, n int, version string) []state.Slot {
	return append(append([]state.Slot(nil), slots...), state.Slot{N: n, Version: version})
}

func withoutSlot(slots []state.Slot, n int) []state.Slot {
	var out []state.Slot
	for _, sl := range slots {
		if sl.N != n {
			out = append(out, sl)
		}
	}
	return out
}

// add starts count more replicas of service, one at a time. Each is
// waited for before the next, and put into the proxy's upstream only once
// it is ready, so no request is ever sent to a replica that cannot answer.
//
// A replica that does not come up is stopped and taken out of state.json
// and replicas.yml again; the ones before it stay. The returned error says
// how many were added.
func (s *replicaScaler) add(service string, count int) error {
	have := s.slots[service]
	numbers, err := freeSlots(have, count)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// Everything that can be known beforehand is checked for the final
	// layout, before anything changes: nothing is started if the last of
	// the replicas could not be.
	final := have
	for _, n := range numbers {
		final = withSlot(final, n, s.st.Version)
	}
	final2 := map[string][]state.Slot{AuthService: s.slots[AuthService], EdgeService: s.slots[EdgeService]}
	final2[service] = final
	central, auth, edge, err := s.layout(final2)
	if err != nil {
		return err
	}
	ready, err := checkTopology(s.topo, s.hostNetwork, deployedProxyReservations(s.st), central, auth, edge)
	if err != nil {
		return err
	}

	// What cannot be known from the compose file: the host's own ports.
	// A foreign process on one would only show as a replica that never
	// becomes ready.
	for _, group := range [][]replica{auth, edge} {
		for _, r := range group {
			if r.Base != service || !contains(numbers, r.Slot) {
				continue
			}
			if err := checkReplicable(r); err != nil {
				return err
			}
			for _, port := range hostPortsOf(r, s.hostNetwork) {
				if err := s.ops.portFree(port); err != nil {
					return fmt.Errorf("%s needs port %d on this machine: %w", r.Service, port, err)
				}
			}
		}
	}

	added := 0
	for _, n := range numbers {
		svc := topology.SlotService(service, n)
		next := withSlot(have, n, s.st.Version)
		if err := s.record(service, next); err != nil {
			// Whatever was written is put back, so state.json and
			// replicas.yml do not disagree about this slot.
			_ = s.record(service, have)
			return s.partial(service, added, err)
		}
		s.out("Starting %s...\n", svc)
		if err := s.ops.compose("up", "-d", "--no-deps", svc); err != nil {
			return s.partial(service, added, s.undo(service, svc, have, fmt.Errorf("couldn't start %s: %w", svc, err)))
		}
		if err := s.ops.waitReady(ready[svc]); err != nil {
			return s.partial(service, added, s.undo(service, svc, have, fmt.Errorf("%s never became ready: %w", svc, err)))
		}
		have = next
		added++

		// Ready: now it takes traffic.
		_, a, e, err := s.layout(s.slots)
		if err != nil {
			return s.partial(service, added, err)
		}
		if err := s.route(s.proxyConfig(a, e)); err != nil {
			return s.partial(service, added, fmt.Errorf("%s is running, but the proxy does not route to it yet: %w -- `versola up` fixes that", svc, err))
		}
		s.out("%s is ready and takes traffic.\n", svc)
	}
	return nil
}

// undo stops and removes a replica that did not come up and puts the
// recorded slots back. It returns cause, with what could not be undone
// appended.
func (s *replicaScaler) undo(service, svc string, previous []state.Slot, cause error) error {
	var left []string
	// Before the file is rewritten: compose only knows the service while it
	// is still in replicas.yml.
	if err := s.ops.compose("stop", "-t", stopTimeoutSeconds, svc); err != nil {
		left = append(left, fmt.Sprintf("couldn't stop %s: %v", svc, err))
	}
	if err := s.ops.compose("rm", "-f", svc); err != nil {
		left = append(left, fmt.Sprintf("couldn't remove %s: %v", svc, err))
	}
	if err := s.record(service, previous); err != nil {
		left = append(left, fmt.Sprintf("couldn't restore the recorded replicas: %v", err))
	}
	if len(left) > 0 {
		return fmt.Errorf("%w (and cleaning up: %s)", cause, strings.Join(left, "; "))
	}
	return cause
}

func (s *replicaScaler) partial(service string, added int, err error) error {
	if added == 0 {
		return err
	}
	return fmt.Errorf("added %d replica(s) of %s before this failed: %w", added, service, err)
}

// route makes the proxy use c's upstreams: reloaded when it is running,
// otherwise only the file changes and the next `up` starts it with it.
func (s *replicaScaler) route(c proxy.Config) error {
	running, err := s.ops.isRunning(proxy.ContainerFor(s.st.ProxyMode))
	if err != nil {
		return err
	}
	if !running {
		return s.ops.writeUpstreams(c)
	}
	return s.ops.reload(c)
}

// remove stops count replicas of service, the highest slots first, one at
// a time: out of the proxy's upstream first, then -- once the requests
// they were serving are finished -- stopped and removed, and only then
// taken out of state.json and replicas.yml. Compose can address a replica
// only while replicas.yml still defines it, so that file goes last.
func (s *replicaScaler) remove(service string, count int) error {
	have := s.slots[service]
	numbers, err := highestSlots(have, count)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	for i, n := range numbers {
		svc := topology.SlotService(service, n)
		rest := withoutSlot(have, n)

		remaining := map[string][]state.Slot{AuthService: s.slots[AuthService], EdgeService: s.slots[EdgeService]}
		remaining[service] = rest
		_, a, e, err := s.layout(remaining)
		if err != nil {
			return s.removed(service, i, err)
		}
		s.out("Taking %s out of the proxy...\n", svc)
		running, err := s.ops.isRunning(proxy.ContainerFor(s.st.ProxyMode))
		if err != nil {
			return s.removed(service, i, err)
		}
		if err := s.route(s.proxyConfig(a, e)); err != nil {
			return s.removed(service, i, fmt.Errorf("couldn't take %s out of the proxy: %w", svc, err))
		}
		if running {
			s.ops.waitDrained()
		}

		s.out("Stopping %s...\n", svc)
		if err := s.ops.compose("stop", "-t", stopTimeoutSeconds, svc); err != nil {
			return s.removed(service, i, fmt.Errorf("couldn't stop %s: %w -- it is out of the proxy but still recorded; run the command again", svc, err))
		}
		if err := s.ops.compose("rm", "-f", svc); err != nil {
			return s.removed(service, i, fmt.Errorf("couldn't remove %s: %w -- it is stopped but still recorded; run the command again", svc, err))
		}
		if err := s.record(service, rest); err != nil {
			return s.removed(service, i+1, err)
		}
		have = rest
		s.out("%s removed.\n", svc)
	}
	return nil
}

func (s *replicaScaler) removed(service string, done int, err error) error {
	if done == 0 {
		return err
	}
	return fmt.Errorf("removed %d replica(s) of %s before this failed: %w", done, service, err)
}

// ---- the real thing ----

type dockerReplicaOps struct {
	dir, composePath string
	mode             string
}

func (o dockerReplicaOps) isRunning(c string) (bool, error) { return docker.IsRunning(c) }
func (o dockerReplicaOps) compose(args ...string) error {
	return docker.Run(state.ComposeArgs(o.composePath, args...)...)
}
func (o dockerReplicaOps) waitReady(url string) error  { return wait.ForReady(url, 60*time.Second) }
func (o dockerReplicaOps) reload(c proxy.Config) error { return proxy.Reload(o.dir, c) }
func (o dockerReplicaOps) writeUpstreams(c proxy.Config) error {
	return proxy.WriteUpstreams(o.dir, c)
}
func (o dockerReplicaOps) portFree(port int) error {
	if res := checks.PortFree(port, ""); !res.OK {
		return fmt.Errorf("%s", res.Detail)
	}
	return nil
}
func (o dockerReplicaOps) waitDrained() { proxy.WaitDrained(o.mode) }
func (o dockerReplicaOps) save(service string, slots []state.Slot) error {
	return state.SaveSlots(service, slots)
}
func (o dockerReplicaOps) writeReplicas(auth, edge []replica) error {
	return writeReplicasFile(o.dir, filepath.Base(o.composePath), auth, edge)
}

// CheckReplicaRequest is everything about a `replica add|remove` that can be
// refused without touching anything -- so a vps operator is not asked to
// confirm something that was never going to run.
func CheckReplicaRequest(st *state.State, service string, count int, adding bool) error {
	if st == nil {
		return fmt.Errorf("nothing has been configured yet -- run `versola bootstrap local <version>` first")
	}
	if !Scalable(service) {
		return fmt.Errorf("%q can't have replicas: only %s and %s can", service, AuthService, EdgeService)
	}
	if count < 1 {
		return fmt.Errorf("the number of replicas must be at least 1")
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) -- configure it again")
	}
	if st.ProxyMode == "" || !proxy.HasUpstreamsFile(filepath.Dir(composePath)) {
		return fmt.Errorf("this deployment's reverse proxy config predates replicas -- run `versola configure` again")
	}
	have := st.ActiveSlots(service)
	if adding {
		_, err = freeSlots(have, count)
	} else {
		_, err = highestSlots(have, count)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	return nil
}

// centralRunning asks Compose whether central is running, by service: its
// container may have no fixed name.
func centralRunning(composePath string) (bool, error) {
	out, err := docker.Output(state.ComposeArgs(composePath, "ps", "-q", CentralService)...)
	if err != nil {
		return false, fmt.Errorf("couldn't ask Compose whether central is running: %w", err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// openReplicaScaler returns a scaler for the deployment. The caller holds
// state.Lock and has run CheckReplicaRequest.
func openReplicaScaler(st *state.State, service string, needRunning bool) (*replicaScaler, error) {
	composePath, _, err := st.ComposeFilePath()
	if err != nil {
		return nil, err
	}
	if needRunning {
		// central is what every replica needs before it can start.
		running, err := centralRunning(composePath)
		if err != nil {
			return nil, err
		}
		if !running {
			return nil, fmt.Errorf("the deployment isn't running (central is not) -- run `versola up` first")
		}
	}
	topo, err := loadTopology(composePath)
	if err != nil {
		return nil, err
	}
	ops := dockerReplicaOps{dir: filepath.Dir(composePath), composePath: composePath, mode: st.ProxyMode}
	return newReplicaScaler(st, topo, ops), nil
}

// AddReplicas starts count more replicas of auth or edge. The caller holds
// state.Lock.
func AddReplicas(st *state.State, service string, count int) error {
	if err := CheckReplicaRequest(st, service, count, true); err != nil {
		return err
	}
	if err := requireOverrideSupport(); err != nil {
		return err
	}
	s, err := openReplicaScaler(st, service, true)
	if err != nil {
		return err
	}
	return s.add(service, count)
}

// RemoveReplicas stops count replicas of auth or edge, highest slots first.
// The caller holds state.Lock.
func RemoveReplicas(st *state.State, service string, count int) error {
	if err := CheckReplicaRequest(st, service, count, false); err != nil {
		return err
	}
	s, err := openReplicaScaler(st, service, false)
	if err != nil {
		return err
	}
	return s.remove(service, count)
}

// refuseWithReplicas stops a configure on a deployment that has replicas
// beyond slot 1. Configure starts a new record with one replica of each, so
// the extra ones would keep running with nothing managing them, from a
// bundle that is about to be deleted.
func refuseWithReplicas() error {
	st, err := state.Load()
	if err != nil {
		return nil // nothing configured, or unreadable: configure reports that itself
	}
	return replicasBlockConfigure(st)
}

func replicasBlockConfigure(st *state.State) error {
	var extra []string
	for _, svc := range []string{AuthService, EdgeService} {
		if n := len(st.ActiveSlots(svc)); n > 1 {
			extra = append(extra, fmt.Sprintf("%s has %d replicas", svc, n))
		}
	}
	if len(extra) == 0 {
		return nil
	}
	return fmt.Errorf("this deployment runs more than one replica (%s) -- remove the extra ones first, with `versola replica remove auth|edge <count>`, then configure again", strings.Join(extra, ", "))
}

// ReplicaSummary is the replicas a deployment records, for `status`:
// "Replicas: auth x2 (slots 1, 2), edge x1 (slot 1)".
func ReplicaSummary(st *state.State) string {
	var parts []string
	for _, svc := range []string{AuthService, EdgeService} {
		ns := slotNumbers(st.ActiveSlots(svc))
		strs := make([]string, len(ns))
		for i, n := range ns {
			strs[i] = fmt.Sprint(n)
		}
		word := "slot"
		if len(ns) > 1 {
			word = "slots"
		}
		parts = append(parts, fmt.Sprintf("%s x%d (%s %s)", svc, len(ns), word, strings.Join(strs, ", ")))
	}
	return "Replicas: " + strings.Join(parts, ", ")
}

// minComposeForOverride is the first Docker Compose with the `!override`
// tag replicas.yml uses to replace the base service's ports.
var minComposeForOverride = [2]int{2, 24}

// requireOverrideSupport fails early, in words, on a Compose too old for
// replicas.yml; otherwise every compose command fails opaquely once the
// file exists. A version that cannot be read is let through.
func requireOverrideSupport() error {
	out, err := docker.Output("compose", "version", "--short")
	if err != nil {
		return nil // the compose commands that follow report it
	}
	major, minor, ok := parseComposeVersion(string(out))
	if !ok {
		return nil
	}
	if major < minComposeForOverride[0] || (major == minComposeForOverride[0] && minor < minComposeForOverride[1]) {
		return fmt.Errorf("Docker Compose %d.%d is too old for replicas (needs %d.%d or newer) -- update Docker Compose", major, minor, minComposeForOverride[0], minComposeForOverride[1])
	}
	return nil
}

// parseComposeVersion reads "2.29.1", "v5.0.2" or "5.0.2-desktop.1".
func parseComposeVersion(s string) (major, minor int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	var err1, err2 error
	major, err1 = strconv.Atoi(parts[0])
	minor, err2 = strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}
