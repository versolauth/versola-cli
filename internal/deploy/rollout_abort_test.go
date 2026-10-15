package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/state"
)

// abortBundleDir is the bundle an abort configures for the version it goes
// back to.
const abortBundleDir = "/home/v/.versola/active/bundle-back"

// prepareAbort turns the world a forward rollout left (finished or cut off
// anywhere) into what `rollout --abort` prepares before the engine runs:
// the record of the staged configure of the previous version (state.Finalize
// with the abort's options), and compose creating containers of that version
// from the abort's bundle.
func prepareAbort(t *testing.T, w *world) {
	t.Helper()
	st := w.load()
	if st.PreviousVersion == "" {
		t.Fatal("no rollout to abort")
	}
	migrated := time.Now().UTC()
	if st.MigratedAt != nil {
		migrated = *st.MigratedAt
	}
	next := &state.State{Target: st.Target, Version: st.PreviousVersion, PreviousVersion: st.Version, RollingBack: true,
		ProxyMode: st.ProxyMode, AuthURL: st.AuthURL, PostgresHost: st.PostgresHost, BundleDir: "bundle-back",
		MountedBundleDirs: st.MountedBundleDirs, MigratedAt: &migrated}
	for _, svc := range []string{AuthService, EdgeService} {
		next.SetSlots(svc, st.ActiveSlots(svc))
	}
	for svc, n := range st.Surge {
		if next.Surge == nil {
			next.Surge = map[string]int{}
		}
		next.Surge[svc] = n
	}
	w.persist(next)
	w.topo = testTopologyFrom(t, strings.ReplaceAll(rolloutCompose, ":"+toV, ":"+fromV))
	w.targetV, w.targetDir = fromV, abortBundleDir
}

func slotsOf(ss []state.Slot) []int {
	var out []int
	for _, sl := range ss {
		out = append(out, sl.N)
	}
	return out
}

var abortLayouts = []struct {
	name       string
	auth, edge []state.Slot
}{
	{"one replica each", old(1), old(1)},
	{"two auth, one edge", old(1, 2), old(1)},
}

// A rollout cut off at any call -- before or after it took effect -- is
// aborted by the engine: every container ends up the previous version's,
// from the abort's bundle, with the proxy never routing to a replica that
// isn't ready nor to fewer than the operator's count.
func TestAbortAfterARolloutCutOffAnywhere(t *testing.T) {
	for _, layout := range abortLayouts {
		clean := newWorld(t, preparedState(layout.auth, layout.edge))
		if err := engineFor(clean, true, false).run(); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= clean.calls; i++ {
			for _, after := range []bool{false, true} {
				name := fmt.Sprintf("%s/rollout cut at call %d (%s, after=%v)", layout.name, i, clean.callNames[i-1], after)
				t.Run(name, func(t *testing.T) {
					w := newWorld(t, preparedState(layout.auth, layout.edge))
					w.crashAt, w.crashAfter = i, after
					if err := engineFor(w, true, false).run(); err != nil && !errors.Is(err, errCrash) {
						t.Fatalf("rollout: %v", err)
					}
					w.crashed, w.crashAt = false, 0
					if w.load().PreviousVersion == "" {
						t.Skip("the rollout had already finished: nothing to abort (CheckAbort refuses)")
					}
					prepareAbort(t, w)
					if err := engineFor(w, true, false).run(); err != nil {
						t.Fatalf("abort: %v\nevents: %v", err, w.events)
					}
					w.done(t, slotsOf(layout.auth), slotsOf(layout.edge))
				})
			}
		}
	}
}

// An abort cut off anywhere is finished by running it again (--abort reads
// RollingBack and does not turn round).
func TestAbortResumesAfterACrashAnywhere(t *testing.T) {
	for _, layout := range abortLayouts {
		// Abort a rollout cut off halfway through auth: central moved, a
		// stand-in recorded, a slot or two of each version.
		start := func(t *testing.T) *world {
			w := newWorld(t, preparedState(layout.auth, layout.edge))
			w.crashAt = indexOf(w.runAndCount(t, layout.auth, layout.edge), "compose up -d --no-deps auth") + 2
			w.crashAfter = true
			if err := engineFor(w, true, false).run(); !errors.Is(err, errCrash) {
				t.Fatalf("rollout: %v", err)
			}
			w.crashed, w.crashAt = false, 0
			prepareAbort(t, w)
			return w
		}
		probe := start(t)
		base := probe.calls
		if err := engineFor(probe, true, false).run(); err != nil {
			t.Fatal(err)
		}
		for i := base + 1; i <= probe.calls; i++ {
			for _, after := range []bool{false, true} {
				name := fmt.Sprintf("%s/abort cut at call %d (%s, after=%v)", layout.name, i-base, probe.callNames[i-1], after)
				t.Run(name, func(t *testing.T) {
					w := start(t)
					w.crashAt, w.crashAfter = i, after
					if err := engineFor(w, true, false).run(); err != nil && !errors.Is(err, errCrash) {
						t.Fatalf("abort: %v", err)
					}
					w.crashed, w.crashAt = false, 0
					st := w.load()
					if st.RolloutUnderway() && (!st.RollingBack || st.Version != fromV) {
						t.Fatalf("the record no longer says the abort is under way: %+v", st)
					}
					if err := engineFor(w, true, false).run(); err != nil {
						t.Fatalf("resumed abort: %v\nevents: %v", err, w.events)
					}
					w.done(t, slotsOf(layout.auth), slotsOf(layout.edge))
				})
			}
		}
	}
}

// runAndCount runs a clean rollout of the same layout in a scratch world and
// returns its call names, to aim a crash at a step by name.
func (w *world) runAndCount(t *testing.T, auth, edge []state.Slot) []string {
	t.Helper()
	c := newWorld(t, preparedState(auth, edge))
	if err := engineFor(c, true, false).run(); err != nil {
		t.Fatal(err)
	}
	return c.callNames
}

// A stand-in of the version the abort leaves is not recreated in place (it
// takes traffic): retired first, then a stand-in of the old version starts.
func TestAbortRetiresTheNewVersionsStandIn(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	calls := w.runAndCount(t, old(1, 2), old(1))
	w.crashAt, w.crashAfter = indexOf(calls, "compose up -d --no-deps auth")+1, true
	if err := engineFor(w, true, false).run(); !errors.Is(err, errCrash) {
		t.Fatalf("rollout: %v", err)
	}
	w.crashed, w.crashAt = false, 0
	if w.load().Surge[AuthService] != 3 {
		t.Fatalf("expected a stand-in in auth slot 3: %v", w.load().Surge)
	}
	prepareAbort(t, w)
	w.events = nil
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	rm := indexOf(w.events, "rm auth-3")
	cleared := indexOf(w.events, "surge map[]")
	restart := indexOf(w.events, "compose up -d --no-deps auth-3")
	if rm < 0 || cleared < rm || restart < cleared {
		t.Errorf("stand-in not retired before a new one started: rm %d, cleared %d, started %d\n%v", rm, cleared, restart, w.events)
	}
	// Before it went, it was taken out of the proxy and drained.
	if rm < 1 || w.events[rm-1] != "drained" {
		t.Errorf("auth-3 removed without draining: %v", w.events[max(0, rm-3):rm+1])
	}
	w.done(t, []int{1, 2}, []int{1})
}

// One replica, a stand-in of the new version serving alone: the replica was
// recreated on the new version and never became ready (what an abort is
// for). The abort moves the replica back first -- it serves nothing, so it
// needs no stand-in -- and only then retires the stand-in, which until then
// is all that serves.
func TestAbortWhenTheNewStandInIsAllThatServes(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.never["auth"] = true
	if err := engineFor(w, true, false).run(); err == nil || !strings.Contains(err.Error(), "auth never became ready") {
		t.Fatalf("rollout: %v", err)
	}
	if got := w.upstream[AuthService]; !reflect.DeepEqual(got, []string{"auth-2"}) {
		t.Fatalf("setup: upstream %v", got)
	}
	delete(w.never, "auth")
	prepareAbort(t, w)
	w.minUp[AuthService] = 1
	w.events = nil
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatalf("abort: %v\n%v", err, w.events)
	}
	if indexOf(w.events, "compose up -d --no-deps auth") > indexOf(w.events, "rm auth-2") {
		t.Errorf("the stand-in went before the replica was back: %v", w.events)
	}
	w.done(t, []int{1}, []int{1})
}

// The same, cut off right after the replica was started on the old version
// and run again while it is still starting: it is brought up (pending)
// before the stand-in goes, not refused for leaving nothing to route to.
func TestAbortResumedWhileTheReplicaIsStarting(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.never["auth"] = true
	if err := engineFor(w, true, false).run(); err == nil {
		t.Fatal("setup: the rollout should fail")
	}
	prepareAbort(t, w)
	w.minUp[AuthService] = 1
	// auth recreated on the old version from the abort's bundle, not ready yet.
	w.containers["auth"] = &fakeContainer{image: imageOf(AuthService, fromV), workDir: abortBundleDir, running: true, ready: false}
	delete(w.never, "auth")
	e := engineFor(w, true, false)
	e.ops = readyAfterStart{w}
	if err := e.run(); err != nil {
		t.Fatalf("resumed abort: %v\n%v", err, w.events)
	}
	w.done(t, []int{1}, []int{1})
}

// readyAfterStart makes the starting replica ready once waited for.
type readyAfterStart struct{ *world }

func (r readyAfterStart) waitReady(u string) error {
	if c := r.containers[r.urls[u]]; c != nil {
		c.ready = true
	}
	return r.world.waitReady(u)
}

func (r readyAfterStart) waitReadyWithin(u string, limit time.Duration) error {
	if c := r.containers[r.urls[u]]; c != nil {
		c.ready = true
	}
	return r.world.waitReadyWithin(u, limit)
}

// Going back, a container Compose did not label with its bundle is not taken
// for the target's, though it has the target's image: it may mount the
// bundle the end of the run deletes.
func TestAbortRecreatesAnUnlabelledContainer(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	prepareAbort(t, w)
	w.containers["auth-2"].workDir = ""
	w.containers[CentralService].workDir = ""
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{"central", "auth-2"} {
		if indexOf(w.events, "compose up -d --no-deps "+svc) < 0 {
			t.Errorf("%s was not recreated", svc)
		}
	}
	w.done(t, []int{1, 2}, []int{1})
}

// The replicas that never left the old version keep their record (the
// version they run) but are recreated: they mount the old bundle, which the
// end of the run deletes.
func TestAbortRecreatesReplicasThatNeverMoved(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	calls := w.runAndCount(t, old(1, 2), old(1))
	// Cut off right after central came up on the new version: no auth or
	// edge replica moved.
	w.crashAt, w.crashAfter = indexOf(calls, "ready central"), true
	if err := engineFor(w, true, false).run(); !errors.Is(err, errCrash) {
		t.Fatalf("rollout: %v", err)
	}
	w.crashed, w.crashAt = false, 0
	prepareAbort(t, w)
	e := engineFor(w, true, false)
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, sl := range w.load().ActiveSlots(AuthService) {
		if sl.Version != fromV {
			t.Errorf("auth slot %d relabelled %s: it runs %s", sl.N, sl.Version, fromV)
		}
		if e.current[AuthService][sl.N] {
			t.Errorf("auth slot %d taken as done, but its container is from the old bundle", sl.N)
		}
	}
	w.events = nil
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{"central", "auth", "auth-2", "edge"} {
		if indexOf(w.events, "compose up -d --no-deps "+svc) < 0 {
			t.Errorf("%s was not recreated from the abort's bundle", svc)
		}
	}
	w.done(t, []int{1, 2}, []int{1})
}

// finish never lets the old bundles go while a container still mounts one.
func TestFinishRefusesAContainerFromAnotherBundle(t *testing.T) {
	// Nothing moved: every container is the old version's, from the old
	// bundle -- the right version, the wrong bundle.
	w := newWorld(t, preparedState(old(1), old(1)))
	prepareAbort(t, w)
	e := engineFor(w, true, false)
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(); err == nil || !strings.Contains(err.Error(), "was not recreated") {
		t.Fatalf("got %v", err)
	}
	if indexOf(w.events, "markRunning") >= 0 {
		t.Error("markRunning ran")
	}
}

// A container of the target that is not running when the run starts --
// Docker restarting it (restart: unless-stopped after an early exit) -- is
// not counted as serving: it is started, waited for, and only then routed to.
func TestRolloutStartsACurrentReplicaThatIsNotRunning(t *testing.T) {
	st := preparedState(old(1), old(1))
	st.SetSlots(AuthService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: toV}})
	now := time.Now()
	st.MigratedAt = &now
	w := newWorld(t, st)
	w.containers[CentralService] = &fakeContainer{image: imageOf(CentralService, toV), workDir: newBundleDir, running: true, ready: true}
	w.containers["auth-2"] = &fakeContainer{image: imageOf(AuthService, toV), workDir: newBundleDir, running: false}
	w.upstream[AuthService] = []string{"auth"}
	w.minUp[AuthService] = 1
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatal(err)
	}
	start := indexOf(w.events, "compose up -d --no-deps auth-2")
	ready := indexOf(w.events, "ready auth-2")
	if start < 0 || ready < start {
		t.Errorf("auth-2 not started and waited for: %v", w.events)
	}
	w.done(t, []int{1, 2}, []int{1})
}

// `down --force` mid-rollout leaves no container; the same rollout command
// brings everything up on the new version.
func TestRolloutAfterForcedDown(t *testing.T) {
	w := newWorld(t, preparedState(old(1, 2), old(1)))
	calls := w.runAndCount(t, old(1, 2), old(1))
	w.crashAt = indexOf(calls, "compose up -d --no-deps auth-2")
	if err := engineFor(w, true, false).run(); !errors.Is(err, errCrash) {
		t.Fatalf("rollout: %v", err)
	}
	w.crashed, w.crashAt = false, 0
	w.containers = map[string]*fakeContainer{} // docker compose down
	w.minUp = map[string]int{}                 // nothing serves until it is back
	if err := engineFor(w, true, false).run(); err != nil {
		t.Fatalf("rollout after down: %v\n%v", err, w.events)
	}
	w.violations = nil // routing to replicas that are down, by necessity, until they are up
	w.done(t, []int{1, 2}, []int{1})
}

// ---- how an abort goes back ----

func abortDirs(t *testing.T, withOld bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, b := range []string{"bundle-old", "bundle-new"} {
		if b == "bundle-old" && !withOld {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, b), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, b, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestChooseAbortPath(t *testing.T) {
	type tc struct {
		mutate  func(st *state.State, w *world, dir string)
		withOld bool
		want    abortPath
		missing []string
	}
	cases := map[string]tc{
		"prepared only": {func(*state.State, *world, string) {}, true, abortRestore, nil},
		"migrated, central not moved yet (markStarting done)": {func(st *state.State, _ *world, _ string) {
			now := time.Now()
			st.MigratedAt = &now
			st.MountedBundleDirs = []string{"bundle-old", "bundle-new"}
		}, true, abortRestore, nil},
		"central moved": {func(st *state.State, w *world, dir string) {
			st.MountedBundleDirs = []string{"bundle-old", "bundle-new"}
			w.containers[CentralService].workDir = filepath.Join(dir, "bundle-new")
		}, true, abortRollBack, nil},
		"a stand-in recorded": {func(st *state.State, _ *world, _ string) {
			st.Surge = map[string]int{AuthService: 2}
		}, true, abortRollBack, nil},
		"the old bundle is gone": {func(*state.State, *world, string) {}, false, abortRollBack, nil},
		"a container without the working-dir label": {func(_ *state.State, w *world, _ string) {
			w.containers["edge"].workDir = ""
		}, true, abortRollBack, nil},
		"already rolling back": {func(st *state.State, _ *world, _ string) { st.RollingBack = true }, true, abortRollBack, nil},
		"a slot recorded at the new version": {func(st *state.State, _ *world, _ string) {
			st.SetSlots(AuthService, []state.Slot{{N: 1, Version: toV}})
		}, true, abortRollBack, nil},
		"a replica down: still the old bundle's deployment": {func(_ *state.State, w *world, _ string) {
			delete(w.containers, "edge")
		}, true, abortRestore, []string{"edge"}},
		"central gone (a recreate cut off halfway): restored, and said": {func(_ *state.State, w *world, _ string) {
			delete(w.containers, CentralService)
		}, true, abortRestore, []string{CentralService}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := abortDirs(t, c.withOld)
			st := preparedState(old(1), old(1))
			w := newWorld(t, st)
			for _, ct := range w.containers {
				ct.workDir = filepath.Join(dir, "bundle-old")
			}
			c.mutate(st, w, dir)
			path, prev, missing, err := chooseAbortPath(st, w, dir)
			if err != nil {
				t.Fatal(err)
			}
			if path != c.want {
				t.Errorf("path %v, want %v", path, c.want)
			}
			if !reflect.DeepEqual(missing, c.missing) {
				t.Errorf("missing %v, want %v", missing, c.missing)
			}
			if path == abortRestore && prev != "bundle-old" {
				t.Errorf("restores %q", prev)
			}
		})
	}
}

// A migration a killed CLI left running (from the new bundle, still moving
// the schema) stops the abort, whichever way it would go.
func TestChooseAbortPathWaitsForARunningMigration(t *testing.T) {
	dir := abortDirs(t, true)
	st := preparedState(old(1), old(1))
	w := newWorld(t, st)
	w.oneOffs = []string{"versola-auth-migrate"}
	if _, _, _, err := chooseAbortPath(st, w, dir); err == nil || !strings.Contains(err.Error(), "versola-auth-migrate still running") {
		t.Fatalf("got %v", err)
	}
}

func TestAbortByRestoringTouchesNoContainer(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	if err := abortByRestoring(w.load(), w, "bundle-old"); err != nil {
		t.Fatal(err)
	}
	if w.restored != "bundle-old" {
		t.Errorf("restored %q", w.restored)
	}
	for _, e := range w.events {
		if strings.HasPrefix(e, "compose") || strings.HasPrefix(e, "rm ") || strings.HasPrefix(e, "reload") {
			t.Errorf("touched something running: %s", e)
		}
	}
}

// ---- refusals and texts ----

func TestCheckAbort(t *testing.T) {
	st := rolloutGuardState(t)
	underway := func(s *state.State) {
		s.PreviousVersion, s.Version = fromV, toV
		s.SetSlots(AuthService, old(1))
		s.SetSlots(EdgeService, old(1))
	}
	s := *st
	underway(&s)
	if err := CheckAbort(&s, RolloutOptions{}); err != nil {
		t.Fatalf("an abort under way: %v", err)
	}
	refused := map[string]struct {
		mutate func(s *state.State)
		opts   RolloutOptions
		want   string
	}{
		"nothing under way":     {func(*state.State) {}, RolloutOptions{}, "nothing to abort"},
		"finished rollout":      {func(s *state.State) { s.Version = toV }, RolloutOptions{}, "versola rollout <version> --assume-compatible"},
		"local":                 {func(s *state.State) { underway(s); s.Target = "local" }, RolloutOptions{}, "configured and started again"},
		"stand-in, no from":     {func(s *state.State) { s.Surge = map[string]int{AuthService: 2} }, RolloutOptions{}, "finishes it"},
		"one replica, no surge": {underway, RolloutOptions{NoSurge: true}, "--allow-downtime"},
		"downtime alone":        {underway, RolloutOptions{AllowDowntime: true}, "only goes with --no-surge"},
	}
	for name, c := range refused {
		s := *st
		s.Slots = nil
		c.mutate(&s)
		if err := CheckAbort(&s, c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if err := CheckAbort(nil, RolloutOptions{}); err == nil {
		t.Error("nothing configured: accepted")
	}
	// With a stand-in, --no-surge counts the operator's replicas only.
	s2 := *st
	underway(&s2)
	s2.SetSlots(AuthService, []state.Slot{{N: 1, Version: fromV}, {N: 2, Version: toV}})
	s2.Surge = map[string]int{AuthService: 2}
	if err := CheckAbort(&s2, RolloutOptions{NoSurge: true}); err == nil || !strings.Contains(err.Error(), "auth has one replica") {
		t.Errorf("stand-in counted as a replica: %v", err)
	}
}

func TestCheckRolloutDuringAnAbort(t *testing.T) {
	st := rolloutGuardState(t)
	s := *st
	s.Version, s.PreviousVersion, s.RollingBack = fromV, toV, true
	s.SetSlots(AuthService, []state.Slot{{N: 1, Version: toV}})
	s.MountedBundleDirs = []string{"bundle-0", "bundle-1"}
	if _, err := CheckRollout(&s, toV, RolloutOptions{}); err == nil || !strings.Contains(err.Error(), "an abort back to 0.6.2 is under way") {
		t.Errorf("rolling forward during an abort: %v", err)
	}
	// `rollout <old>` continues the abort, like --abort.
	if done, err := CheckRollout(&s, fromV, RolloutOptions{}); done || err != nil {
		t.Errorf("continuing the abort: %v %v", done, err)
	}
}

func TestRequireCompatible(t *testing.T) {
	st := rolloutGuardState(t)
	err := RequireCompatible(st, toV, RolloutOptions{})
	if err == nil || !strings.Contains(err.Error(), "--assume-compatible") || !strings.Contains(err.Error(), "fresh database") {
		t.Errorf("unconfirmed: %v", err)
	}
	if err := RequireCompatible(st, toV, RolloutOptions{AssumeCompatible: true}); err != nil {
		t.Errorf("confirmed: %v", err)
	}
	s := *st
	s.PreviousVersion, s.Version = fromV, toV
	if err := RequireCompatible(&s, toV, RolloutOptions{}); err != nil {
		t.Errorf("continuing needs no confirmation: %v", err)
	}
	plan := RolloutPlan(st, toV, RolloutOptions{})
	if !strings.Contains(plan, "Compatibility: NOT confirmed") {
		t.Errorf("plan without the flag:\n%s", plan)
	}
	if plan := RolloutPlan(st, toV, RolloutOptions{AssumeCompatible: true}); !strings.Contains(plan, "confirmed by you") {
		t.Errorf("plan with the flag:\n%s", plan)
	}
}

func TestDownBlocked(t *testing.T) {
	st := preparedState(old(1), old(1))
	if err := DownBlocked(st, false); err == nil || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "--abort") {
		t.Errorf("got %v", err)
	}
	if err := DownBlocked(st, true); err != nil {
		t.Errorf("forced: %v", err)
	}
	st.PreviousVersion, st.Surge = "", nil
	st.Slots = nil
	if err := DownBlocked(st, false); err != nil {
		t.Errorf("no rollout: %v", err)
	}
}

func TestAbortPlanAndSummary(t *testing.T) {
	st := preparedState(old(1), old(1))
	plan := AbortPlan(st, RolloutOptions{})
	for _, want := range []string{"back from 0.7.0 to 0.6.2", "What does NOT go back", "migrations already applied stay", "secrets", "stand-in of 0.7.0 is retired", "--abort` again"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	st.Version, st.PreviousVersion, st.RollingBack = fromV, toV, true
	if plan := AbortPlan(st, RolloutOptions{}); !strings.Contains(plan, "back from 0.7.0 to 0.6.2") || !strings.Contains(plan, "continuing") {
		t.Errorf("plan of an abort under way:\n%s", plan)
	}
	if sum := RolloutSummary(st); !strings.Contains(sum, "Abort of the rolling upgrade under way: back from 0.7.0 to 0.6.2") {
		t.Errorf("summary: %s", sum)
	}
}
