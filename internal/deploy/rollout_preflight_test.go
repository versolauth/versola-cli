package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

const miB = 1024 * 1024

type fakeMemory struct {
	avail    int64
	readable bool
	why      string
	err      error
	used     map[string]int64 // compose service -> bytes; absent = not running
}

func (f fakeMemory) available() (int64, bool, string, error) {
	return f.avail, f.readable, f.why, f.err
}

func (f fakeMemory) usage(service string) (int64, bool, error) {
	b, ok := f.used[service]
	return b, ok, nil
}

func noWarn(t *testing.T) func(string, ...any) {
	return func(f string, a ...any) { t.Errorf("unexpected warning: "+f, a...) }
}

func plainState(auth, edge int) *state.State {
	st := &state.State{Target: "vps", Version: fromV}
	var as, es []state.Slot
	for n := 1; n <= auth; n++ {
		as = append(as, state.Slot{N: n, Version: fromV})
	}
	for n := 1; n <= edge; n++ {
		es = append(es, state.Slot{N: n, Version: fromV})
	}
	st.SetSlots(AuthService, as)
	st.SetSlots(EdgeService, es)
	return st
}

func TestStandInMemory(t *testing.T) {
	st := plainState(2, 1)
	used := map[string]int64{"auth": 1100 * miB, "auth-2": 1150 * miB, "edge": 1300 * miB}

	// Enough: the largest running replica plus the reserve fits.
	if err := checkStandInMemory(fakeMemory{avail: 1600 * miB, readable: true, used: used}, st, []string{AuthService, EdgeService}, noWarn(t)); err != nil {
		t.Errorf("enough memory: %v", err)
	}
	// Short for edge (1300 + 256 > 1450; auth's 1150 + 256 fits): refused, with the numbers and the way round.
	err := checkStandInMemory(fakeMemory{avail: 1450 * miB, readable: true, used: used}, st, []string{AuthService, EdgeService}, noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "stand-in edge") || !strings.Contains(err.Error(), "1300 MiB") ||
		!strings.Contains(err.Error(), "1450 MiB is available") || !strings.Contains(err.Error(), "--no-surge") || !strings.Contains(err.Error(), "--allow-downtime") {
		t.Errorf("short for edge: %v", err)
	}
	// auth has two replicas: no --allow-downtime hint.
	err = checkStandInMemory(fakeMemory{avail: 1300 * miB, readable: true, used: used}, st, []string{AuthService}, noWarn(t))
	if err == nil || strings.Contains(err.Error(), "--allow-downtime") {
		t.Errorf("short for auth: %v", err)
	}
	// Nothing to measure: the size of one replica (900 MiB) is assumed.
	err = checkStandInMemory(fakeMemory{avail: 1100 * miB, readable: true, used: map[string]int64{}}, st, []string{AuthService}, noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "the size of one replica") || !strings.Contains(err.Error(), "900 MiB") {
		t.Errorf("nothing measured: %v", err)
	}
	if err := checkStandInMemory(fakeMemory{avail: 1200 * miB, readable: true, used: map[string]int64{}}, st, []string{AuthService}, noWarn(t)); err != nil {
		t.Errorf("nothing measured, enough for the floor: %v", err)
	}
	// A replica measured soon after its start (heap not touched yet) does
	// not lower the need below one replica's size.
	need, measured, err := standInNeed(fakeMemory{used: map[string]int64{"auth": 350 * miB}}, plainState(1, 1), AuthService)
	if err != nil || measured || need != standInMemoryFallback {
		t.Errorf("fresh replica: need %d measured %v %v", need, measured, err)
	}
	// A recorded stand-in is not taken for one of the operator's replicas.
	st2 := plainState(2, 1)
	st2.Surge = map[string]int{AuthService: 2}
	need, measured, err = standInNeed(fakeMemory{used: map[string]int64{"auth": 1100 * miB, "auth-2": 2000 * miB}}, st2, AuthService)
	if err != nil || !measured || need != 1100*miB {
		t.Errorf("need with a stand-in recorded: %d %v %v", need, measured, err)
	}
	// Another host's memory: refused, not guessed.
	err = checkStandInMemory(fakeMemory{readable: false, why: `Docker is pointed at "ssh://vps"`}, st, []string{AuthService}, noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "ssh://vps") {
		t.Errorf("remote docker: %v", err)
	}
	// /proc/meminfo unreadable: a warning, not a refusal.
	warned := false
	if err := checkStandInMemory(fakeMemory{readable: true, err: errors.New("no meminfo")}, st, []string{AuthService}, func(string, ...any) { warned = true }); err != nil || !warned {
		t.Errorf("unreadable: err %v, warned %v", err, warned)
	}
	// No stand-in to start: nothing checked, nothing read.
	if err := checkStandInMemory(fakeMemory{readable: false}, st, nil, noWarn(t)); err != nil {
		t.Errorf("no stand-in: %v", err)
	}
}

func TestStandInServices(t *testing.T) {
	st := plainState(1, 1)
	if got := standInServices(st, RolloutOptions{NoSurge: true}, false, false, false); got != nil {
		t.Errorf("no surge: %v", got)
	}
	if got := fmt.Sprint(standInServices(st, RolloutOptions{}, false, false, false)); got != "[auth edge]" {
		t.Errorf("a new rollout: %v", got)
	}
	under := preparedState(old(1), old(1))
	under.Surge = map[string]int{AuthService: 2}
	if got := standInServices(under, RolloutOptions{}, false, false, false); len(got) != 1 || got[0] != EdgeService {
		t.Errorf("auth's stand-in already runs: %v", got)
	}
	// Continuing a rollout whose auth is done (recorded at the target):
	// no stand-in for auth any more, so no check for one.
	p := preparedState([]state.Slot{{N: 1, Version: toV}}, old(1))
	if got := fmt.Sprint(standInServices(p, RolloutOptions{}, false, false, false)); got != "[edge]" {
		t.Errorf("auth done: %v", got)
	}
	// Everything recorded at the target (cut off in finish): nothing.
	p.SetSlots(EdgeService, []state.Slot{{N: 1, Version: toV}})
	if got := standInServices(p, RolloutOptions{}, false, false, false); got != nil {
		t.Errorf("all done: %v", got)
	}
	// An abort checks every service: untouched replicas are recorded at the
	// version it goes back to and still get moved behind a stand-in.
	if got := fmt.Sprint(standInServices(p, RolloutOptions{}, true, false, false)); got != "[auth edge]" {
		t.Errorf("abort: %v", got)
	}
	// An abort that goes back from the previous bundle leaves what never
	// moved alone: edge (recorded at the version it goes back to) starts no
	// stand-in, auth (moved on) does.
	moved := preparedState([]state.Slot{{N: 1, Version: toV}}, old(1))
	moved.SetSlots(CentralService, []state.Slot{{N: 1, Version: fromV}})
	if got := fmt.Sprint(standInServices(moved, RolloutOptions{}, true, true, true)); got != "[auth]" {
		t.Errorf("abort from the previous bundle: %v", got)
	}
	moved.SetSlots(CentralService, []state.Slot{{N: 1, Version: toV}})
	if got := fmt.Sprint(standInServices(moved, RolloutOptions{}, true, true, true)); got != "[central auth]" {
		t.Errorf("abort from the previous bundle, central moved: %v", got)
	}
	// An abort under way already (resumed) can't tell which way it was
	// prepared: it checks every service.
	moved.RollingBack = true
	if got := fmt.Sprint(standInServices(moved, RolloutOptions{}, true, true, false)); got != "[auth edge]" {
		t.Errorf("resumed abort: %v", got)
	}
	// Behind the proxy's front, central gets a stand-in too -- first.
	if got := fmt.Sprint(standInServices(st, RolloutOptions{}, false, false, true)); got != "[central auth edge]" {
		t.Errorf("central fronted: %v", got)
	}
	if got := fmt.Sprint(standInServices(p, RolloutOptions{}, true, false, true)); got != "[central auth edge]" {
		t.Errorf("abort, central fronted: %v", got)
	}
}

// The engine checks memory again right before each stand-in; short for the
// second one, it stops with the first service done and nothing half-made.
func TestRolloutChecksMemoryBeforeEachStandIn(t *testing.T) {
	w := newWorld(t, preparedState(old(1), old(1)))
	w.memShort = map[string]bool{EdgeService: true}
	err := engineFor(w, true, false).run()
	if err == nil || !strings.Contains(err.Error(), "stand-in edge") {
		t.Fatalf("error: %v", err)
	}
	if fmt.Sprint(w.memChecked) != "[auth edge]" {
		t.Errorf("checked %v", w.memChecked)
	}
	st := w.load()
	if st.Surge != nil || w.containers["edge-2"] != nil {
		t.Errorf("a stand-in was recorded or started: surge %v", st.Surge)
	}
	if w.containers["auth"].image != imageOf(AuthService, toV) {
		t.Error("auth should be done before edge's check")
	}
	if len(w.violations) > 0 {
		t.Errorf("violations: %v", w.violations)
	}
	// The way round: the same rollout without a stand-in for edge would
	// need two edge replicas; with downtime accepted it finishes.
	w.minUp = map[string]int{}
	if err := engineFor(w, false, true).run(); err != nil {
		t.Fatal(err)
	}
	w.violations = nil
	w.done(t, []int{1}, []int{1})
}

func TestParseMemUsage(t *testing.T) {
	for in, want := range map[string]int64{
		"812.5MiB / 3.8GiB": int64(812.5 * miB),
		"1.25GiB / 3.8GiB":  int64(1.25 * 1024 * miB),
		"900kB / 1GB":       900000,
		"0B / 0B":           0,
	} {
		if got, err := parseMemUsage(in); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	if _, err := parseMemUsage("--"); err == nil {
		t.Error("garbage accepted")
	}
}

const authConf = `# Generated by versola-tools
auth {
  url = "https://id.example.com"
}

postgres {
  url = "jdbc:postgresql://127.0.0.1:5432/auth?currentSchema=auth&sslmode=disable"
  user = "versola_app"
  password = ${POSTGRES_PASSWORD}
  maximum-pool-size = 10
  minimum-idle = 10
}
`

func TestParsePgTarget(t *testing.T) {
	got, err := parsePgTarget(authConf)
	if err != nil {
		t.Fatal(err)
	}
	want := pgTarget{host: "127.0.0.1", port: "5432", db: "auth", user: "versola_app", sslmode: "disable", pool: 10}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// notifications-url before url: the pool's url is still the one read.
	withNotify := strings.Replace(authConf, "  url = ", "  notifications-url = \"jdbc:postgresql://10.0.0.9:5432/auth\"\n  url = ", 1)
	if got, err := parsePgTarget(withNotify); err != nil || got.host != "127.0.0.1" {
		t.Errorf("notifications-url first: %+v %v", got, err)
	}
	remote := strings.Replace(authConf, "127.0.0.1:5432/auth?currentSchema=auth&sslmode=disable", "db.example.com/auth?currentSchema=auth", 1)
	remote = strings.Replace(remote, "maximum-pool-size = 10", "maximum-pool-size = 25", 1)
	got, err = parsePgTarget(remote)
	if err != nil || got.host != "db.example.com" || got.port != "5432" || got.sslmode != "" || got.pool != 25 {
		t.Errorf("remote: %+v %v", got, err)
	}
	for name, conf := range map[string]string{
		"no block":       "auth {}\n",
		"user from env":  strings.Replace(authConf, `user = "versola_app"`, `user = ${PGUSER}`, 1),
		"not postgresql": strings.Replace(authConf, "jdbc:postgresql://", "jdbc:mysql://", 1),
		"no database":    strings.Replace(authConf, "5432/auth?", "5432/?", 1),
	} {
		if _, err := parsePgTarget(conf); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func pgBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	edgeConf := strings.Replace(strings.Replace(authConf, "/auth?currentSchema=auth", "/edge?currentSchema=edge", 1), "maximum-pool-size = 10", "maximum-pool-size = 12", 1)
	for name, body := range map[string]string{"auth.conf": authConf, "edge.conf": edgeConf} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPgConnections(t *testing.T) {
	dir := pgBundle(t)
	var asked pgTarget
	var file string
	run := func(headroom int, err error) pgRunner {
		return func(tg pgTarget, secrets string) (int, error) {
			asked, file = tg, secrets
			return headroom, err
		}
	}
	// edge's pool (12) + its LISTEN connection is the largest: 13 + 3 kept free.
	if err := checkPgConnections(dir, []string{AuthService, EdgeService}, run(16, nil), noWarn(t)); err != nil {
		t.Errorf("enough: %v", err)
	}
	if asked.db != "edge" || file != filepath.Join(dir, "edge.secrets.env") {
		t.Errorf("asked as %+v with %s", asked, file)
	}
	err := checkPgConnections(dir, []string{AuthService, EdgeService}, run(15, nil), noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "takes 15 more") || !strings.Contains(err.Error(), "edge replica opens 13") || !strings.Contains(err.Error(), "max_connections") {
		t.Errorf("short: %v", err)
	}
	// auth alone: 10 + 3.
	if err := checkPgConnections(dir, []string{AuthService}, run(13, nil), noWarn(t)); err != nil {
		t.Errorf("auth alone: %v", err)
	}
	// The check can't run: a warning, never a refusal.
	for name, c := range map[string]struct {
		dir string
		run pgRunner
	}{
		"psql failed": {dir, run(0, errors.New("password authentication failed"))},
		"no config":   {t.TempDir(), run(100, nil)},
	} {
		warned := false
		if err := checkPgConnections(c.dir, []string{AuthService}, c.run, func(string, ...any) { warned = true }); err != nil || !warned {
			t.Errorf("%s: err %v, warned %v", name, err, warned)
		}
	}
	// Nothing to start: not asked at all.
	if err := checkPgConnections(dir, nil, func(pgTarget, string) (int, error) { t.Error("asked"); return 0, nil }, noWarn(t)); err != nil {
		t.Error(err)
	}
}

// The query is passed to psql as one argument ($0), never spliced into the
// shell script, and no connection parameter reaches a command line.
func TestPgHeadroomQueryShape(t *testing.T) {
	if strings.Contains(pgHeadroomQuery, `"`) {
		t.Error("the query must not contain double quotes")
	}
	for _, want := range []string{"max_connections", "superuser_reserved_connections", "reserved_connections', true", "client backend", "backend_type IS NULL AND usesysid IS NOT NULL"} {
		if !strings.Contains(pgHeadroomQuery, want) {
			t.Errorf("query lacks %q", want)
		}
	}
}

func TestParsePgTargetRootCert(t *testing.T) {
	remote := strings.Replace(authConf, "127.0.0.1:5432/auth?currentSchema=auth&sslmode=disable", "db.example.com:6432/auth?currentSchema=auth", 1)
	withCA := strings.Replace(remote, `  user = "versola_app"`, "  user = \"versola_app\"\n  ssl-root-cert = \"/etc/versola/postgres-ca/ca.crt\"", 1)
	if got, err := parsePgTarget(withCA); err != nil || got.rootCert != "/etc/versola/postgres-ca/ca.crt" || got.sslmode != "" {
		t.Errorf("ssl-root-cert: %+v %v", got, err)
	}
	// The URL's sslrootcert wins, as in the services.
	inURL := strings.Replace(withCA, "currentSchema=auth\"", "currentSchema=auth&sslrootcert=/etc/versola/postgres-ca/other.pem\"", 1)
	if got, err := parsePgTarget(inURL); err != nil || got.rootCert != "/etc/versola/postgres-ca/other.pem" {
		t.Errorf("sslrootcert in the url: %+v %v", got, err)
	}
	if got, err := parsePgTarget(remote); err != nil || got.rootCert != "" {
		t.Errorf("no CA: %+v %v", got, err)
	}
}

// envOf returns the value of -e NAME=... in docker args, and whether it is set.
func envOf(args []string, name string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" && strings.HasPrefix(args[i+1], name+"=") {
			return strings.TrimPrefix(args[i+1], name+"="), true
		}
	}
	return "", false
}

func mountsOf(args []string) []string {
	var m []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-v" {
			m = append(m, args[i+1])
		}
	}
	return m
}

func TestPgHeadroomArgs(t *testing.T) {
	base := pgTarget{host: "db.example.com", port: "6432", db: "auth", user: "versola_app", pool: 10}
	ca := "/home/v/.versola/active/bundle/postgres-ca/ca.crt"
	for name, c := range map[string]struct {
		sslmode, caFile string
		mode, root      string // root "" = PGSSLROOTCERT not set
		mount           string
	}{
		"default, no CA":     {"", "", "verify-full", "system", ""},
		"default, own CA":    {"", ca, "verify-full", "/etc/versola/postgres-ca/ca.crt", ca + ":/etc/versola/postgres-ca/ca.crt:ro"},
		"verify-full, no CA": {"verify-full", "", "verify-full", "system", ""},
		"verify-ca, own CA":  {"verify-ca", ca, "verify-ca", "/etc/versola/postgres-ca/ca.crt", ca + ":/etc/versola/postgres-ca/ca.crt:ro"},
		"verify-ca, no CA":   {"verify-ca", "", "verify-ca", "", ""},
		"require, own CA":    {"require", ca, "require", "", ""},
		"disable":            {"disable", "", "disable", "", ""},
	} {
		tg := base
		tg.sslmode, tg.caFile = c.sslmode, c.caFile
		args := pgHeadroomArgs(tg, "/b/auth.secrets.env")
		if got, _ := envOf(args, "PGSSLMODE"); got != c.mode {
			t.Errorf("%s: PGSSLMODE %q, want %q", name, got, c.mode)
		}
		got, set := envOf(args, "PGSSLROOTCERT")
		if c.root == "" && set || c.root != "" && got != c.root {
			t.Errorf("%s: PGSSLROOTCERT %q (set %v), want %q", name, got, set, c.root)
		}
		mounts := mountsOf(args)
		if c.mount == "" && len(mounts) > 0 || c.mount != "" && (len(mounts) != 1 || mounts[0] != c.mount) {
			t.Errorf("%s: mounts %v, want %q", name, mounts, c.mount)
		}
		// The image and the script come last, after every option.
		if i := indexOf(args, pgCheckImage); i < 0 || i != len(args)-5 {
			t.Errorf("%s: image at %d of %v", name, i, args)
		}
	}
}

func TestPgConnectionsFindsTheBundleCA(t *testing.T) {
	dir := t.TempDir()
	remote := strings.Replace(authConf, "127.0.0.1:5432/auth?currentSchema=auth&sslmode=disable", "db.example.com:6432/auth?currentSchema=auth", 1)
	withCA := strings.Replace(remote, `  user = "versola_app"`, "  user = \"versola_app\"\n  ssl-root-cert = \"/etc/versola/postgres-ca/ca.crt\"", 1)
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var asked pgTarget
	run := func(tg pgTarget, _ string) (int, error) { asked = tg; return 100, nil }
	warnedWith := func(msg *string) func(string, ...any) {
		return func(f string, a ...any) { *msg = fmt.Sprintf(f, a...) }
	}

	// The CA is not in the bundle: warned, not asked, not refused.
	write("auth.conf", withCA)
	var msg string
	asked = pgTarget{}
	if err := checkPgConnections(dir, []string{AuthService}, run, warnedWith(&msg)); err != nil || !strings.Contains(msg, "not in the bundle") || asked.host != "" {
		t.Errorf("missing CA: err %v, warning %q, asked %+v", err, msg, asked)
	}

	write("postgres-ca/ca.crt", "-----BEGIN CERTIFICATE-----\n")
	if err := checkPgConnections(dir, []string{AuthService}, run, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "postgres-ca", "ca.crt"); asked.caFile != want {
		t.Errorf("caFile %q, want %q", asked.caFile, want)
	}

	// A CA path versola-cli does not mount: warned.
	write("auth.conf", strings.Replace(withCA, "/etc/versola/postgres-ca/ca.crt", "/opt/ca.pem", 1))
	msg = ""
	if err := checkPgConnections(dir, []string{AuthService}, run, warnedWith(&msg)); err != nil || !strings.Contains(msg, "not one versola-cli mounts") {
		t.Errorf("foreign CA path: err %v, warning %q", err, msg)
	}
	write("auth.conf", strings.Replace(withCA, "/etc/versola/postgres-ca/ca.crt", "/etc/versola/postgres-ca/../../../etc/shadow", 1))
	msg = ""
	if err := checkPgConnections(dir, []string{AuthService}, run, warnedWith(&msg)); err != nil || !strings.Contains(msg, "not one versola-cli mounts") {
		t.Errorf("dot-dot CA path: err %v, warning %q", err, msg)
	}

	// Modes that do not verify: the CA is not looked for at all; verify-ca
	// looks for it like verify-full.
	for mode, wantCA := range map[string]bool{"disable": false, "require": false, "verify-ca": true} {
		conf := strings.Replace(withCA, "currentSchema=auth\"", "currentSchema=auth&sslmode="+mode+"\"", 1)
		if !wantCA {
			conf = strings.Replace(conf, "/etc/versola/postgres-ca/ca.crt", "/opt/ca.pem", 1)
		}
		write("auth.conf", conf)
		asked = pgTarget{}
		if err := checkPgConnections(dir, []string{AuthService}, run, noWarn(t)); err != nil || (asked.caFile != "") != wantCA || asked.host == "" {
			t.Errorf("%s: err %v, asked %+v", mode, err, asked)
		}
	}
}

// `replica add` starts the replicas together and keeps them: Postgres has to
// take every pool, not the largest one as for a stand-in that goes again.
func TestPgConnectionsForNewReplicas(t *testing.T) {
	dir := pgBundle(t)
	run := func(headroom int) pgRunner {
		return func(pgTarget, string) (int, error) { return headroom, nil }
	}
	// auth: 10 per replica, two of them, 3 kept free.
	if err := checkPgForReplicas(dir, AuthService, 2, run(23), noWarn(t)); err != nil {
		t.Errorf("enough: %v", err)
	}
	err := checkPgForReplicas(dir, AuthService, 2, run(22), noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "takes 22 more") || !strings.Contains(err.Error(), "2 more auth replica(s) open 20") || strings.Contains(err.Error(), "stand-in") || strings.Contains(err.Error(), "--no-surge") {
		t.Errorf("short: %v", err)
	}
	// edge: pool 12 + its LISTEN connection, per replica.
	if err := checkPgForReplicas(dir, EdgeService, 2, run(29), noWarn(t)); err != nil {
		t.Errorf("edge enough: %v", err)
	}
	if err := checkPgForReplicas(dir, EdgeService, 2, run(28), noWarn(t)); err == nil {
		t.Error("edge short: accepted")
	}
	// The check can't run: a warning that names the new replicas, never a refusal.
	var msg string
	warned := func(f string, a ...any) { msg = fmt.Sprintf(f, a...) }
	if err := checkPgForReplicas(dir, AuthService, 1, func(pgTarget, string) (int, error) { return 0, errors.New("refused") }, warned); err != nil || !strings.Contains(msg, "The new replicas open 10") {
		t.Errorf("psql failed: %v, warning %q", err, msg)
	}
	msg = ""
	if err := checkPgForReplicas(t.TempDir(), AuthService, 1, run(100), warned); err != nil || !strings.Contains(msg, "the new replicas need are not checked") {
		t.Errorf("no config: %v, warning %q", err, msg)
	}
}

// The stand-in check's warnings read as they did before `replica add` shared
// its code.
func TestPgConnectionsStandInWarnings(t *testing.T) {
	dir := pgBundle(t)
	var msg string
	warned := func(f string, a ...any) { msg = fmt.Sprintf(f, a...) }
	if err := checkPgConnections(dir, []string{AuthService}, func(pgTarget, string) (int, error) { return 0, errors.New("refused") }, warned); err != nil || !strings.HasSuffix(msg, "not checked. A stand-in opens 10.\n") {
		t.Errorf("psql failed: %v, warning %q", err, msg)
	}
	if err := checkPgConnections(t.TempDir(), []string{AuthService}, nil, warned); err != nil || !strings.HasSuffix(msg, "-- the Postgres connections a stand-in needs are not checked.\n") {
		t.Errorf("no config: %v, warning %q", err, msg)
	}
}
