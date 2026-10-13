package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
	"github.com/versolauth/versola-cli/internal/wait"
)

// RolloutOptions are a rollout's choices (see cmd/rollout.go).
type RolloutOptions struct {
	// NoSurge: no stand-in replica; each replica is replaced with one fewer
	// serving meanwhile. A service with one replica then needs
	// AllowDowntime.
	NoSurge bool
	// AllowDowntime: with NoSurge, a service with one replica is recreated in
	// place, unreachable until it is ready.
	AllowDowntime bool
	// Secrets: --generate-missing / --allow-legacy-secrets, as for configure;
	// applied when the new version is prepared.
	Secrets SecretOptions
}

// rolloutReadyTimeout bounds the wait for one container of the new version
// to report ready. Longer than `up`'s minute: central's start on a loaded
// server was not measured (see the PR's "not verified" list).
const rolloutReadyTimeout = 120 * time.Second

// CheckRollout is everything about `rollout <version>` that can be refused
// on the record alone, before the operator is asked to confirm. done is true
// when the deployment already runs version and no rollout is under way.
func CheckRollout(st *state.State, version string, o RolloutOptions) (done bool, err error) {
	if st == nil {
		return false, fmt.Errorf("nothing has been configured yet -- a rollout upgrades a running deployment")
	}
	if version == "" {
		return false, fmt.Errorf("a version is required")
	}
	if len(version) > 1 && version[0] == 'v' && version[1] >= '0' && version[1] <= '9' {
		return false, fmt.Errorf("Versola releases are tagged without a leading \"v\" -- use %q", version[1:])
	}
	if st.Target != "vps" {
		return false, fmt.Errorf("`rollout` upgrades a vps deployment without stopping it; a %s deployment is throwaway -- run `versola configure %s %s`, `versola migrate` and `versola up` instead", st.Target, st.Target, version)
	}
	if o.AllowDowntime && !o.NoSurge {
		return false, fmt.Errorf("--allow-downtime only goes with --no-surge (with a stand-in, nothing is down)")
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) -- configure it again")
	}
	if err := requireCurrentBundle(st, filepath.Dir(composePath)); err != nil {
		return false, err
	}
	if st.PostgresHost == "" {
		return false, fmt.Errorf("this deployment was configured by a versola-cli from before rolling upgrades (it does not record --postgres-host) -- configure it once with this one (`versola configure vps ...`, `migrate`, `up`: a short stop), and later versions can be rolled out")
	}
	if st.RolloutUnderway() {
		switch version {
		case st.Version:
		case st.PreviousVersion:
			return false, fmt.Errorf("a rollout from %s to %s is under way; going back to %s is `versola rollout --abort`, which this versola-cli does not have yet -- `versola rollout %s` finishes the rollout", st.PreviousVersion, st.Version, version, st.Version)
		default:
			return false, fmt.Errorf("a rollout to %s is under way -- finish it with `versola rollout %s` before rolling out %s", st.Version, st.Version, version)
		}
	} else {
		if why := st.MixedVersions(); why != "" {
			return false, fmt.Errorf("the recorded replicas are not one version (%s), but no rollout is recorded -- was state.json edited by hand?", why)
		}
		if !st.RunsFromCurrentBundle() {
			return false, fmt.Errorf("the running stack is not from this configuration yet -- run `versola up` first")
		}
		if version == st.Version {
			return true, nil
		}
	}
	if o.NoSurge && !o.AllowDowntime {
		for _, svc := range []string{AuthService, EdgeService} {
			if _, standIn := st.Surge[svc]; standIn {
				continue
			}
			slots := st.ActiveSlots(svc)
			oldOnes := 0
			for _, sl := range slots {
				if sl.Version != version || !st.RolloutUnderway() {
					oldOnes++
				}
			}
			if len(slots) == 1 && oldOnes == 1 {
				return false, fmt.Errorf("%s has one replica: without a stand-in (--no-surge) replacing it leaves %s unreachable until the new one is ready -- drop --no-surge, or add --allow-downtime to accept that", svc, svc)
			}
		}
	}
	return false, nil
}

// RolloutPlan describes what `rollout version` is about to do, from the
// record alone: printed before the confirmation, and by --dry-run.
func RolloutPlan(st *state.State, version string, o RolloutOptions) string {
	var b strings.Builder
	from := st.Version
	if st.RolloutUnderway() {
		from = st.PreviousVersion
	}
	fmt.Fprintf(&b, "Rolling upgrade of the vps deployment at %s\n", st.AuthURL)
	fmt.Fprintf(&b, "  Versola:      %s -> %s", from, version)
	if st.RolloutUnderway() {
		b.WriteString("  (under way: continuing where it stopped)")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  Taken from this deployment: auth URL %s, Postgres at %s, proxy %q", st.AuthURL, st.PostgresHost, st.ProxyMode)
	if dir, err := bundleDirOf(st); err == nil && usesACMEStaging(dir) {
		b.WriteString(" (Let's Encrypt staging)")
	}
	if openbaoSetUpByHand(st.Target) {
		b.WriteString(", OpenBao set up by hand")
	}
	b.WriteString("\n\n  Steps:\n")
	done := func(ok bool) string {
		if ok {
			return "  [done]"
		}
		return ""
	}
	prepared := st.RolloutUnderway()
	fmt.Fprintf(&b, "   1. prepare %s: versola-tools, the secrets plan (new secrets go into OpenBao), a new bundle -- nothing running changes%s\n", version, done(prepared))
	fmt.Fprintf(&b, "   2. migrate the database (migrations can't be rolled back -- back it up first)%s\n", done(prepared && st.MigratedAt != nil))
	b.WriteString("   3. central: stopped and started on the new version -- the admin console and self-registration are\n      unavailable until it is ready; auth and edge keep serving from their caches\n")
	step := 4
	for _, svc := range []string{AuthService, EdgeService} {
		slots := st.ActiveSlots(svc)
		standIn, hasStandIn := st.Surge[svc]
		var nums []string
		for _, sl := range slots {
			if hasStandIn && sl.N == standIn {
				continue
			}
			mark := ""
			if prepared && sl.Version == version {
				mark = " (done)"
			}
			nums = append(nums, fmt.Sprintf("%d%s", sl.N, mark))
		}
		switch {
		case hasStandIn:
			fmt.Fprintf(&b, "   %d. %s: stand-in %s already started; then slot(s) %s one at a time; then the stand-in goes\n", step, svc, topology.SlotService(svc, standIn), strings.Join(nums, ", "))
		case !o.NoSurge:
			n := "a free slot"
			if free, err := freeSlots(slots, 1); err == nil {
				n = "slot " + fmt.Sprint(free[0])
			}
			fmt.Fprintf(&b, "   %d. %s: a stand-in on %s in %s, then slot(s) %s one at a time (out of the proxy, drained,\n      recreated, ready, back in), then the stand-in goes -- capacity never drops\n", step, svc, version, n, strings.Join(nums, ", "))
		case len(nums) == 1 && o.AllowDowntime:
			fmt.Fprintf(&b, "   %d. %s: slot %s recreated in place -- %s is DOWN until it is ready\n", step, svc, nums[0], svc)
		default:
			fmt.Fprintf(&b, "   %d. %s: slot(s) %s one at a time, one replica fewer serving meanwhile\n", step, svc, strings.Join(nums, ", "))
		}
		step++
	}
	fmt.Fprintf(&b, "   %d. finish: the proxy on the new bundle, the previous one removed\n", step)
	if !o.NoSurge {
		b.WriteString("\n  Note: a stand-in is one more JVM next to the running ones (about 0.6-0.9 GB) and about 10 more\n" +
			"  Postgres connections. This versola-cli does not check free memory or max_connections yet --\n" +
			"  look at `free -m` first; on a small server use --no-surge (with two or more replicas).\n")
	}
	b.WriteString("\n  If it stops (an error, Ctrl-C, a dropped SSH session), running the same command continues it.\n")
	return b.String()
}

func bundleDirOf(st *state.State) (string, error) {
	p, _, err := st.ComposeFilePath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// usesACMEStaging reports whether the bundle's proxy gets its certificate
// from Let's Encrypt's staging environment (configure's --acme-staging, which
// the record doesn't keep): the new bundle is configured the same way.
func usesACMEStaging(bundleDir string) bool {
	b, err := os.ReadFile(filepath.Join(bundleDir, "proxy", "conf.d", "versola.conf"))
	return err == nil && strings.Contains(string(b), proxy.ACMEStaging)
}

// openbaoSetUpByHand: no admin credentials saved for target means this CLI
// did not provision its OpenBao (configure's --setup-openbao); the new
// version is configured the same way.
func openbaoSetUpByHand(target string) bool {
	_, err := openbao.LoadAdminCreds(target)
	return errors.Is(err, openbao.ErrNoAdminCreds)
}

// rolloutPreflight checks what the record cannot say, before anything
// changes: the host ports each stand-in will take. (Free memory and
// Postgres connections are a later step: see RolloutPlan's note.)
func rolloutPreflight(st *state.State, o RolloutOptions) error {
	// The rollout moves traffic by reloading the proxy; a stopped one would
	// only get files, and nothing would be routed around a replica being
	// replaced.
	name := proxy.ContainerFor(st.ProxyMode)
	running, err := docker.IsRunning(name)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("the reverse proxy (%s) is not running -- start it first (`docker start %s`)", name, name)
	}
	if o.NoSurge {
		return nil
	}
	composePath, _, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	topo, err := loadTopology(composePath)
	if err != nil {
		return err
	}
	for _, svc := range []string{AuthService, EdgeService} {
		if _, standIn := st.Surge[svc]; standIn {
			continue
		}
		free, err := freeSlots(st.ActiveSlots(svc), 1)
		if err != nil {
			return fmt.Errorf("%s: no free slot for a stand-in replica (%w) -- use --no-surge", svc, err)
		}
		r, err := standInReplica(topo, svc, free[0], true)
		if err != nil {
			return err
		}
		for _, port := range hostPortsOf(r, true) {
			if res := checks.PortFree(port, ""); !res.OK {
				return fmt.Errorf("the stand-in %s would need port %d on this machine: %s", r.Service, port, res.Detail)
			}
		}
	}
	// TODO(0d PR-3): free memory against a JVM's footprint, and Postgres
	// max_connections against one more pool, belong here.
	return nil
}

// Rollout runs `versola rollout <version>` on st (what CheckRollout passed):
// prepares the new version unless that is done, then carries the rollout
// to the end. The caller holds state.Lock for the whole run.
func Rollout(st *state.State, version string, o RolloutOptions) error {
	if err := rolloutPreflight(st, o); err != nil {
		return err
	}
	if !st.RolloutUnderway() {
		dir, err := bundleDirOf(st)
		if err != nil {
			return err
		}
		proxyOpts := ProxyOptions{Mode: st.ProxyMode, ACMEStaging: usesACMEStaging(dir)}
		fmt.Printf("\nPreparing Versola %s next to the running %s (nothing running changes in this step)...\n", version, st.Version)
		if _, err := configure(st.Target, version, st.AuthURL, st.PostgresHost, openbaoSetUpByHand(st.Target), proxyOpts, o.Secrets,
			&stagedConfigure{from: st, surge: !o.NoSurge}); err != nil {
			return fmt.Errorf("preparing %s failed -- nothing running was changed: %w", version, err)
		}
		next, err := state.Load()
		if err != nil {
			return err
		}
		st = next
	}

	composePath, _, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	topo, err := loadTopology(composePath)
	if err != nil {
		return err
	}
	fronts, genDir, err := proxyRouting(topo, true)
	if err != nil {
		return err
	}
	authURL, err := proxy.ParseAuthURL(st.AuthURL, st.ProxyMode)
	if err != nil {
		return fmt.Errorf("the recorded auth URL is no longer valid: %w", err)
	}
	if topo.Project() == "" {
		return fmt.Errorf("the compose file has no project name, so the rollout can't find its containers")
	}
	ops := dockerRolloutOps{
		dockerReplicaOps: dockerReplicaOps{dir: filepath.Dir(composePath), composePath: composePath, mode: st.ProxyMode},
		st:               st, project: topo.Project(), host: authURL.Host,
	}

	interrupted, stopWatching := watchInterrupts()
	defer stopWatching()
	// Ctrl-C reaches this process only, not the docker calls it makes: the
	// step under way finishes, and the run stops before the next.
	docker.SetShielded(true)
	defer docker.SetShielded(false)

	e := &rolloutEngine{st: st, topo: topo, bundleDir: filepath.Dir(composePath), ops: ops, out: func(f string, a ...any) { fmt.Printf(f, a...) },
		fronts: fronts, genDir: genDir, surge: !o.NoSurge, allowDowntime: o.AllowDowntime, interrupted: interrupted}
	if err := e.run(); err != nil {
		return err
	}
	fmt.Printf("\nVersola %s is rolled out at %s.\n", st.Version, st.AuthURL)
	return nil
}

// watchInterrupts turns the first Ctrl-C (or SIGTERM, or the SIGHUP of a
// dropped SSH session) into a request to stop between two steps, and a
// second into leaving at once.
func watchInterrupts() (interrupted func() bool, stop func()) {
	var asked atomic.Bool
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sigs:
				if !asked.Swap(true) {
					fmt.Fprintln(os.Stderr, "\nStopping after the step under way -- that can take a few minutes (a replica is started, waited for and warmed up in one step). Ctrl-C again to leave sooner.")
					continue
				}
				// Leaving releases the lock: not while a docker command is
				// still recreating a container, or a run started right after
				// would act on it halfway through. From here no new docker
				// command starts (docker.Leave); the one under way finishes.
				idle := docker.Leave()
				select {
				case <-idle:
				default:
					fmt.Fprintln(os.Stderr, "\nLeaving as soon as the docker command under way finishes (Ctrl-C once more to leave right now -- then wait for it, `docker ps`, before running versola again)...")
					select {
					case <-idle:
					case <-sigs:
						fmt.Fprintln(os.Stderr, "\nLeaving now; a docker command is still running -- wait for it before running versola again.")
						os.Exit(130)
					case <-done:
						return // the run ended on its own meanwhile
					}
				}
				fmt.Fprintln(os.Stderr, "\nLeaving now. Run the same `versola rollout` again to continue.")
				os.Exit(130)
			case <-done:
				return
			}
		}
	}()
	return asked.Load, func() {
		signal.Stop(sigs)
		close(done)
	}
}

// RolloutDryRun prints what `rollout version` would do and changes nothing:
// the plan; the host ports a stand-in needs; and, from the new version's
// versola-tools run into a temporary directory, its secrets plan and the
// migrations it would apply (`migrate --dry-run`, against the real
// database). Nothing is written to OpenBao, state.json or the bundle.
func RolloutDryRun(st *state.State, version string, o RolloutOptions) error {
	fmt.Print(RolloutPlan(st, version, o))
	fmt.Println()
	if err := rolloutPreflight(st, o); err != nil {
		fmt.Printf("Would stop before changing anything: %v\n", err)
		return err
	}
	if st.RolloutUnderway() {
		if st.MigratedAt != nil {
			fmt.Println("The migrations of this rollout are applied.")
			return nil
		}
		fmt.Printf("Pending migrations of %s (from its prepared bundle):\n", version)
		return Migrate(st, MigrateOptions{DryRun: true})
	}
	return previewVersion(st, version, o.Secrets)
}

// previewVersion runs version's versola-tools into a temporary directory,
// settles its secrets against OpenBao without writing any (see
// settleSecrets), writes them into that directory only, and has the
// migrate service report what it would apply. The directory -- secret
// values included -- is removed before this returns, and on Ctrl-C; one a
// kill -9 leaves behind (rollout-dryrun-*) is removed by the next plan or dry
// run after an hour.
func previewVersion(st *state.State, version string, secretOpts SecretOptions) error {
	creds, err := openbao.LoadCredentials(st.Target)
	if err != nil {
		if errors.Is(err, openbao.ErrNoCredentials) {
			return fmt.Errorf("no OpenBao credentials stored for %q -- the dry run reads what OpenBao holds", st.Target)
		}
		return err
	}
	client := openbao.NewClient(creds)
	ctx := context.Background()
	if err := client.Login(ctx); err != nil {
		return err
	}
	dir, cleanup, err := scratchDir("rollout-dryrun-")
	if err != nil {
		return err
	}
	defer cleanup(os.Stdout)
	defer removeOnSignal(os.Stdout, cleanup)()

	fmt.Printf("Running versola-tools %s into a temporary directory...\n", version)
	if err := pullAndRunTools(dir, ToolsImage(version), st.Target, st.AuthURL, st.PostgresHost); err != nil {
		if isManifestUnknown(err) {
			return fmt.Errorf("version %q of Versola doesn't exist (no versola-tools image published for it)", version)
		}
		return fmt.Errorf("versola-tools failed: %w", err)
	}
	if err := requireAdminConsole(dir, version); err != nil {
		return err
	}
	fmt.Println("Secrets (read from OpenBao, nothing written):")
	set, err := settleSecrets(ctx, client, dir, st.Target, version, secretOpts)
	if err != nil {
		return fmt.Errorf("the rollout would stop when preparing %s: %w", version, err)
	}
	for _, service := range secretServices {
		final, _ := mergeSecretsShared(withoutKeys(set.existing[service], set.replace[service]), set.candidates[service], set.shared[service])
		if err := writeDotenv(filepath.Join(dir, service+".secrets.env"), final); err != nil {
			return err
		}
	}
	composePath := filepath.Join(dir, "compose.yml")
	if err := os.Rename(filepath.Join(dir, "compose.fragment.yml"), composePath); err != nil {
		return fmt.Errorf("couldn't finalize the temporary compose file: %w", err)
	}
	has, err := hasMigrateService(composePath)
	if err != nil {
		return err
	}
	if !has {
		fmt.Printf("Versola %s predates the standalone migrate service: its pending migrations can't be listed without applying them.\n", version)
		return nil
	}
	fmt.Printf("\nPending migrations of %s:\n", version)
	return migrateViaService(composePath, MigrateOptions{DryRun: true, noDeps: true})
}

// ---- the real thing ----

type dockerRolloutOps struct {
	dockerReplicaOps
	st      *state.State
	project string
	// host is the public host name auth is reached at: the Host of the
	// warm-up probes (see probeReplica).
	host string
}

func (o dockerRolloutOps) waitReady(u string) error { return wait.ForReady(u, rolloutReadyTimeout) }

// writeReplicas writes overrides.yml along with replicas.yml: the rollout
// recreates auth's slot 1 from this bundle, and without the override it
// would listen on the APORT/MPORT the proxy holds.
func (o dockerRolloutOps) writeReplicas(auth, edge []replica) error {
	if err := o.dockerReplicaOps.writeReplicas(auth, edge); err != nil {
		return err
	}
	return writeOverridesFile(o.dir, auth)
}

func (o dockerRolloutOps) readyNow(u string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(time.Second)
		}
		resp, err := c.Get(u)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
	}
	return false
}

func (o dockerRolloutOps) pull() error {
	services := []string{CentralService, AuthService, EdgeService}
	if has, err := hasMigrateService(o.composePath); err != nil {
		return err
	} else if has {
		services = append(services, "migrate")
	}
	return docker.Run(state.ComposeArgs(o.composePath, append([]string{"pull"}, services...)...)...)
}

// containersOf are the IDs of the containers Compose made for a service of
// this project (not `compose run` one-offs), found by its labels.
func (o dockerRolloutOps) containersOf(service string) ([]string, error) {
	out, err := docker.Output("ps", "-a", "-q", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+o.project,
		"--filter", "label=com.docker.compose.service="+service,
		"--filter", "label=com.docker.compose.oneoff=False")
	if err != nil {
		return nil, fmt.Errorf("couldn't list the containers of %s: %w", service, err)
	}
	return strings.Fields(string(out)), nil
}

func (o dockerRolloutOps) inspect(service string) (containerInfo, error) {
	ids, err := o.containersOf(service)
	if err != nil {
		return containerInfo{}, err
	}
	switch len(ids) {
	case 0:
		return containerInfo{}, nil
	case 1:
	default:
		// Compose recreates a container under a temporary name
		// ("<id>_<name>") first: a recreate cut off, or still finishing.
		return containerInfo{}, fmt.Errorf("%d containers carry the compose labels of %s -- a recreate still under way, or one cut off: wait a minute and run again, or remove the one whose name starts with an ID (`docker ps -a`)", len(ids), service)
	}
	out, found, err := docker.Inspect(ids[0], `{{.Config.Image}}|{{.State.Running}}|{{index .Config.Labels "com.docker.compose.project.working_dir"}}`)
	if err != nil || !found {
		return containerInfo{}, err
	}
	parts := strings.SplitN(out, "|", 3)
	if len(parts) != 3 {
		return containerInfo{}, fmt.Errorf("couldn't read the container of %s", service)
	}
	workDir := parts[2]
	if workDir == "<no value>" {
		workDir = ""
	}
	return containerInfo{found: true, image: parts[0], running: parts[1] == "true", workDir: workDir}, nil
}

func (o dockerRolloutOps) removeContainer(service string) error {
	ids, err := o.containersOf(service)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := docker.Run("stop", "-t", stopTimeoutSeconds, id); err != nil {
			return fmt.Errorf("couldn't stop %s: %w", service, err)
		}
		if err := docker.Run("rm", "-f", id); err != nil {
			return fmt.Errorf("couldn't remove %s: %w", service, err)
		}
	}
	return nil
}

func (o dockerRolloutOps) warm(r replica) error {
	time.Sleep(standInWarmup)
	return probeReplica(r, o.host, 60*time.Second)
}

func (o dockerRolloutOps) migrate() error { return Migrate(o.st, MigrateOptions{}) }
func (o dockerRolloutOps) markStarting() error {
	return state.MarkStarting()
}
func (o dockerRolloutOps) markRunning() error { return state.MarkRunning(openbaoBundles()...) }

func (o dockerRolloutOps) update(f func(*state.State)) error {
	s, err := state.Load()
	if err != nil {
		return err
	}
	// A second guard besides the lock (as recordMigrated has): never write
	// this run's progress into another deployment's record.
	if s.BundleDir != o.st.BundleDir {
		return fmt.Errorf("state.json now describes another deployment (%s, not %s)", s.BundleDir, o.st.BundleDir)
	}
	f(s)
	return s.Save()
}

// replicaProbes is the request that shows a replica of each service serves,
// asked of the replica itself: the same ones proxy.WaitReady asks through
// the proxy.
var replicaProbes = map[string]struct {
	path   string
	status int
}{
	AuthService: {"/.well-known/openid-configuration", http.StatusOK},
	EdgeService: {"/permissions/me", http.StatusUnauthorized},
}

// warmProbesInARow is how many probes in a row a replica has to answer.
const warmProbesInARow = 3

// probeReplica waits until the replica answers its probe warmProbesInARow
// times in a row, on its own PORT (loopback: vps), with host as Host.
func probeReplica(r replica, host string, timeout time.Duration) error {
	p, ok := replicaProbes[r.Base]
	if !ok {
		return nil
	}
	u := url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", r.Ports.Port), Path: p.path}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	inARow := 0
	var last string
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		if host != "" {
			req.Host = host
		}
		resp, err := client.Do(req)
		switch {
		case err != nil:
			inARow, last = 0, err.Error()
		case resp.StatusCode != p.status:
			resp.Body.Close()
			inARow, last = 0, fmt.Sprintf("answered %d, not %d", resp.StatusCode, p.status)
		default:
			resp.Body.Close()
			inARow++
			if inARow >= warmProbesInARow {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s (%s) did not answer %d times in a row within %s: %s", r.Service, u.Path, warmProbesInARow, timeout, last)
}

// RolloutSummary is the rollout under way, for `status`, or "" when none is.
func RolloutSummary(st *state.State) string {
	if !st.RolloutUnderway() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rolling upgrade %s -> %s under way -- `versola rollout %s` continues it\n", st.PreviousVersion, st.Version, st.Version)
	if st.MigratedAt == nil {
		b.WriteString("  migrations: not applied yet\n")
	}
	for _, svc := range []string{AuthService, EdgeService} {
		var parts []string
		slots := st.ActiveSlots(svc)
		sort.Slice(slots, func(i, j int) bool { return slots[i].N < slots[j].N })
		for _, sl := range slots {
			p := fmt.Sprintf("slot %d at %s", sl.N, sl.Version)
			if n, ok := st.Surge[svc]; ok && n == sl.N {
				p += " (stand-in)"
			}
			parts = append(parts, p)
		}
		fmt.Fprintf(&b, "  %s: %s\n", svc, strings.Join(parts, ", "))
	}
	b.WriteString("  (central's version: see the image in the list below)")
	return b.String()
}
