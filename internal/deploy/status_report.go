package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// StatusReport is what `versola status --json` prints: one object, for a
// script, a health check or a support request. The field names are a
// contract (README, "status"): fields may be added, none is renamed or
// changes type. Nothing in it is a secret -- it is built from state.json
// (which holds none), a strict inspect template (containerInspectFormat,
// never the container's environment) and the certificates' dates.
// ReportSchema is the version of the JSON that `status --json` and `stats
// --json` print. It stays 1 while fields are only added; a change a script
// could trip on (a rename, a type change, a field removed) raises it.
const ReportSchema = 1

type StatusReport struct {
	Schema     int    `json:"schema"`
	CLIVersion string `json:"cliVersion"`
	// Deployed is false when this machine has no deployment (no compose
	// file): every other field but warnings is then left out, and the
	// command still exits 0, like the text form.
	Deployed      bool                       `json:"deployed"`
	Target        string                     `json:"target,omitempty"`
	Version       string                     `json:"version,omitempty"`
	AuthURL       string                     `json:"authUrl,omitempty"`
	CentralURL    string                     `json:"centralUrl,omitempty"`
	ProxyMode     string                     `json:"proxyMode,omitempty"`
	ImageRegistry string                     `json:"imageRegistry,omitempty"`
	MigratedAt    *time.Time                 `json:"migratedAt,omitempty"`
	Rollout       *RolloutStatus             `json:"rollout,omitempty"`
	Replicas      map[string][]ReplicaStatus `json:"replicas,omitempty"`
	Database      *DatabaseStatus            `json:"database,omitempty"`
	// Containers is null (not []) when Docker could not be asked: a script
	// tells "nothing runs" from "unknown" without parsing the warning.
	Containers   []ContainerStatus `json:"containers"`
	Certificates []CertStatus      `json:"certificates,omitempty"`
	Warnings     []string          `json:"warnings"`
}

// RolloutStatus is an unfinished rolling upgrade: Version is where it goes,
// From where it comes from (st.PreviousVersion), RollingBack that it is an
// abort going back.
type RolloutStatus struct {
	Underway    bool   `json:"underway"`
	From        string `json:"from,omitempty"`
	RollingBack bool   `json:"rollingBack"`
}

// ReplicaStatus is one recorded slot of a scalable service. The version is
// per slot because a rolling upgrade has two side by side; StandIn marks the
// rollout's own extra replica (st.Surge).
type ReplicaStatus struct {
	Slot    int    `json:"slot"`
	Version string `json:"version"`
	StandIn bool   `json:"standIn,omitempty"`
}

// DatabaseStatus says whose Postgres the deployment uses. Host is the
// host:port as configured (never a URL, never a user or password); null
// when the record does not have one (local: the compose file's own
// service).
type DatabaseStatus struct {
	Mode string  `json:"mode"`
	Host *string `json:"host"`
}

// ContainerStatus is one container of the deployment's compose project, as
// containerInspectFormat reads it.
type ContainerStatus struct {
	Name string `json:"name"`
	// Service is the scalable service a replica belongs to ("auth" for
	// auth-2) with its Slot, or the compose service itself for the others
	// (postgres, openbao, the proxy), which have no slot.
	Service   string `json:"service"`
	Slot      int    `json:"slot,omitempty"`
	State     string `json:"state"`
	Health    string `json:"health,omitempty"`
	Image     string `json:"image"`
	StartedAt string `json:"startedAt,omitempty"`

	// composeService is the compose service name ("auth-2"): what the
	// stand-in memory probe asks about (see statsProbe).
	composeService string
}

// CertStatus is one certificate of the deployment: NotAfter and RenewDue,
// or Error when it could not be read.
type CertStatus struct {
	Name     string     `json:"name"`
	NotAfter *time.Time `json:"notAfter,omitempty"`
	RenewDue bool       `json:"renewDue"`
	Expired  bool       `json:"expired"`
	Error    string     `json:"error,omitempty"`
}

// NewStatusReport fills what the record says. Containers, certificates and
// warnings are the caller's: they need Docker.
func NewStatusReport(st *state.State, cliVersion string) StatusReport {
	r := StatusReport{
		Schema:        ReportSchema,
		CLIVersion:    cliVersion,
		Deployed:      true,
		Target:        st.Target,
		Version:       st.Version,
		AuthURL:       st.AuthURL,
		CentralURL:    st.CentralURL,
		ProxyMode:     st.ProxyMode,
		ImageRegistry: st.ImageRegistry,
		Rollout:       &RolloutStatus{Underway: st.RolloutUnderway(), From: st.PreviousVersion, RollingBack: st.RollingBack},
		Replicas:      map[string][]ReplicaStatus{},
		Warnings:      []string{},
	}
	if st.MigratedAt != nil {
		t := st.MigratedAt.UTC()
		r.MigratedAt = &t
	}
	for _, svc := range rolloutServices {
		var slots []ReplicaStatus
		for _, sl := range st.ActiveSlots(svc) {
			n, standIn := st.Surge[svc]
			slots = append(slots, ReplicaStatus{Slot: sl.N, Version: sl.Version, StandIn: standIn && n == sl.N})
		}
		r.Replicas[svc] = slots
	}
	// local always runs its own Postgres (a service of the compose file);
	// on vps an empty mode is an external one (State.PostgresMode).
	db := &DatabaseStatus{Mode: "external"}
	if st.Target == "local" || st.PostgresMode == PostgresModeBundled {
		db.Mode = "bundled"
	}
	if st.PostgresHost != "" {
		h := st.PostgresHost
		db.Host = &h
	}
	r.Database = db
	return r
}

// NotDeployedReport is status --json on a machine with nothing deployed.
func NotDeployedReport(cliVersion string) StatusReport {
	return StatusReport{Schema: ReportSchema, CLIVersion: cliVersion, Containers: []ContainerStatus{}, Warnings: []string{}}
}

// CertStatuses turns the certificate report into its JSON form, soonest
// to expire first (the order of the text table).
func CertStatuses(infos []CertInfo, now time.Time) []CertStatus {
	sorted := append([]CertInfo(nil), infos...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if (sorted[i].Error != "") != (sorted[j].Error != "") {
			return sorted[i].Error == ""
		}
		return sorted[i].NotAfter.Before(sorted[j].NotAfter)
	})
	out := []CertStatus{}
	for _, c := range sorted {
		if c.Error != "" || c.NotAfter.IsZero() {
			out = append(out, CertStatus{Name: c.Name, Error: c.Error})
			continue
		}
		t := c.NotAfter.UTC()
		out = append(out, CertStatus{Name: c.Name, NotAfter: &t, RenewDue: c.Due(now), Expired: c.Left(now) <= 0})
	}
	return out
}

// containerInspectFormat is the only thing `docker inspect` is asked for:
// one JSON object per container, of these fields alone. Never a bare inspect
// -- Config.Env carries the services' secrets (env_file is inlined), and an
// output that held them once could end up in a support ticket.
const containerInspectFormat = `{"name":{{json .Name}},"state":{{json .State.Status}},` +
	`"health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}},` +
	`"startedAt":{{json .State.StartedAt}},"image":{{json .Config.Image}},` +
	`"service":{{json (index .Config.Labels "com.docker.compose.service")}}}`

// ListContainers lists every container of the deployment's compose project
// (the services, the proxy, OpenBao and a bundled Postgres: they are all in
// it, see state.ComposeArgs), stopped ones included. An empty list is
// nothing created; an error is Docker not answering.
func ListContainers(composePath string) ([]ContainerStatus, error) {
	out, err := docker.Output(state.ComposeArgs(composePath, "ps", "-a", "-q")...)
	if err != nil {
		return nil, fmt.Errorf("docker compose ps failed: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return []ContainerStatus{}, nil
	}
	args := append([]string{"inspect", "--type", "container", "--format", containerInspectFormat}, ids...)
	out, err = docker.Output(args...)
	if err != nil {
		// A container removed between the two calls fails the whole
		// inspect; the next status sees the list as it is.
		return nil, fmt.Errorf("docker inspect failed: %w", err)
	}
	return parseContainerInspect(out)
}

// parseContainerInspect reads containerInspectFormat's lines, sorted by
// name so that the output does not change order between two runs.
func parseContainerInspect(out []byte) ([]ContainerStatus, error) {
	list := []ContainerStatus{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var raw struct {
			Name, State, Health, StartedAt, Image, Service string
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, fmt.Errorf("unexpected docker inspect output: %w", err)
		}
		// A health status is only meaningful while the container runs. Docker
		// keeps the last one of an exited container, and a one-shot one that
		// inherits its image's HEALTHCHECK (mtls-init, from step-ca's) ends
		// up "unhealthy" after a perfectly good run: a script reading this
		// would report an outage that is not one. State says what happened.
		health := raw.Health
		if raw.State != "running" {
			health = ""
		}
		c := ContainerStatus{
			Name:           strings.TrimPrefix(raw.Name, "/"),
			State:          raw.State,
			Health:         health,
			Image:          raw.Image,
			StartedAt:      raw.StartedAt,
			composeService: raw.Service,
		}
		// A container that never started reports year 1.
		if strings.HasPrefix(c.StartedAt, "0001-") {
			c.StartedAt = ""
		}
		c.Service, c.Slot = splitSlotService(raw.Service)
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// splitSlotService undoes topology.SlotService for the scalable services:
// "auth" is slot 1, "auth-3" slot 3. Any other service has no slot.
func splitSlotService(service string) (base string, slot int) {
	for _, svc := range rolloutServices {
		if service == svc {
			return svc, 1
		}
		if rest, ok := strings.CutPrefix(service, svc+"-"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n > 1 {
				return svc, n
			}
		}
	}
	return service, 0
}
