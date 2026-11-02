package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

// docker stats' JSON lines: a busy auth (CPU above 100%: more than one
// core), edge, and a row docker could not measure.
const fakeDockerStats = `{"BlockIO":"0B / 0B","CPUPerc":"250.50%","Container":"abc","ID":"abc","MemPerc":"21.10%","MemUsage":"812.4MiB / 3.8GiB","Name":"versola-auth","NetIO":"1kB / 2kB","PIDs":"45"}
{"CPUPerc":"1.02%","MemUsage":"700MiB / 3.8GiB","Name":"versola-edge","PIDs":"40"}
{"CPUPerc":"--","MemUsage":"-- / --","Name":"versola-postgres","PIDs":"--"}
`

func TestParseDockerStats(t *testing.T) {
	m, err := parseDockerStats([]byte(fakeDockerStats))
	if err != nil {
		t.Fatal(err)
	}
	a := m["versola-auth"]
	if a.CPUPercent == nil || *a.CPUPercent != 250.5 || a.PIDs == nil || *a.PIDs != 45 {
		t.Errorf("auth %+v", a)
	}
	wantUsed, _ := parseMemUsage("812.4MiB")
	wantLimit, _ := parseMemUsage("3.8GiB")
	if a.MemUsedBytes == nil || *a.MemUsedBytes != wantUsed || a.MemLimitBytes == nil || *a.MemLimitBytes != wantLimit || wantUsed < 812*miB {
		t.Errorf("auth memory %v / %v", a.MemUsedBytes, a.MemLimitBytes)
	}
	p := m["versola-postgres"]
	if p.CPUPercent != nil || p.MemUsedBytes != nil || p.MemLimitBytes != nil || p.PIDs != nil {
		t.Errorf("unmeasured row has figures: %+v", p)
	}
	if _, err := parseDockerStats([]byte("{")); err == nil {
		t.Error("garbage parsed")
	}
}

// statsList is what ListContainers would return for a vps with one auth and
// one edge replica, a bundled Postgres and a stopped OpenBao.
func statsList() []ContainerStatus {
	return []ContainerStatus{
		{Name: "versola-auth", Service: "auth", Slot: 1, State: "running", composeService: "auth"},
		{Name: "versola-edge", Service: "edge", Slot: 1, State: "running", composeService: "edge"},
		{Name: "versola-openbao", Service: "openbao", State: "exited", composeService: "openbao"},
		{Name: "versola-postgres", Service: "postgres", State: "running", composeService: "postgres"},
	}
}

func fakeStatsDeps(avail int64, readable bool, why string, pgImage bool, run pgRunner) statsDeps {
	return statsDeps{
		containers:     func(string) ([]ContainerStatus, error) { return statsList(), nil },
		dockerStats:    func([]string) ([]byte, error) { return []byte(fakeDockerStats), nil },
		hostMemory:     func() (int64, bool, string, error) { return avail, readable, why, nil },
		centralFronted: func(*state.State) bool { return false },
		pgImagePresent: func() bool { return pgImage },
		pgRun:          run,
	}
}

func headroom(n int) pgRunner { return func(pgTarget, string) (int, error) { return n, nil } }

func TestStatsRowsOnlyMeasureRunning(t *testing.T) {
	var asked []string
	rows, err := statsRows(statsList(), func(names []string) ([]byte, error) { asked = names; return []byte(fakeDockerStats), nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "versola-auth,versola-edge,versola-postgres" {
		t.Errorf("asked docker stats for %v", asked)
	}
	if len(rows) != 4 || rows[2].Name != "versola-openbao" || rows[2].MemUsedBytes != nil || rows[0].Service != "auth" || rows[0].MemUsedBytes == nil {
		t.Errorf("rows %+v", rows)
	}
}

// The verdict is the rollout's own check: for any free memory, stats says
// "fits" exactly when checkStandInMemory lets the stand-in start.
func TestStatsVerdictMatchesThePreflight(t *testing.T) {
	st := plainState(1, 1)
	rows, _ := statsRows(statsList(), func([]string) ([]byte, error) { return []byte(fakeDockerStats), nil })
	services := []string{AuthService, EdgeService}
	for _, avail := range []int64{900 * miB, 1155 * miB, 1156 * miB, 1068 * miB, 1069 * miB, 3000 * miB} {
		p := statsProbe{rows: rows, mem: func() (int64, bool, string, error) { return avail, true, "", nil }}
		h := hostVerdict(p, st, services)
		refused := checkStandInMemory(p, st, services, noWarn(t)) != nil
		if h.StandIn == nil || h.StandIn.Fits == refused {
			t.Errorf("avail %d MiB: verdict %+v, preflight refused %v", avail/miB, h.StandIn, refused)
		}
	}
	// auth measured at 812.4 MiB is below the floor: the floor (900 MiB)
	// plus the reserve (256 MiB) is the threshold, as the README says.
	p := statsProbe{rows: rows, mem: func() (int64, bool, string, error) { return 2 << 30, true, "", nil }}
	if v := hostVerdict(p, st, services).StandIn; v.ThresholdBytes != 1156*miB || v.Measured {
		t.Errorf("floor: %+v", v)
	}
	// A replica measured above the floor raises it.
	big := append([]ContainerStats{}, rows...)
	used := int64(1000 * miB)
	big[1].MemUsedBytes = &used
	p.rows = big
	if v := hostVerdict(p, st, services).StandIn; v.ThresholdBytes != 1256*miB || !v.Measured || v.Service != EdgeService {
		t.Errorf("measured: %+v", v)
	}
}

func TestStatsVps(t *testing.T) {
	dir := pgBundle(t)
	compose := filepath.Join(dir, "compose.yml")
	st := plainState(1, 1)

	r, err := collectStats(st, compose, fakeStatsDeps(2400*miB, true, "", true, headroom(50)), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Host == nil || !r.Host.StandIn.Fits || r.Postgres == nil || !r.Postgres.Checked || !r.Postgres.Fits || r.Postgres.Service != EdgeService {
		t.Fatalf("report %+v host %+v pg %+v", r, r.Host, r.Postgres)
	}
	var out bytes.Buffer
	WriteStats(&out, r)
	for _, want := range []string{"versola-auth", "250.50%", "812.4MiB / 3.8GiB", "versola-openbao", "exited",
		"Stand-in memory: free 2.34 GiB -- enough for one stand-in (needs 1.13 GiB",
		"Postgres connections: 50 free -- enough for a stand-in edge replica (opens 13, plus 3 kept free)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}

	// Short of memory: the way round is said.
	r, _ = collectStats(st, compose, fakeStatsDeps(900*miB, true, "", false, headroom(50)), &bytes.Buffer{})
	out.Reset()
	WriteStats(&out, r)
	for _, want := range []string{"not enough for a stand-in", "--no-surge", "Postgres connections: " + pgNotPulled} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}

	// Host memory can't be read: the reason, no verdict.
	r, _ = collectStats(st, compose, fakeStatsDeps(0, false, "this machine runs windows, not the vps", false, headroom(50)), &bytes.Buffer{})
	if r.Host.Readable || r.Host.StandIn != nil || r.Host.MemAvailableBytes != nil {
		t.Errorf("unreadable: %+v", r.Host)
	}
	out.Reset()
	WriteStats(&out, r)
	if !strings.Contains(out.String(), "Stand-in memory: not checked -- this machine runs windows, not the vps") || strings.Contains(out.String(), "enough") {
		t.Errorf("unreadable:\n%s", out.String())
	}
}

func TestStatsLocalHasNoVerdict(t *testing.T) {
	st := &state.State{Target: "local", Version: "0.7.2"}
	r, err := collectStats(st, filepath.Join(t.TempDir(), "compose.yml"), fakeStatsDeps(2400*miB, true, "", true, headroom(50)), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Host != nil || r.Postgres != nil || len(r.Containers) != 4 {
		t.Errorf("local: %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), `"host"`) || strings.Contains(string(b), `"postgres":`) {
		t.Errorf("local JSON: %s", b)
	}
}

func TestStatsDockerDown(t *testing.T) {
	d := fakeStatsDeps(0, false, "x", false, headroom(0))
	d.containers = func(string) ([]ContainerStatus, error) {
		return nil, errors.New("cannot connect to the Docker daemon\nmore")
	}
	r, err := collectStats(&state.State{Target: "local"}, "compose.yml", d, &bytes.Buffer{})
	if err == nil || r.Containers != nil || len(r.Warnings) != 1 || strings.Contains(r.Warnings[0], "more") {
		t.Errorf("err %v report %+v", err, r)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"containers":null`) {
		t.Errorf("JSON %s", b)
	}
}

// Nothing of a service's secrets file reaches stats' output, text or JSON,
// nor its progress: the check hands docker the file's path, never its
// contents.
func TestStatsNeverShowsSecrets(t *testing.T) {
	const secret = "fixture-secret-Zq81xP"
	dir := pgBundle(t)
	for _, svc := range []string{"auth", "edge"} {
		if err := os.WriteFile(filepath.Join(dir, svc+".secrets.env"), []byte("POSTGRES_PASSWORD="+secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var gotFile string
	run := func(_ pgTarget, secretsFile string) (int, error) {
		gotFile = secretsFile
		return 0, errors.New("password authentication failed for user \"auth\"")
	}
	var progress, out bytes.Buffer
	r, _ := collectStats(plainState(1, 1), filepath.Join(dir, "compose.yml"), fakeStatsDeps(2400*miB, true, "", true, run), &progress)
	WriteStats(&out, r)
	b, _ := json.Marshal(r)
	for name, s := range map[string]string{"text": out.String(), "json": string(b), "progress": progress.String()} {
		if strings.Contains(s, secret) {
			t.Errorf("%s output carries the secret:\n%s", name, s)
		}
	}
	if !strings.HasSuffix(gotFile, ".secrets.env") || r.Postgres.Checked || !strings.Contains(r.Postgres.Reason, "password authentication failed") {
		t.Errorf("file %q, pg %+v", gotFile, r.Postgres)
	}
}

func TestPgStatsArgsNeverPull(t *testing.T) {
	args := pgHeadroomArgsPull(pgTarget{host: "127.0.0.1", port: "5432", db: "versola", user: "auth"}, "/b/auth.secrets.env", "never")
	if i := indexOf(args, "--pull"); i < 0 || args[i+1] != "never" {
		t.Errorf("args %v", args)
	}
	if i := indexOf(pgHeadroomArgs(pgTarget{host: "h", port: "1", db: "d", user: "u"}, "/f"), "--pull"); i < 0 {
		t.Error("rollout args lost --pull")
	}
}

// Losing Docker between the list and snapshot still fails the command, even
// though the report can retain the containers it already discovered.
func TestStatsSnapshotFailure(t *testing.T) {
	d := fakeStatsDeps(0, false, "", false, headroom(0))
	d.dockerStats = func([]string) ([]byte, error) {
		return nil, errors.New("Docker disconnected during snapshot")
	}
	r, err := collectStats(&state.State{Target: "local"}, "compose.yml", d, &bytes.Buffer{})
	if err == nil || len(r.Containers) != 4 || len(r.Warnings) != 1 {
		t.Fatalf("err %v, report %+v", err, r)
	}
	if r.Containers[0].MemUsedBytes != nil {
		t.Fatal("failed snapshot must leave measurements unknown")
	}
}
