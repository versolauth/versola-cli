package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// StatsReport is what `versola stats` shows: what each container of the
// deployment uses right now (one `docker stats --no-stream` snapshot) and,
// on vps, whether the host could take a rolling upgrade's stand-in replica
// -- in the rollout's own numbers (standInThreshold, pgHeadroomFor), so the
// answer here is the answer the rollout's pre-flight would give.
//
// Like StatusReport its JSON field names are a contract: additive only.
type StatsReport struct {
	Schema int    `json:"schema"` // ReportSchema
	Target string `json:"target"`
	// Containers is null when Docker could not be asked.
	Containers []ContainerStats `json:"containers"`
	// Host and Postgres are vps only: on local, Docker Desktop's VM memory
	// is not this machine's, and no rollout runs there.
	Host     *HostMemoryStatus `json:"host,omitempty"`
	Postgres *PgHeadroomStatus `json:"postgres,omitempty"`
	Warnings []string          `json:"warnings"`
}

// ContainerStats is one container's row. The figures are null for a
// container that is not running, or that docker stats could not measure
// ("--": it stopped in between).
type ContainerStats struct {
	Name          string   `json:"name"`
	Service       string   `json:"service"`
	Slot          int      `json:"slot,omitempty"`
	State         string   `json:"state"`
	CPUPercent    *float64 `json:"cpuPercent"`
	MemUsedBytes  *int64   `json:"memUsedBytes"`
	MemLimitBytes *int64   `json:"memLimitBytes"`
	PIDs          *int     `json:"pids"`

	composeService string
	// What docker stats printed, for the text table: its own rounding
	// ("812.4MiB / 3.8GiB") reads better than ours.
	cpuText, memText, pidsText string
}

// HostMemoryStatus is the Docker host's free memory and what a stand-in
// needs of it. Readable is false, with Reason, when this process can't see
// that host's memory (not Linux, Docker pointed elsewhere) -- the case where
// the rollout itself refuses to start a stand-in.
type HostMemoryStatus struct {
	Readable          bool            `json:"readable"`
	Reason            string          `json:"reason,omitempty"`
	MemAvailableBytes *int64          `json:"memAvailableBytes,omitempty"`
	StandIn           *StandInVerdict `json:"standIn,omitempty"`
}

// StandInVerdict is the stand-in memory check of the rollout's pre-flight,
// made now. Services are those a rollout would start a stand-in for (see
// standInServices); Service is the one that needs the most, and
// ThresholdBytes (NeedBytes plus the reserve) what MemAvailable must reach.
// Services is empty when an unfinished rollout has no stand-in left to
// start: Fits is then true.
type StandInVerdict struct {
	Services       []string `json:"services"`
	Service        string   `json:"service,omitempty"`
	NeedBytes      int64    `json:"needBytes"`
	ThresholdBytes int64    `json:"thresholdBytes"`
	Measured       bool     `json:"measured"`
	Fits           bool     `json:"fits"`
}

// PgHeadroomStatus is the rollout's Postgres connection check, made now.
// Checked is false with Reason when it could not be (the psql image not on
// this machine -- stats does not pull it --, a config it can't read, a
// server it can't log in to).
type PgHeadroomStatus struct {
	Checked bool   `json:"checked"`
	Reason  string `json:"reason,omitempty"`
	// FreeConnections Postgres takes now; NeedConnections a stand-in of
	// Service opens, plus MarginConnections kept free. Pointers, so that they
	// are present, 0 included, exactly when Checked is true: with 0 free
	// connections -- the case a script most needs to see -- a plain int with
	// omitempty would drop the field.
	FreeConnections   *int   `json:"freeConnections,omitempty"`
	NeedConnections   *int   `json:"needConnections,omitempty"`
	MarginConnections *int   `json:"marginConnections,omitempty"`
	Service           string `json:"service,omitempty"`
	Fits              bool   `json:"fits"`
}

// pgNotPulled is the Postgres line when the psql image is absent: stats
// does not download ~100 MB to answer a question the rollout asks anyway.
const pgNotPulled = "not checked (runs during rollout)"

// statsDeps is what CollectStats asks Docker and the host, a seam for tests.
type statsDeps struct {
	containers     func(composePath string) ([]ContainerStatus, error)
	dockerStats    func(names []string) ([]byte, error)
	hostMemory     func() (int64, bool, string, error)
	centralFronted func(st *state.State) bool
	pgImagePresent func() bool
	pgRun          pgRunner
}

var realStatsDeps = statsDeps{
	containers: ListContainers,
	dockerStats: func(names []string) ([]byte, error) {
		args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)
		out, _, err := docker.OutputWithin(30*time.Second, args...)
		return out, err
	},
	hostMemory:     checks.HostMemoryAvailable,
	centralFronted: func(st *state.State) bool { return centralStandsIn(st, RolloutOptions{}) },
	pgImagePresent: func() bool { return docker.RunQuiet("image", "inspect", "--format", "{{.Id}}", pgCheckImage) == nil },
	// --pull never: the image was checked for just above; if it went in
	// between, the run fails instead of downloading.
	pgRun: func(t pgTarget, secretsFile string) (int, error) {
		return runPgHeadroomArgs(pgHeadroomArgsPull(t, secretsFile, "never"), time.Minute)
	},
}

// CollectStats gathers the report. progress gets what is not the report (the
// Postgres check's "Checking..." line). The error is Docker not answering:
// the report is still filled as far as it goes (Containers null, a warning),
// for the caller to print before it fails.
func CollectStats(st *state.State, progress io.Writer) (StatsReport, error) {
	composePath, _, err := st.ComposeFilePath()
	if err != nil {
		return StatsReport{}, err
	}
	return collectStats(st, composePath, realStatsDeps, progress)
}

func collectStats(st *state.State, composePath string, d statsDeps, progress io.Writer) (StatsReport, error) {
	r := StatsReport{Schema: ReportSchema, Target: st.Target, Warnings: []string{}}
	list, dockerErr := d.containers(composePath)
	if dockerErr != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("Docker could not list the containers: %v", errFirstLine(dockerErr)))
	} else {
		rows, err := statsRows(list, d.dockerStats)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("docker stats failed (%v): no figures", errFirstLine(err)))
			dockerErr = err
		}
		r.Containers = rows
	}
	if st.Target != "vps" {
		return r, dockerErr
	}

	services := standInServices(st, RolloutOptions{}, false, false, d.centralFronted(st))
	r.Host = hostVerdict(statsProbe{rows: r.Containers, mem: d.hostMemory}, st, services)

	// The connection check needs the containers' configs in the bundle and
	// psql on the host's network: what the rollout runs, with the image
	// left where it is.
	switch {
	case len(services) == 0:
		r.Postgres = &PgHeadroomStatus{Reason: "no stand-in left to start"}
	case !d.pgImagePresent():
		r.Postgres = &PgHeadroomStatus{Reason: pgNotPulled}
	default:
		var why string
		warn := func(f string, a ...any) {
			why = strings.TrimSpace(strings.TrimPrefix(fmt.Sprintf(f, a...), "Warning: "))
		}
		headroom, need, needFor, ok := pgHeadroomFor(progress, filepath.Dir(composePath), services, 1, standInAsker, d.pgRun, warn)
		if !ok {
			r.Postgres = &PgHeadroomStatus{Reason: why}
		} else {
			margin := pgMargin
			r.Postgres = &PgHeadroomStatus{Checked: true, FreeConnections: &headroom, NeedConnections: &need,
				MarginConnections: &margin, Service: needFor, Fits: headroom >= need+pgMargin}
		}
	}
	return r, dockerErr
}

// statsRows measures the running containers in one docker stats call and
// lists the others without figures.
func statsRows(list []ContainerStatus, run func([]string) ([]byte, error)) ([]ContainerStats, error) {
	rows := make([]ContainerStats, 0, len(list))
	var running []string
	for _, c := range list {
		rows = append(rows, ContainerStats{Name: c.Name, Service: c.Service, Slot: c.Slot, State: c.State, composeService: c.composeService})
		if c.State == "running" {
			running = append(running, c.Name)
		}
	}
	if len(running) == 0 {
		return rows, nil
	}
	out, err := run(running)
	if err != nil {
		return rows, err
	}
	figures, err := parseDockerStats(out)
	if err != nil {
		return rows, err
	}
	for i := range rows {
		if f, ok := figures[rows[i].Name]; ok {
			f.Name, f.Service, f.Slot, f.State, f.composeService = rows[i].Name, rows[i].Service, rows[i].Slot, rows[i].State, rows[i].composeService
			rows[i] = f
		}
	}
	return rows, nil
}

// parseDockerStats reads `docker stats --format '{{json .}}'` lines, by
// container name. A figure docker could not measure ("--", or a container
// that stopped in between: "0B / 0B") is left null; CPU may exceed 100% (one
// core is 100%).
func parseDockerStats(out []byte) (map[string]ContainerStats, error) {
	m := map[string]ContainerStats{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var raw struct{ Name, CPUPerc, MemUsage, PIDs string }
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, fmt.Errorf("unexpected docker stats output: %w", err)
		}
		c := ContainerStats{cpuText: raw.CPUPerc, memText: raw.MemUsage, pidsText: raw.PIDs}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(raw.CPUPerc), "%"), 64); err == nil {
			c.CPUPercent = &v
		}
		if used, limit, ok := strings.Cut(raw.MemUsage, "/"); ok {
			u, errU := parseMemUsage(used)
			l, errL := parseMemUsage(limit)
			if errU == nil && errL == nil && l > 0 {
				c.MemUsedBytes, c.MemLimitBytes = &u, &l
			}
		}
		if n, err := strconv.Atoi(strings.TrimSpace(raw.PIDs)); err == nil {
			c.PIDs = &n
		}
		m[strings.TrimPrefix(raw.Name, "/")] = c
	}
	return m, nil
}

// statsProbe is the stand-in check's memoryProbe over the snapshot already
// taken: the same figures the table shows (hostMemory would ask docker stats
// again, per service, for the same numbers a moment later).
type statsProbe struct {
	rows []ContainerStats
	mem  func() (int64, bool, string, error)
}

func (p statsProbe) available() (int64, bool, string, error) { return p.mem() }

func (p statsProbe) usage(service string) (int64, bool, error) {
	for _, r := range p.rows {
		if r.composeService == service && r.State == "running" && r.MemUsedBytes != nil {
			return *r.MemUsedBytes, true, nil
		}
	}
	return 0, false, nil
}

// hostVerdict is the stand-in memory check of checkStandInMemory, kept as
// numbers: the largest standInThreshold of services against MemAvailable
// (the Postgres container's and every other container's memory already
// counted as used: MemAvailable is what is left).
func hostVerdict(p memoryProbe, st *state.State, services []string) *HostMemoryStatus {
	avail, readable, why, err := p.available()
	if !readable {
		return &HostMemoryStatus{Reason: why}
	}
	if err != nil {
		return &HostMemoryStatus{Reason: fmt.Sprintf("couldn't read the free memory: %v", err)}
	}
	h := &HostMemoryStatus{Readable: true, MemAvailableBytes: &avail}
	v := &StandInVerdict{Services: append([]string{}, services...)}
	for _, svc := range services {
		need, threshold, measured, err := standInThreshold(p, st, svc)
		if err != nil {
			h.Reason = err.Error()
			return h
		}
		if threshold > v.ThresholdBytes {
			v.Service, v.NeedBytes, v.ThresholdBytes, v.Measured = svc, need, threshold, measured
		}
	}
	v.Fits = avail >= v.ThresholdBytes
	h.StandIn = v
	return h
}

// WriteStats prints the report for a person.
func WriteStats(w io.Writer, r StatsReport) {
	if r.Containers != nil {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSERVICE\tSTATE\tCPU %\tMEM USAGE / LIMIT\tPIDS")
		for _, c := range r.Containers {
			svc := c.Service
			if c.Slot > 1 {
				svc = fmt.Sprintf("%s (slot %d)", c.Service, c.Slot)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, svc, c.State, dash(c.cpuText), dash(c.memText), dash(c.pidsText))
		}
		tw.Flush()
		if len(r.Containers) == 0 {
			fmt.Fprintln(w, "(no containers: run `versola up`)")
		}
	}
	if r.Host != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, hostLine(r.Host))
	}
	if r.Postgres != nil {
		fmt.Fprintln(w, pgLine(r.Postgres))
	}
	for _, msg := range r.Warnings {
		fmt.Fprintf(w, "WARNING: %s\n", msg)
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "--"
	}
	return s
}

// noSurgeHint is what --no-surge does instead of a stand-in (README,
// "rollout"): said in full, since it is the way round both refusals.
const noSurgeHint = "--no-surge (each replica is replaced without a stand-in: one fewer serves meanwhile, a service with one replica also needs --allow-downtime, and central restarts in place)"

func hostLine(h *HostMemoryStatus) string {
	if h.MemAvailableBytes == nil {
		return "Stand-in memory: not checked -- " + h.Reason + " (run `versola stats` on the vps itself)"
	}
	free := gib(*h.MemAvailableBytes)
	v := h.StandIn
	switch {
	case v == nil:
		return fmt.Sprintf("Stand-in memory: free %s -- not checked: %s", free, h.Reason)
	case len(v.Services) == 0:
		return fmt.Sprintf("Stand-in memory: free %s -- the unfinished rollout has no stand-in left to start", free)
	}
	basis := "what a running " + v.Service + " replica uses now"
	if !v.Measured {
		basis = "the size of one replica"
	}
	needs := fmt.Sprintf("needs %s: %s for a stand-in %s (%s) plus %s kept free", gib(v.ThresholdBytes), mib(v.NeedBytes), v.Service, basis, mib(memoryReserve))
	if v.Fits {
		return fmt.Sprintf("Stand-in memory: free %s -- enough for one stand-in (%s)", free, needs)
	}
	return fmt.Sprintf("Stand-in memory: free %s -- not enough for a stand-in (%s): a rollout would refuse; free memory first, or run it with %s", free, needs, noSurgeHint)
}

func pgLine(p *PgHeadroomStatus) string {
	switch {
	case !p.Checked:
		return "Postgres connections: " + p.Reason
	case p.Fits:
		return fmt.Sprintf("Postgres connections: %d free -- enough for a stand-in %s replica (opens %d, plus %d kept free)", *p.FreeConnections, p.Service, *p.NeedConnections, *p.MarginConnections)
	}
	return fmt.Sprintf("Postgres connections: %d free -- not enough for a stand-in %s replica (opens %d, plus %d kept free): a rollout would refuse; raise max_connections, or run it with --no-surge", *p.FreeConnections, p.Service, *p.NeedConnections, *p.MarginConnections)
}

func gib(b int64) string { return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30)) }
