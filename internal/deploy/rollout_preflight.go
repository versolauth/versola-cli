package deploy

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// A stand-in replica is one more JVM next to the running ones, with one
// more connection pool. Before one starts, the rollout checks that the host
// has the memory and Postgres the connections -- the two things that, short,
// turn a rolling upgrade into an outage of the replicas already serving.
// Neither check has a switch to turn it off: the way round a refusal is
// --no-surge (no stand-in), which needs neither.

// standInMemoryFallback is the least a stand-in is assumed to need: one
// replica's budget. Since 0.7.0 the images size the JVM in absolute terms
// (heap 512m with Xms = Xmx, direct memory, metaspace and code cache capped:
// about 848 MiB for auth and 880 MiB for edge, per the Dockerfiles), so a
// replica's footprint no longer depends on the host. Rounded up to 900 MiB.
// Not measured on a real host yet.
const standInMemoryFallback = 900 * 1024 * 1024

// memoryReserve is left free on top of what a stand-in needs: the OS,
// page cache and Postgres must not be squeezed to the last byte.
const memoryReserve = 256 * 1024 * 1024

// memoryProbe is what the memory check reads, a seam for tests.
type memoryProbe interface {
	// available is MemAvailable on the Docker host. readable is false
	// (with why) when this process can't see that host's memory.
	available() (bytes int64, readable bool, why string, err error)
	// usage is the memory a running container of a compose service uses
	// now (docker stats: its cgroup usage minus page cache). found is
	// false when there is no running container to measure.
	usage(service string) (bytes int64, found bool, err error)
}

// standInNeed is what a stand-in of svc needs: the most any running replica
// of svc uses now, but never less than standInMemoryFallback -- a replica
// measured soon after its start has not touched all of its heap yet (Xms is
// committed, not resident), and the stand-in will grow to the full size.
// measured says whether a measurement raised it above that floor.
func standInNeed(p memoryProbe, st *state.State, svc string) (need int64, measured bool, err error) {
	for _, sl := range st.ActiveSlots(svc) {
		if n, ok := st.Surge[svc]; ok && n == sl.N {
			continue
		}
		b, found, err := p.usage(topology.SlotService(svc, sl.N))
		if err != nil {
			return 0, false, err
		}
		if found && b > need {
			need, measured = b, true
		}
	}
	if need < standInMemoryFallback {
		need, measured = standInMemoryFallback, false
	}
	return need, measured, nil
}

// checkStandInMemory refuses when the host can't hold a stand-in of any of
// services next to what runs. It warns (through warn) rather than refuses
// when the host's free memory can't be read at all.
func checkStandInMemory(p memoryProbe, st *state.State, services []string, warn func(string, ...any)) error {
	if len(services) == 0 {
		return nil
	}
	avail, readable, why, err := p.available()
	if !readable {
		return fmt.Errorf("can't check the free memory a stand-in replica needs: %s -- run `versola rollout` on the vps itself (over SSH), or use --no-surge", why)
	}
	if err != nil {
		warn("Warning: couldn't read the free memory (%v) -- not checked whether a stand-in replica fits.\n", err)
		return nil
	}
	for _, svc := range services {
		need, measured, err := standInNeed(p, st, svc)
		if err != nil {
			return err
		}
		if avail >= need+memoryReserve {
			continue
		}
		basis := "what a running " + svc + " replica uses now"
		if !measured {
			basis = "the size of one replica"
		}
		hint := "--no-surge (one replica fewer serves while each is replaced"
		if svc == CentralService {
			hint = "--no-surge (central is then stopped and started in place: the admin console is down until it is ready"
		}
		if replicas := len(st.ActiveSlots(svc)); replicas < 2 && svc != CentralService {
			hint += "; with one replica add --allow-downtime: it is down until the new one is ready"
		}
		return fmt.Errorf("not enough free memory for a stand-in %s replica: it needs about %s (%s) plus %s kept free, and %s is available -- free memory first, or run with %s)",
			svc, mib(need), basis, mib(memoryReserve), mib(avail), hint)
	}
	return nil
}

func mib(b int64) string { return fmt.Sprintf("%d MiB", b/(1024*1024)) }

// standInServices are the services a rollout (or abort) with surge will
// still start a stand-in for: those without one recorded already (a
// running stand-in is counted in what runs). Continuing a rollout forward
// (not an abort), a service whose every replica is recorded at the target
// already is done and gets no stand-in: checking memory for one would
// refuse a rollout that only has its last steps left. (A recorded version
// is not proof -- the engine still replaces a replica its container says
// is not done, and checks memory again before any stand-in it starts.)
// An abort checks every service: replicas that never moved are recorded
// at the version it goes back to and are recreated all the same.
//
// central is in only when centralFronted: a stand-in of central needs the
// proxy in front of central's PORT (see rolloutEngine.centralInPlace).
func standInServices(st *state.State, o RolloutOptions, abort, centralFronted bool) []string {
	if o.NoSurge {
		return nil
	}
	services := []string{AuthService, EdgeService}
	if centralFronted {
		services = rolloutServices
	}
	var out []string
	for _, svc := range services {
		if _, ok := st.Surge[svc]; ok {
			continue
		}
		if !abort && st.RolloutUnderway() && !st.RollingBack && allAt(st.ActiveSlots(svc), st.Version) {
			continue
		}
		out = append(out, svc)
	}
	return out
}

func allAt(slots []state.Slot, version string) bool {
	for _, sl := range slots {
		if sl.Version != version {
			return false
		}
	}
	return true
}

// ---- Postgres connections ----

// pgCheckImage runs psql for the connection check. The same major version
// as local's Postgres; psql talks to older servers too.
const pgCheckImage = "postgres:18-alpine"

// pgMargin is kept free on top of a stand-in's pool.
const pgMargin = 3

// pgTarget is what the connection check needs from a service's config,
// read from the bundle: no secret among it.
type pgTarget struct {
	host, port, db, user, sslmode string
	// rootCert is the CA file the service verifies Postgres against, as a
	// path inside its container: the URL's sslrootcert, else the config's
	// ssl-root-cert (what versola-tools writes for --postgres-ca), else
	// empty (the JVM's trust store).
	rootCert string
	// caFile is rootCert on this host, found in the bundle by
	// checkPgConnections; empty when there is none.
	caFile string
	// pool is the service's connection pool size (maximum-pool-size;
	// minimum-idle equals it in Versola's configs, so a replica holds all
	// of it from its start).
	pool int
}

var (
	pgBlockRe = regexp.MustCompile(`(?s)\npostgres\s*\{(.*?)\n\}`)
	// Keys at the start of a line: `url` must not match notifications-url.
	pgURLRe  = regexp.MustCompile(`(?m)^\s*url\s*[=:]\s*"([^"]*)"`)
	pgUserRe = regexp.MustCompile(`(?m)^\s*user\s*[=:]\s*"([^"]*)"`)
	pgPoolRe = regexp.MustCompile(`(?m)^\s*maximum-pool-size\s*[=:]\s*(\d+)`)
	pgCARe   = regexp.MustCompile(`(?m)^\s*ssl-root-cert\s*[=:]\s*"([^"]*)"`)
)

// pgCAContainerDir is where every service sees the bundle's postgres-ca
// directory (versola-tools' compose template mounts ./postgres-ca there
// read-only); --postgres-ca's file is ca.crt in it.
const (
	pgCAContainerDir = "/etc/versola/postgres-ca"
	pgCABundleDir    = "postgres-ca"
)

// parsePgTarget reads the postgres block of a service's HOCON config as
// versola-tools writes it. Only a jdbc:postgresql URL with a literal host is
// understood; anything else is an error (the check then only warns).
func parsePgTarget(conf string) (pgTarget, error) {
	m := pgBlockRe.FindStringSubmatch("\n" + conf)
	if m == nil {
		return pgTarget{}, fmt.Errorf("no postgres block")
	}
	block := m[1]
	u := pgURLRe.FindStringSubmatch(block)
	if u == nil || !strings.HasPrefix(u[1], "jdbc:postgresql://") {
		return pgTarget{}, fmt.Errorf("no jdbc:postgresql url")
	}
	parsed, err := url.Parse(strings.TrimPrefix(u[1], "jdbc:"))
	if err != nil || parsed.Hostname() == "" {
		return pgTarget{}, fmt.Errorf("can't read the postgres url")
	}
	q := parsed.Query()
	t := pgTarget{host: parsed.Hostname(), port: parsed.Port(), db: strings.TrimPrefix(parsed.Path, "/"), sslmode: q.Get("sslmode"), rootCert: q.Get("sslrootcert")}
	if t.rootCert == "" {
		if c := pgCARe.FindStringSubmatch(block); c != nil {
			t.rootCert = c[1]
		}
	}
	if t.port == "" {
		t.port = "5432"
	}
	if us := pgUserRe.FindStringSubmatch(block); us != nil {
		t.user = us[1]
	}
	if t.user == "" || strings.Contains(t.user, "${") || t.db == "" {
		return pgTarget{}, fmt.Errorf("the postgres user or database is not written out")
	}
	if p := pgPoolRe.FindStringSubmatch(block); p != nil {
		t.pool, _ = strconv.Atoi(p[1])
	}
	if t.pool == 0 {
		t.pool = 10 // HikariCP's default
	}
	return t, nil
}

// pgListenConns is the dedicated LISTEN connection a service holds besides
// its pool (edge and central, for their caches).
var pgListenConns = map[string]int{EdgeService: 1, CentralService: 1}

// pgHeadroomQuery is how many more client connections the server takes:
// max_connections, less the slots kept for superusers (and, from Postgres
// 16, for roles with pg_use_reserved_connections), less the client
// backends open now (this check's own included). A role that is not a
// superuser sees backend_type only for its own sessions; another role's
// session shows as a row with a user and no type, and counts too (a
// background worker running as a role may be counted with them: on the
// safe side).
const pgHeadroomQuery = `SELECT current_setting('max_connections')::int` +
	` - current_setting('superuser_reserved_connections')::int` +
	` - coalesce(nullif(current_setting('reserved_connections', true), '')::int, 0)` +
	` - (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' OR (backend_type IS NULL AND usesysid IS NOT NULL))::int`

// pgHeadroom runs the query with psql in a throwaway container on the host's
// network. The password stays out of this process: docker reads it from the
// service's secrets file into the container's environment (--env-file),
// and the shell there hands it to psql. The connection parameters go in as
// environment variables, never into a command line.
type pgRunner func(t pgTarget, secretsFile string) (int, error)

// dockerPgHeadroom is the runner before a rollout starts: time to pull the
// image the first time. pgHeadroomWithin(time.Minute) is the one inside the
// engine, where a second Ctrl-C waits for the check under way.
func dockerPgHeadroom(t pgTarget, secretsFile string) (int, error) {
	return pgHeadroomWithin(3*time.Minute)(t, secretsFile)
}

func pgHeadroomWithin(timeout time.Duration) pgRunner {
	return func(t pgTarget, secretsFile string) (int, error) { return runPgHeadroom(t, secretsFile, timeout) }
}

// pgVerifies reports whether sslmode makes the client check the server's
// certificate. Empty is the services' default, verify-full.
func pgVerifies(sslmode string) bool {
	return sslmode == "" || sslmode == "verify-ca" || sslmode == "verify-full"
}

// pgHeadroomArgs is the docker command line of the check. TLS is set up as
// the services connect: sslmode from the URL, else verify-full; verified
// against the service's own CA file (mounted read-only at the path the
// service sees it at) when it has one, else against the system's CAs (the
// services: the JVM's trust store). No root certificate is passed for the
// modes that do not verify: libpq, unlike the services' JDBC driver, would
// turn require into verify-ca once it has one.
func pgHeadroomArgs(t pgTarget, secretsFile string) []string {
	args := []string{"run", "--rm", "--pull", "missing", "--network", "host", "--env-file", secretsFile,
		"-e", "PGHOST=" + t.host, "-e", "PGPORT=" + t.port, "-e", "PGDATABASE=" + t.db, "-e", "PGUSER=" + t.user,
		"-e", "PGCONNECT_TIMEOUT=10", "-e", "PGAPPNAME=versola-rollout-preflight"}
	mode := t.sslmode
	if mode == "" {
		mode = "verify-full"
	}
	args = append(args, "-e", "PGSSLMODE="+mode)
	switch {
	case !pgVerifies(mode):
	case t.caFile != "":
		in := pgCAContainerDir + "/ca.crt"
		args = append(args, "-v", t.caFile+":"+in+":ro", "-e", "PGSSLROOTCERT="+in)
	case mode == "verify-full":
		// libpq accepts "system" only with verify-full; verify-ca
		// without a CA file of its own fails the check, which then warns.
		args = append(args, "-e", "PGSSLROOTCERT=system")
	}
	return append(args, pgCheckImage, "sh", "-c", `PGPASSWORD="$POSTGRES_PASSWORD" exec psql -XAtq -v ON_ERROR_STOP=1 -c "$0"`, pgHeadroomQuery)
}

func runPgHeadroom(t pgTarget, secretsFile string, timeout time.Duration) (int, error) {
	args := pgHeadroomArgs(t, secretsFile)
	out, stderr, err := docker.OutputWithin(timeout, args...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 125 {
			// docker itself failed (pulling the image, or reading the
			// secrets file -- whose parse errors can quote the file):
			// its message is not repeated.
			return 0, fmt.Errorf("docker couldn't run the check (pulling %s, or starting it, failed)", pgCheckImage)
		}
		// psql's own errors (refused, authentication failed, TLS) name
		// the host and the user, never the password.
		return 0, fmt.Errorf("%v%s", err, firstLine(stderr))
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unexpected answer from psql")
	}
	return n, nil
}

// firstLine is the last non-empty line of psql's or docker's error output,
// as ": <line>" -- the reason, without the pull progress. psql does not
// print the password in its errors.
func firstLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return ": " + l
		}
	}
	return ""
}

// pgAsker words the check for what it is about: a stand-in that goes again, or
// the replicas `replica add` leaves running.
type pgAsker struct {
	announce string // "Checking that Postgres has the connections for <announce>..."
	needs    string // "the Postgres connections <needs> are not checked"
	opens    string // format of the sentence with how many connections are needed
}

var (
	standInAsker    = pgAsker{"a stand-in replica", "a stand-in needs", "A stand-in opens %d."}
	newReplicaAsker = pgAsker{"the new replicas", "the new replicas need", "The new replicas open %d."}
)

// checkPgConnections refuses when Postgres can't take the pool of a stand-in
// of any of services (they run one after another: the largest counts). It
// only warns when the check itself can't run -- a config it can't read, the
// image out of reach, a server it can't log in to: the rollout's own
// readiness checks still stop a stand-in that can't connect.
func checkPgConnections(bundleDir string, services []string, run pgRunner, warn func(string, ...any)) error {
	headroom, need, needFor, ok := pgHeadroomFor(bundleDir, services, 1, standInAsker, run, warn)
	if !ok || headroom >= need+pgMargin {
		return nil
	}
	return fmt.Errorf("Postgres takes %d more connections and a stand-in %s replica opens %d (plus %d kept free): the replicas already serving would start failing to reconnect -- raise max_connections, or run with --no-surge",
		headroom, needFor, need, pgMargin)
}

// checkPgForReplicas is the check of `replica add`: count more replicas of
// service open their pools together and stay open, so Postgres has to take
// all of them, not the largest one as for a stand-in that goes again. Like
// checkPgConnections it only warns when it can't ask.
func checkPgForReplicas(bundleDir, service string, count int, run pgRunner, warn func(string, ...any)) error {
	headroom, need, _, ok := pgHeadroomFor(bundleDir, []string{service}, count, newReplicaAsker, run, warn)
	if !ok || headroom >= need+pgMargin {
		return nil
	}
	return fmt.Errorf("Postgres takes %d more connections and %d more %s replica(s) open %d (plus %d kept free): the replicas already serving would start failing to reconnect -- raise max_connections, or add fewer replicas",
		headroom, count, service, need, pgMargin)
}

// pgHeadroomFor asks Postgres how many connections are free and what
// replicas of the services need: the largest of them, times replicas. ok is
// false when the check could not be made (a warning was given), or there is
// nothing to check.
func pgHeadroomFor(bundleDir string, services []string, replicas int, who pgAsker, run pgRunner, warn func(string, ...any)) (headroom, need int, needFor string, ok bool) {
	if len(services) == 0 {
		return 0, 0, "", false
	}
	var target pgTarget
	var secrets string
	per := 0
	for _, svc := range services {
		b, err := os.ReadFile(filepath.Join(bundleDir, svc+".conf"))
		if err != nil {
			warn("Warning: couldn't read %s.conf (%v) -- the Postgres connections %s are not checked.\n", svc, err, who.needs)
			return 0, 0, "", false
		}
		t, err := parsePgTarget(string(b))
		if err != nil {
			warn("Warning: couldn't read the Postgres settings of %s (%v) -- the connections %s are not checked.\n", svc, err, who.needs)
			return 0, 0, "", false
		}
		if n := t.pool + pgListenConns[svc]; n > per {
			per, needFor, target, secrets = n, svc, t, filepath.Join(bundleDir, svc+".secrets.env")
		}
	}
	need = per * replicas
	if target.rootCert != "" && pgVerifies(target.sslmode) {
		ca, err := pgCAOnHost(bundleDir, target.rootCert)
		if err != nil {
			warn("Warning: %v -- the Postgres connections %s are not checked.\n", err, who.needs)
			return 0, 0, "", false
		}
		target.caFile = ca
	}
	fmt.Printf("Checking that Postgres has the connections for %s...\n", who.announce)
	headroom, err := run(target, secrets)
	if err != nil {
		warn("Warning: couldn't ask Postgres how many connections are free (%v) -- not checked. "+who.opens+"\n", err, need)
		return 0, 0, "", false
	}
	return headroom, need, needFor, true
}

// pgCAOnHost maps the CA file a service names (a path in its container) to
// the file in the bundle it is mounted from.
func pgCAOnHost(bundleDir, inContainer string) (string, error) {
	rel, ok := strings.CutPrefix(inContainer, pgCAContainerDir+"/")
	if !ok || rel == "" || strings.Contains(rel, "..") {
		return "", fmt.Errorf("the Postgres CA file %s is not one versola-cli mounts (%s/...)", inContainer, pgCAContainerDir)
	}
	abs, err := filepath.Abs(filepath.Join(bundleDir, pgCABundleDir, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("the Postgres CA file %s is not in the bundle (%s)", inContainer, abs)
	}
	return abs, nil
}

// ---- the real probes ----

type hostMemory struct{ ops dockerRolloutOps }

func (h hostMemory) available() (int64, bool, string, error) { return checks.HostMemoryAvailable() }

func (h hostMemory) usage(service string) (int64, bool, error) {
	ids, err := h.ops.containersOf(service)
	if err != nil || len(ids) != 1 {
		return 0, false, nil
	}
	if running, err := docker.IsRunning(ids[0]); err != nil || !running {
		return 0, false, nil
	}
	// A failed or invalid reading ("-- / --": the container stopped in
	// between) is "not measured", not an error: the floor applies.
	out, _, err := docker.OutputWithin(30*time.Second, "stats", "--no-stream", "--format", "{{.MemUsage}}", ids[0])
	if err != nil {
		return 0, false, nil
	}
	b, err := parseMemUsage(string(out))
	if err != nil {
		return 0, false, nil
	}
	return b, true, nil
}

// parseMemUsage reads the first figure of docker stats' MemUsage ("812.4MiB
// / 3.8GiB").
func parseMemUsage(s string) (int64, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(s), "/")
	first = strings.TrimSpace(first)
	units := []struct {
		suffix string
		mult   float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(first, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(first, u.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("unexpected memory figure %q", first)
			}
			return int64(v * u.mult), nil
		}
	}
	return 0, fmt.Errorf("unexpected memory figure %q", first)
}
