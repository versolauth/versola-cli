package deploy

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// A rolling upgrade (`versola rollout <version>`) moves a running vps
// deployment to another Versola version without taking it down:
//
//	prepare  the new version's bundle (a staged configure: versola-tools,
//	         the secrets plan, replicas.yml) -- nothing running changes
//	migrate  the database (additive migrations: the old version keeps
//	         working on the new schema -- the release contract)
//	central  stopped and started on the new version (not replicated: the
//	         admin console and self-registration are down until it is
//	         ready; auth and edge keep serving from their caches)
//	auth     a stand-in replica of the new version in a free slot, then
//	         every replica in turn: out of the proxy, drained, recreated
//	         on the new version, ready, back in; then the stand-in goes
//	edge     the same
//	finish   the proxy on the new bundle, the old bundle removed
//
// There is no journal of how far it got. Progress is read back from what is
// recorded in state.json (each slot's version, the stand-in marker Surge,
// PreviousVersion, MigratedAt) checked against the containers themselves --
// the image each one was created from -- so a rollout cut off anywhere
// (Ctrl-C, a dropped SSH session, a failed step) is continued by running the
// same command again. Every step below is written to be repeated: it looks
// at what is there before it acts.
//
// New containers are created by `docker compose up -d --no-deps <service>`
// from the new bundle, so they come up on the new version directly; the old
// ones are only ever stopped (Compose recreates a container whose definition
// changed). Always --no-deps (central is up, and nothing else may be
// recreated as a side effect); never --remove-orphans.

// errInterrupted is a rollout stopped between two steps at the operator's
// request.
var errInterrupted = errors.New("stopped at your request, between two steps -- run the same `versola rollout` again to continue")

// standInWarmup is how long a replica is left after it first reports ready
// before it is probed and put into the proxy: readiness says the process is
// up, not that its caches are filled and its JIT has seen a request.
const standInWarmup = 5 * time.Second

// rolloutOps is what a rollout does to the world, besides what replicaOps
// already covers (compose, readiness, proxy). A seam, like replicaOps, so the
// order of the steps and how a cut-off rollout is picked up again can be
// tested without Docker.
type rolloutOps interface {
	replicaOps
	// inspect describes the container of a compose service (see
	// containerInfo). It finds the container by its compose labels, so it
	// works for a service no compose file names any more.
	inspect(service string) (containerInfo, error)
	// waitReadyWithin is waitReady with a limit of its own.
	waitReadyWithin(url string, timeout time.Duration) error
	// readyNow asks a readiness URL up to three times (about 11 s at worst): one
	// slow answer (a GC pause) must not take a healthy replica out.
	readyNow(url string) bool
	// pull fetches the new version's images before anything changes, so
	// central's restart and a stand-in's start don't include the download,
	// and a registry that can't be reached stops the rollout before the
	// migrations.
	pull() error
	// warm waits standInWarmup, then until the replica answers its probe
	// several times in a row (see probeReplica).
	warm(r replica) error
	// removeContainer stops (`docker stop -t 20`) and removes the container of
	// a compose service, found by its compose labels -- `compose stop/rm`
	// only know services of the compose files, and a stand-in is taken out
	// of replicas.yml once it is gone. No container: nothing to do.
	removeContainer(service string) error
	// migrate applies the new version's migrations and records MigratedAt.
	migrate() error
	// standInCheck refuses when the host has not the free memory, or
	// Postgres not the connections, for a stand-in of svc now (see
	// rollout_preflight.go).
	standInCheck(st *state.State, svc string) error
	// markStarting and markRunning: state.MarkStarting / MarkRunning.
	markStarting() error
	markRunning() error
	// update changes state.json: read, f, written.
	update(f func(*state.State)) error
}

// containerInfo is what the rollout needs to know about a container.
type containerInfo struct {
	found, running bool
	// image is the reference it was created from.
	image string
	// workDir is the compose project directory it was created from (the
	// label com.docker.compose.project.working_dir): its bundle. "" when
	// the label is missing.
	workDir string
}

// rolloutEngine holds one run of a rollout whose new bundle is prepared.
type rolloutEngine struct {
	// st mirrors state.json: every change goes through update, which writes
	// it there first.
	st   *state.State
	topo topology.Topology
	// bundleDir is the new version's bundle: where a container of the new
	// version was created from.
	bundleDir string
	ops       rolloutOps
	out       func(format string, args ...any)
	fronts    proxy.Fronts
	genDir    string

	// surge: start a stand-in replica before replacing a service's replicas.
	surge bool
	// allowDowntime: a service with one replica and no stand-in is
	// recreated in place, unreachable until it is ready.
	allowDowntime bool
	// interrupted reports whether the operator asked to stop; checked
	// between steps, never in the middle of one.
	interrupted func() bool

	// running and inUp, per scalable service and slot: whether its container
	// runs, and whether the proxy routes to it. Set by reconcile, kept in step
	// as the run goes.
	running map[string]map[int]bool
	inUp    map[string]map[int]bool
	// current, per scalable service and slot: whether its container is the
	// target's -- its image, created from this bundle (isNew). What has to be
	// replaced is decided by it, not by the version recorded: a container of
	// the right version from another bundle (an abort's untouched replicas,
	// which still mount the bundle the end of the run deletes) is recorded
	// at the version it runs and still has to be recreated.
	current map[string]map[int]bool
}

func (e *rolloutEngine) say(format string, args ...any) { e.out(format, args...) }

func (e *rolloutEngine) checkpoint() error {
	if e.interrupted != nil && e.interrupted() {
		return errInterrupted
	}
	return nil
}

func (e *rolloutEngine) update(f func(*state.State)) error {
	if err := e.ops.update(f); err != nil {
		return fmt.Errorf("couldn't record the rollout's progress in state.json: %w", err)
	}
	f(e.st)
	return nil
}

// layout is the replicas as recorded: central's single one and auth's (both
// behind the proxy's fronts), and edge's.
func (e *rolloutEngine) layout() (central, auth, edge []replica, err error) {
	if central, err = replicasOf(e.topo, CentralService, []state.Slot{{N: 1, Version: e.st.Version}}); err != nil {
		return
	}
	if central, err = frontCentral(central, true); err != nil {
		return
	}
	if auth, err = replicasOf(e.topo, AuthService, e.st.ActiveSlots(AuthService)); err != nil {
		return
	}
	if auth, err = frontAuth(auth, true); err != nil {
		return
	}
	edge, err = replicasOf(e.topo, EdgeService, e.st.ActiveSlots(EdgeService))
	return
}

func (e *rolloutEngine) replicaOf(svc string, n int) (replica, error) {
	_, auth, edge, err := e.layout()
	if err != nil {
		return replica{}, err
	}
	group := edge
	if svc == AuthService {
		group = auth
	}
	for _, r := range group {
		if r.Slot == n {
			return r, nil
		}
	}
	return replica{}, fmt.Errorf("%s has no slot %d recorded", svc, n)
}

func (e *rolloutEngine) readyURL(r replica) (string, error) {
	return readinessURL(true, r.Ports)
}

// expectedImage is the image the new bundle's compose file gives a service:
// what a container of the new version was created from.
func (e *rolloutEngine) expectedImage(base string) (string, error) {
	svc, err := e.topo.Service(base)
	if err != nil {
		return "", err
	}
	if svc.Image() == "" {
		return "", fmt.Errorf("the compose file names no image for %s, so its containers' versions can't be told apart", base)
	}
	return svc.Image(), nil
}

// isNew reports whether a container is the new version's: created from the
// new bundle's image -- and from the new bundle, when Compose says which
// (two releases could name the same image; a container of the old bundle
// still mounts files the end of the rollout deletes).
func (e *rolloutEngine) isNew(base string, c containerInfo) (bool, error) {
	want, err := e.expectedImage(base)
	if err != nil {
		return false, err
	}
	if !c.found || c.image != want {
		return false, nil
	}
	if c.workDir == "" || e.bundleDir == "" {
		// Compose did not say which bundle it came from. Going forward the
		// image tells versions apart; going back (an abort) the replicas that
		// never moved have the target's image already, but mount the bundle
		// the end of the run deletes -- so there it is not taken as new.
		return !e.st.RollingBack, nil
	}
	return filepath.Clean(c.workDir) == filepath.Clean(e.bundleDir), nil
}

// writeFiles rewrites replicas.yml and overrides.yml from the record: what
// compose reads to know every slot -- a stand-in included -- and central's
// moved PORT/DPORT and auth's moved APORT/MPORT.
func (e *rolloutEngine) writeFiles() error {
	central, auth, edge, err := e.layout()
	if err != nil {
		return err
	}
	return e.ops.writeReplicas(central, auth, edge)
}

// route has the proxy route to exactly the slots in inUp.
func (e *rolloutEngine) route() error {
	central, auth, edge, err := e.layout()
	if err != nil {
		return err
	}
	pick := func(svc string, all []replica) []replica {
		var out []replica
		for _, r := range all {
			if e.inUp[svc][r.Slot] {
				out = append(out, r)
			}
		}
		return out
	}
	c := routedConfig(e.st.ProxyMode, e.fronts, e.genDir, central, pick(AuthService, auth), pick(EdgeService, edge))
	return routeVia(e.ops, e.st.ProxyMode, c)
}

// addToProxy puts a slot into the proxy's upstreams. If that fails, the
// previous upstreams are put back -- a failure after nginx has reloaded
// (its fronts not answering, say) would otherwise leave nginx routing to the
// slot while this run thinks it doesn't. Whether the slot may now be
// stopped is the caller's to decide from the error: it may not.
func (e *rolloutEngine) addToProxy(svc string, n int) error {
	if e.inUp[svc][n] {
		return nil
	}
	e.inUp[svc][n] = true
	if err := e.route(); err != nil {
		delete(e.inUp[svc], n)
		if rerr := e.route(); rerr != nil {
			return fmt.Errorf("%w (and routing back to the previous replicas failed too: %v)", err, rerr)
		}
		return err
	}
	return nil
}

// takeOutOfProxy takes a slot out of the proxy's upstreams and waits until
// the requests already sent to it are answered.
func (e *rolloutEngine) takeOutOfProxy(svc string, n int) error {
	if !e.inUp[svc][n] {
		return nil
	}
	rest := 0
	for m, in := range e.inUp[svc] {
		if in && m != n {
			rest++
		}
	}
	if rest == 0 {
		return fmt.Errorf("taking %s out of the proxy would leave it no replica to route %s to", topology.SlotService(svc, n), svc)
	}
	delete(e.inUp[svc], n)
	if err := e.route(); err != nil {
		e.inUp[svc][n] = true
		_ = e.route() // best effort: back to routing to it, as before
		return err
	}
	running, err := e.ops.isRunning(proxy.ContainerFor(e.st.ProxyMode))
	if err != nil {
		return err
	}
	if running {
		e.ops.waitDrained()
	}
	return nil
}

// reconcile reads back where the rollout stands, from the containers: each
// recorded slot's version from the image its container was created from
// (a cut-off run may have recreated one without recording it), whether it
// runs, and whether it is ready. The proxy is then made to route to exactly
// the ready slots -- a run cut off between taking a slot out of the proxy
// and recreating it would otherwise leave that slot out for good.
func (e *rolloutEngine) reconcile() error {
	e.running = map[string]map[int]bool{}
	e.inUp = map[string]map[int]bool{}
	e.current = map[string]map[int]bool{}
	for _, svc := range []string{AuthService, EdgeService} {
		e.running[svc], e.inUp[svc], e.current[svc] = map[int]bool{}, map[int]bool{}, map[int]bool{}
		slots := e.st.ActiveSlots(svc)
		changed := false
		for i, sl := range slots {
			c, err := e.ops.inspect(topology.SlotService(svc, sl.N))
			if err != nil {
				return err
			}
			isNew, err := e.isNew(svc, c)
			if err != nil {
				return err
			}
			e.running[svc][sl.N] = c.found && c.running
			e.current[svc][sl.N] = isNew
			want, err := e.expectedImage(svc)
			if err != nil {
				return err
			}
			switch {
			case isNew && sl.Version != e.st.Version:
				slots[i].Version, changed = e.st.Version, true
			case c.found && c.image != want && sl.Version == e.st.Version && e.st.PreviousVersion != "":
				// Recorded as moved on, but its container is not of the
				// target's image: it was never recreated. (One of the target's
				// image from another bundle keeps its record and is replaced
				// all the same: current is false.)
				slots[i].Version, changed = e.st.PreviousVersion, true
			}
		}
		if changed {
			if err := e.update(func(s *state.State) { s.SetSlots(svc, slots) }); err != nil {
				return err
			}
		}
	}
	_, auth, edge, err := e.layout()
	if err != nil {
		return err
	}
	for _, group := range [][]replica{auth, edge} {
		for _, r := range group {
			if !e.running[r.Base][r.Slot] {
				continue
			}
			url, err := e.readyURL(r)
			if err != nil {
				return err
			}
			if e.ops.readyNow(url) {
				e.inUp[r.Base][r.Slot] = true
			}
		}
	}
	for _, svc := range []string{AuthService, EdgeService} {
		if len(e.inUp[svc]) == 0 {
			// Nothing of it answers: route to every recorded slot, as `up`
			// would, rather than refuse -- the steps below bring them up.
			e.say("Warning: no %s replica answers its readiness check right now.\n", svc)
			for _, sl := range e.st.ActiveSlots(svc) {
				e.inUp[svc][sl.N] = true
			}
		}
	}
	return e.route()
}

// run carries a prepared rollout through to the end, from wherever it
// stands.
func (e *rolloutEngine) run() error {
	if err := e.checkpoint(); err != nil {
		return err
	}
	// What compose reads, from the record: a run cut off after recording a
	// stand-in and before writing the file would leave compose not knowing it.
	if err := e.writeFiles(); err != nil {
		return err
	}
	e.say("\nPulling the images of %s...\n", e.st.Version)
	if err := e.ops.pull(); err != nil {
		if e.st.MigratedAt == nil {
			return fmt.Errorf("couldn't pull the images of %s -- nothing running was changed: %w", e.st.Version, err)
		}
		return fmt.Errorf("couldn't pull the images of %s: %w -- run the same `versola rollout` again", e.st.Version, err)
	}
	if err := e.checkpoint(); err != nil {
		return err
	}
	if e.st.MigratedAt == nil {
		e.say("\nApplying the database migrations of %s...\n", e.st.Version)
		if err := e.ops.migrate(); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := e.update(func(s *state.State) { s.MigratedAt = &now }); err != nil {
			return err
		}
	}
	if err := e.checkpoint(); err != nil {
		return err
	}
	// From here on containers are started from the new bundle: it must not
	// be deleted, even if this run stops halfway (state.MarkStarting).
	if err := e.ops.markStarting(); err != nil {
		return fmt.Errorf("couldn't record the deployment being started: %w", err)
	}
	if err := e.central(); err != nil {
		return err
	}
	e.say("\nChecking where auth and edge stand...\n")
	if err := e.reconcile(); err != nil {
		return err
	}
	for _, svc := range []string{AuthService, EdgeService} {
		if err := e.service(svc); err != nil {
			return err
		}
	}
	return e.finish()
}

// central restarts central on the new version unless its container already
// is, and waits until it is ready: auth and edge wait for central when they
// start, and hang rather than fail if it is not there. It is the one step
// where something is down (the admin console, self-registration), so it says
// how long that lasted, and waits longer than for a replica
// (centralReadyTimeout): a rollout stopped here costs more to undo than to
// wait out.
func (e *rolloutEngine) central() error {
	c, err := e.ops.inspect(CentralService)
	if err != nil {
		return err
	}
	isNew, err := e.isNew(CentralService, c)
	if err != nil {
		return err
	}
	central, _, _, err := e.layout()
	if err != nil {
		return err
	}
	url, err := e.readyURL(central[0])
	if err != nil {
		return err
	}
	restarted := !isNew || !c.running
	var stoppedAt time.Time
	if restarted {
		if err := e.checkpoint(); err != nil {
			return err
		}
		e.say("\nRestarting central on %s -- the admin console and self-registration are unavailable until it is ready; auth and edge keep serving...\n", e.st.Version)
		stoppedAt = time.Now() // the old container is stopped inside this call
		if err := e.ops.compose("up", "-d", "--no-deps", CentralService); err != nil {
			return fmt.Errorf("couldn't start central on %s: %w", e.st.Version, err)
		}
	}
	e.say("Waiting for central to be ready (up to %s)...\n", centralReadyTimeout)
	if err := e.ops.waitReadyWithin(url, centralReadyTimeout); err != nil {
		hint := "run the same `versola rollout` again: it only waits for central again and carries on. `versola rollout --abort` goes back as well, but recreates every auth and edge replica"
		if e.st.RollingBack {
			hint = "run `versola rollout --abort` again: it only waits for central again and carries on"
		}
		return fmt.Errorf("central never became ready on %s (waited %s): %w -- auth and edge still run from their caches. Look at the logs of central (`docker logs`); once it is up, or if it was only slow, %s",
			e.st.Version, centralReadyTimeout, err, hint)
	}
	if restarted {
		e.say("central is ready on %s: the admin console and self-registration were unavailable for about %s.\n", e.st.Version, time.Since(stoppedAt).Round(time.Second))
	}
	return nil
}

// service moves every replica of svc to the new version: stand-in first,
// then each old slot, then the stand-in goes.
func (e *rolloutEngine) service(svc string) error {
	target := e.st.Version
	standIn, hasStandIn := e.st.Surge[svc]
	var old, pending []int
	for _, sl := range e.st.ActiveSlots(svc) {
		if hasStandIn && sl.N == standIn {
			continue
		}
		switch {
		case !e.current[svc][sl.N]:
			old = append(old, sl.N)
		case !e.inUp[svc][sl.N]:
			// On the new version but not serving: recreated by a run that
			// was cut off, or one that did not become ready.
			pending = append(pending, sl.N)
		}
	}
	sort.Ints(old)
	sort.Ints(pending)
	if hasStandIn && !e.current[svc][standIn] {
		// A stand-in that is not the target's: the one a rollout started on
		// the version an abort is leaving. It is not recreated in place (it
		// takes traffic); it is retired, and a stand-in of the target started
		// below if one is wanted. The replicas out of the proxy (one recreated
		// on the version being left that never became ready) are moved
		// first: nothing serves through them, so they need no stand-in, and
		// the stand-in may be all that serves right now.
		var rest []int
		for _, n := range old {
			if e.inUp[svc][n] {
				rest = append(rest, n)
				continue
			}
			if err := e.replace(svc, n); err != nil {
				return err
			}
		}
		old = rest
		// Started already by an earlier run but not serving yet: brought
		// up for the same reason.
		for _, n := range pending {
			if err := e.bringUp(svc, n, topology.SlotService(svc, n)); err != nil {
				return err
			}
		}
		pending = nil
		e.say("\n%s: the stand-in %s is not on %s -- retiring it.\n", svc, topology.SlotService(svc, standIn), target)
		if err := e.retireStandIn(svc, standIn); err != nil {
			return err
		}
		hasStandIn = false
	}
	if len(old) == 0 && len(pending) == 0 && !hasStandIn {
		e.say("\n%s: every replica runs %s.\n", svc, target)
		return nil
	}
	e.say("\n%s:\n", svc)

	if len(old) > 0 || len(pending) > 0 {
		var err error
		switch {
		case hasStandIn:
			err = e.bringUp(svc, standIn, "the stand-in "+topology.SlotService(svc, standIn))
		case e.surge:
			standIn, err = e.startStandIn(svc)
			hasStandIn = err == nil
		}
		if err != nil {
			return err
		}
	}
	for _, n := range pending {
		if err := e.bringUp(svc, n, topology.SlotService(svc, n)); err != nil {
			return err
		}
	}
	for _, n := range old {
		if err := e.replace(svc, n); err != nil {
			return err
		}
	}
	if hasStandIn {
		return e.retireStandIn(svc, standIn)
	}
	return nil
}

// bringUp makes a slot recorded at the new version serve: started if its
// container is not running (compose leaves a running one of the same
// definition alone), ready, warm, in the proxy.
func (e *rolloutEngine) bringUp(svc string, n int, what string) error {
	if err := e.checkpoint(); err != nil {
		return err
	}
	r, err := e.replicaOf(svc, n)
	if err != nil {
		return err
	}
	url, err := e.readyURL(r)
	if err != nil {
		return err
	}
	e.say("Starting %s on %s...\n", what, e.st.Version)
	if err := e.ops.compose("up", "-d", "--no-deps", r.Service); err != nil {
		return fmt.Errorf("couldn't start %s: %w", r.Service, err)
	}
	e.running[svc][n], e.current[svc][n] = true, true
	if err := e.ops.waitReady(url); err != nil {
		return fmt.Errorf("%s never became ready on %s: %w -- it is not in the proxy; the other replicas serve. Run the same `versola rollout` again to retry", r.Service, e.st.Version, err)
	}
	if err := e.ops.warm(r); err != nil {
		return fmt.Errorf("%s is ready but doesn't answer requests yet: %w -- it is not in the proxy; run the same `versola rollout` again to retry", r.Service, err)
	}
	if err := e.addToProxy(svc, n); err != nil {
		return fmt.Errorf("%s is ready, but the proxy does not route to it: %w", r.Service, err)
	}
	e.say("%s takes traffic.\n", r.Service)
	return nil
}

// startStandIn starts a replica of the new version in the lowest free slot
// and puts it into the proxy, so the service keeps its capacity while each
// of its replicas is replaced. The slot is recorded as the stand-in (Surge)
// before anything is started, so a run cut off from here on knows it is not
// one of the operator's replicas.
func (e *rolloutEngine) startStandIn(svc string) (int, error) {
	if err := e.checkpoint(); err != nil {
		return 0, err
	}
	free, err := freeSlots(e.st.ActiveSlots(svc), 1)
	if err != nil {
		return 0, fmt.Errorf("%s: no free slot for a stand-in replica (%w) -- run with --no-surge", svc, err)
	}
	n := free[0]
	r, err := standInReplica(e.topo, svc, n, true)
	if err != nil {
		return 0, err
	}
	if err := e.ops.standInCheck(e.st, svc); err != nil {
		return 0, err
	}
	if _, err := e.readyURL(r); err != nil {
		return 0, err
	}
	for _, port := range hostPortsOf(r, true) {
		if err := e.ops.portFree(port); err != nil {
			return 0, fmt.Errorf("the stand-in %s needs port %d on this machine: %w", r.Service, port, err)
		}
	}
	target := e.st.Version
	if err := e.update(func(s *state.State) {
		if s.Surge == nil {
			s.Surge = map[string]int{}
		}
		s.Surge[svc] = n
		s.SetSlots(svc, withSlot(s.ActiveSlots(svc), n, target))
	}); err != nil {
		return 0, err
	}
	if err := e.writeFiles(); err != nil {
		return 0, e.dropStandIn(svc, n, err)
	}
	e.say("Starting a stand-in, %s, on %s (removed again once every %s replica runs %s)...\n", r.Service, target, svc, target)
	if err := e.ops.compose("up", "-d", "--no-deps", r.Service); err != nil {
		return 0, e.dropStandIn(svc, n, fmt.Errorf("couldn't start the stand-in %s: %w", r.Service, err))
	}
	e.running[svc][n], e.current[svc][n] = true, true
	url, _ := e.readyURL(r)
	if err := e.ops.waitReady(url); err != nil {
		return 0, e.dropStandIn(svc, n, fmt.Errorf("the stand-in %s never became ready on %s: %w -- nothing else was changed", r.Service, target, err))
	}
	if err := e.ops.warm(r); err != nil {
		return 0, e.dropStandIn(svc, n, fmt.Errorf("the stand-in %s is ready but doesn't answer requests: %w -- nothing else was changed", r.Service, err))
	}
	if err := e.addToProxy(svc, n); err != nil {
		// Not removed: nginx may route to it already (a reload that took,
		// then failed its check). It stays recorded as the stand-in; the next
		// run routes to what is ready and carries on.
		return 0, fmt.Errorf("couldn't put the stand-in %s into the proxy: %w -- it keeps running; run the same `versola rollout` again", r.Service, err)
	}
	e.say("%s takes traffic.\n", r.Service)
	return n, nil
}

// dropStandIn undoes a stand-in that did not come up: its container goes,
// then its record. If the container can't be removed the record stays, so
// the next run still knows the slot is the stand-in.
func (e *rolloutEngine) dropStandIn(svc string, n int, cause error) error {
	name := topology.SlotService(svc, n)
	if err := e.ops.removeContainer(name); err != nil {
		return fmt.Errorf("%w (and removing %s failed: %v -- it stays recorded as the stand-in; the next `versola rollout` removes it)", cause, name, err)
	}
	delete(e.running[svc], n)
	delete(e.current[svc], n)
	if err := e.update(func(s *state.State) {
		s.SetSlots(svc, withoutSlot(s.ActiveSlots(svc), n))
		delete(s.Surge, svc)
		if len(s.Surge) == 0 {
			s.Surge = nil
		}
	}); err != nil {
		return fmt.Errorf("%w (and %v)", cause, err)
	}
	if err := e.writeFiles(); err != nil {
		return fmt.Errorf("%w (and %v)", cause, err)
	}
	return cause
}

// replace moves one slot to the new version: out of the proxy, drained,
// recreated from the new bundle, ready, warm, recorded, back in. With no
// other replica to serve meanwhile (one replica, no stand-in), it is
// recreated in place -- only with allowDowntime.
func (e *rolloutEngine) replace(svc string, n int) error {
	if err := e.checkpoint(); err != nil {
		return err
	}
	r, err := e.replicaOf(svc, n)
	if err != nil {
		return err
	}
	url, err := e.readyURL(r)
	if err != nil {
		return err
	}
	others := 0
	for m, in := range e.inUp[svc] {
		if in && m != n {
			others++
		}
	}
	if others == 0 {
		if !e.allowDowntime {
			return fmt.Errorf("%s is the only %s replica serving and there is no stand-in: replacing it would leave %s unreachable -- run without --no-surge, or with --allow-downtime", r.Service, svc, svc)
		}
		e.say("Recreating %s on %s in place -- %s is unreachable until it is ready...\n", r.Service, e.st.Version, svc)
	} else {
		e.say("Taking %s out of the proxy...\n", r.Service)
		if err := e.takeOutOfProxy(svc, n); err != nil {
			return fmt.Errorf("couldn't take %s out of the proxy: %w", r.Service, err)
		}
		e.say("Recreating %s on %s...\n", r.Service, e.st.Version)
	}
	if err := e.ops.compose("up", "-d", "--no-deps", r.Service); err != nil {
		return fmt.Errorf("couldn't recreate %s on %s: %w -- the other replicas serve; run the same `versola rollout` again to retry", r.Service, e.st.Version, err)
	}
	e.running[svc][n], e.current[svc][n] = true, true
	target := e.st.Version
	// Recorded as soon as it is recreated, ready or not: its container is the
	// new version's either way (reconcile would find the same).
	if err := e.update(func(s *state.State) { s.SetSlots(svc, slotAt(s.ActiveSlots(svc), n, target)) }); err != nil {
		return err
	}
	if err := e.ops.waitReady(url); err != nil {
		if others == 0 {
			return fmt.Errorf("%s never became ready on %s: %w -- %s is unreachable; see its `docker logs`", r.Service, target, err, svc)
		}
		return fmt.Errorf("%s never became ready on %s: %w -- it is out of the proxy and the other replicas serve; see its `docker logs`, then run the same `versola rollout` again", r.Service, target, err)
	}
	if err := e.ops.warm(r); err != nil {
		return fmt.Errorf("%s is ready but doesn't answer requests yet: %w -- it is out of the proxy; run the same `versola rollout` again to retry", r.Service, err)
	}
	if others == 0 {
		// It never left the upstreams; the reload moves the proxy to the new
		// bundle's configuration.
		if err := e.route(); err != nil {
			return err
		}
	} else if err := e.addToProxy(svc, n); err != nil {
		return fmt.Errorf("%s is ready on %s, but the proxy does not route to it: %w", r.Service, target, err)
	}
	e.say("%s runs %s and takes traffic.\n", r.Service, target)
	return nil
}

// retireStandIn takes the stand-in out of the proxy, drains it, removes its
// container and only then its record.
func (e *rolloutEngine) retireStandIn(svc string, n int) error {
	if err := e.checkpoint(); err != nil {
		return err
	}
	name := topology.SlotService(svc, n)
	e.say("Removing the stand-in %s...\n", name)
	if err := e.takeOutOfProxy(svc, n); err != nil {
		return fmt.Errorf("couldn't take the stand-in %s out of the proxy: %w", name, err)
	}
	if err := e.ops.removeContainer(name); err != nil {
		return fmt.Errorf("couldn't remove the stand-in %s: %w -- it is out of the proxy; run the same `versola rollout` again", name, err)
	}
	delete(e.running[svc], n)
	delete(e.current[svc], n)
	if err := e.update(func(s *state.State) {
		s.SetSlots(svc, withoutSlot(s.ActiveSlots(svc), n))
		delete(s.Surge, svc)
		if len(s.Surge) == 0 {
			s.Surge = nil
		}
	}); err != nil {
		return err
	}
	return e.writeFiles()
}

// finish routes the proxy to every replica from the new bundle (also when no
// replica needed replacing), lets the old bundle go (MarkRunning removes it:
// nothing mounts it any more) and records the rollout as done.
func (e *rolloutEngine) finish() error {
	for _, svc := range []string{AuthService, EdgeService} {
		for _, sl := range e.st.ActiveSlots(svc) {
			if sl.Version != e.st.Version {
				return fmt.Errorf("%s slot %d is still recorded at %s -- run the same `versola rollout` again", svc, sl.N, sl.Version)
			}
			if !e.current[svc][sl.N] {
				// MarkRunning deletes every other bundle: not while a
				// container still mounts one.
				return fmt.Errorf("%s slot %d was not recreated from this run's configuration -- run the same `versola rollout` again", svc, sl.N)
			}
			e.inUp[svc][sl.N] = true
		}
	}
	if err := e.route(); err != nil {
		return err
	}
	// MarkRunning first: a run cut off between the two is still recorded as
	// under way, and the next one simply finishes again. (The other order
	// would leave a finished rollout refused as "not running from this
	// configuration".)
	if err := e.ops.markRunning(); err != nil {
		return fmt.Errorf("everything runs %s, but recording that failed: %w -- run the same `versola rollout` again", e.st.Version, err)
	}
	return e.update(func(s *state.State) { s.PreviousVersion, s.RollingBack = "", false })
}

// samePorts refuses a new version whose compose file puts the replicas that
// keep running elsewhere than the running version's: the rollout probes and
// routes every slot by the new bundle's ports, so an old replica would be
// looked for where it does not listen. Such a release is deployed with
// configure and up instead.
func samePorts(oldTopo, newTopo topology.Topology, slots map[string][]state.Slot) error {
	for _, base := range []string{CentralService, AuthService, EdgeService} {
		ss := slots[base]
		if base == CentralService {
			ss = []state.Slot{{N: 1}}
		}
		was, err := replicasOf(oldTopo, base, ss)
		if err != nil {
			return err
		}
		now, err := replicasOf(newTopo, base, ss)
		if err != nil {
			return err
		}
		for i := range was {
			a, b := was[i].Ports, now[i].Ports
			same := a.Port == b.Port && a.DiagnosticsPort == b.DiagnosticsPort
			if base == AuthService {
				same = same && a.AdditionalPort == b.AdditionalPort && a.MutualTLSPort == b.MutualTLSPort
			}
			if !same {
				return fmt.Errorf("the new version's compose file moves %s's ports (PORT/DPORT/APORT/MPORT %d/%d/%d/%d, now %d/%d/%d/%d): a rolling upgrade can't route to the running replicas by the new ports -- deploy this version with `versola configure`, `migrate` and `up` (a short stop)",
					was[i].Service, a.Port, a.DiagnosticsPort, a.AdditionalPort, a.MutualTLSPort, b.Port, b.DiagnosticsPort, b.AdditionalPort, b.MutualTLSPort)
			}
		}
	}
	return nil
}

// slotAt is slots with slot n at version.
func slotAt(slots []state.Slot, n int, version string) []state.Slot {
	out := append([]state.Slot(nil), slots...)
	for i := range out {
		if out[i].N == n {
			out[i].Version = version
		}
	}
	return out
}

// standInReplica is the replica of svc a rollout would start in slot n.
func standInReplica(topo topology.Topology, svc string, n int, hostNetwork bool) (replica, error) {
	rs, err := replicasOf(topo, svc, []state.Slot{{N: n}})
	if err != nil {
		return replica{}, err
	}
	switch svc {
	case AuthService:
		rs, err = frontAuth(rs, hostNetwork)
	case CentralService:
		rs, err = frontCentral(rs, hostNetwork)
	}
	if err != nil {
		return replica{}, err
	}
	return rs[0], nil
}

// withStandIn is the replicas of svc for slots plus the stand-in a rollout
// would start (the lowest free slot), for the port checks of its preparation.
func withStandIn(topo topology.Topology, svc string, slots []state.Slot, version string, hostNetwork bool) ([]replica, error) {
	free, err := freeSlots(slots, 1)
	if err != nil {
		return nil, fmt.Errorf("%s: no free slot for a rollout's stand-in replica (%w) -- use --no-surge", svc, err)
	}
	rs, err := replicasOf(topo, svc, withSlot(slots, free[0], version))
	if err != nil {
		return nil, err
	}
	switch svc {
	case AuthService:
		return frontAuth(rs, hostNetwork)
	case CentralService:
		return frontCentral(rs, hostNetwork)
	}
	return rs, nil
}
