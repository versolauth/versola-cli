package deploy

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/state"
)

// What containerInspectFormat prints for four containers: a healthy replica
// in slot 2, a stopped Postgres, a never-started proxy (year-1 startedAt) and
// one without a health check.
const fakeInspect = `{"name":"/versola-auth-2","state":"running","health":"healthy","startedAt":"2026-10-14T12:11:51.1Z","image":"ghcr.io/versolauth/versola-auth:0.7.2","service":"auth-2"}
{"name":"/versola-postgres","state":"exited","health":"","startedAt":"2026-10-14T12:00:00Z","image":"postgres:18-alpine","service":"postgres"}
{"name":"/versola-nginx","state":"created","health":"","startedAt":"0001-01-01T00:00:00Z","image":"nginx:1.27","service":"nginx"}
{"name":"/versola-central","state":"running","health":"","startedAt":"2026-10-14T12:11:00Z","image":"ghcr.io/versolauth/versola-central:0.7.2","service":"central"}
`

func keysOf(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestParseContainerInspect(t *testing.T) {
	list, err := parseContainerInspect([]byte(fakeInspect))
	if err != nil {
		t.Fatal(err)
	}
	want := []ContainerStatus{
		{Name: "versola-auth-2", Service: "auth", Slot: 2, State: "running", Health: "healthy", Image: "ghcr.io/versolauth/versola-auth:0.7.2", StartedAt: "2026-10-14T12:11:51.1Z", composeService: "auth-2"},
		{Name: "versola-central", Service: "central", Slot: 1, State: "running", Image: "ghcr.io/versolauth/versola-central:0.7.2", StartedAt: "2026-10-14T12:11:00Z", composeService: "central"},
		{Name: "versola-nginx", Service: "nginx", State: "created", Image: "nginx:1.27", composeService: "nginx"},
		{Name: "versola-postgres", Service: "postgres", State: "exited", Image: "postgres:18-alpine", StartedAt: "2026-10-14T12:00:00Z", composeService: "postgres"},
	}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("got %+v\nwant %+v", list, want)
	}
	if _, err := parseContainerInspect([]byte("not json\n")); err == nil {
		t.Error("garbage parsed")
	}
	if list, err := parseContainerInspect(nil); err != nil || list == nil || len(list) != 0 {
		t.Errorf("empty: %v %v", list, err)
	}
}

func TestSplitSlotService(t *testing.T) {
	for in, want := range map[string]struct {
		base string
		slot int
	}{
		"auth": {"auth", 1}, "auth-3": {"auth", 3}, "edge-2": {"edge", 2}, "central": {"central", 1},
		"postgres": {"postgres", 0}, "auth-x": {"auth-x", 0}, "auth-1": {"auth-1", 0}, "openbao": {"openbao", 0},
	} {
		if b, s := splitSlotService(in); b != want.base || s != want.slot {
			t.Errorf("%s: %s %d, want %+v", in, b, s, want)
		}
	}
}

// The inspect template is the only thing that reaches Docker for status: it
// must name its fields, never the environment (Config.Env holds secrets).
func TestContainerInspectFormatIsStrict(t *testing.T) {
	for _, bad := range []string{"Env", "{{json .}}", ".Config}}", "Mounts", "Args"} {
		if strings.Contains(containerInspectFormat, bad) {
			t.Errorf("containerInspectFormat contains %q", bad)
		}
	}
}

func statusFixture() *state.State {
	migrated := time.Date(2026, 10, 14, 12, 11, 51, 0, time.UTC)
	st := &state.State{Target: "vps", Version: "0.7.2", AuthURL: "https://id.example.com", CentralURL: "https://central.example.com",
		ProxyMode: "nginx", PostgresHost: "127.0.0.1:5432", PostgresMode: PostgresModeBundled, MigratedAt: &migrated,
		PreviousVersion: "0.7.1", Surge: map[string]int{AuthService: 2}}
	st.SetSlots(AuthService, []state.Slot{{N: 1, Version: "0.7.1"}, {N: 2, Version: "0.7.2"}})
	return st
}

// The keys of status --json are a contract: this test fails when one is
// renamed or dropped (adding one means adding it here and to the README).
func TestStatusReportKeys(t *testing.T) {
	r := NewStatusReport(statusFixture(), "0.3.2")
	list, _ := parseContainerInspect([]byte(fakeInspect))
	r.Containers = list
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r.Certificates = CertStatuses([]CertInfo{
		{Name: "server.crt", NotBefore: now.Add(-80 * 24 * time.Hour), NotAfter: now.Add(10 * 24 * time.Hour)},
		{Name: "id.example.com:443", Error: "connection refused"},
	}, now)

	want := []string{"authUrl", "centralUrl", "certificates", "cliVersion", "containers", "database", "deployed", "migratedAt",
		"proxyMode", "replicas", "rollout", "schema", "target", "version", "warnings"}
	if got := keysOf(t, r); !reflect.DeepEqual(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	if got := keysOf(t, r.Containers[0]); !reflect.DeepEqual(got, []string{"health", "image", "name", "service", "slot", "startedAt", "state"}) {
		t.Errorf("container keys %v", got)
	}
	if got := keysOf(t, r.Rollout); !reflect.DeepEqual(got, []string{"from", "rollingBack", "underway"}) {
		t.Errorf("rollout keys %v", got)
	}
	if got := keysOf(t, r.Database); !reflect.DeepEqual(got, []string{"host", "mode"}) {
		t.Errorf("database keys %v", got)
	}
	if got := keysOf(t, r.Certificates[0]); !reflect.DeepEqual(got, []string{"expired", "name", "notAfter", "renewDue"}) {
		t.Errorf("certificate keys %v", got)
	}
	if !r.Certificates[0].RenewDue || r.Certificates[0].Expired || r.Certificates[1].Error == "" {
		t.Errorf("certificates %+v", r.Certificates)
	}

	b, _ := json.Marshal(r)
	for _, frag := range []string{
		`"deployed":true`, `"migratedAt":"2026-10-14T12:11:51Z"`,
		`"rollout":{"underway":true,"from":"0.7.1","rollingBack":false}`,
		`"auth":[{"slot":1,"version":"0.7.1"},{"slot":2,"version":"0.7.2","standIn":true}]`,
		`"edge":[{"slot":1,"version":"0.7.2"}]`, `"central":[{"slot":1,"version":"0.7.2"}]`,
		`"database":{"mode":"bundled","host":"127.0.0.1:5432"}`,
	} {
		if !strings.Contains(string(b), frag) {
			t.Errorf("missing %s in %s", frag, b)
		}
	}
}

func TestStatusReportDatabase(t *testing.T) {
	for name, c := range map[string]struct {
		st   *state.State
		want string
	}{
		"local":        {&state.State{Target: "local", Version: "0.7.2"}, `{"mode":"bundled","host":null}`},
		"vps external": {&state.State{Target: "vps", Version: "0.7.2", PostgresHost: "db.internal:5432"}, `{"mode":"external","host":"db.internal:5432"}`},
	} {
		b, _ := json.Marshal(NewStatusReport(c.st, "dev").Database)
		if string(b) != c.want {
			t.Errorf("%s: %s, want %s", name, b, c.want)
		}
	}
}

func TestNotDeployedReport(t *testing.T) {
	b, _ := json.Marshal(NotDeployedReport("0.3.2"))
	if string(b) != `{"schema":1,"cliVersion":"0.3.2","deployed":false,"containers":[],"warnings":[]}` {
		t.Errorf("got %s", b)
	}
}

// The unlimited certificate setting uses a 100-year lifetime. JSON must not
// tell monitoring to renew a fresh certificate because Duration * 3 overflowed.
func TestCertStatusesLongLived(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	lifetime := 36500 * 24 * time.Hour
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		due     bool
	}{
		{"fresh", 0, false},
		{"before renewal", lifetime*2/3 - time.Nanosecond, false},
		{"renewal due", lifetime - lifetime/3, true},
		{"expired", lifetime + time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			infos := []CertInfo{{Name: "server.crt", NotBefore: now, NotAfter: now.Add(lifetime)}}
			out := CertStatuses(infos, now.Add(tc.elapsed))
			if len(out) != 1 || out[0].RenewDue != tc.due || out[0].Expired != (tc.elapsed >= lifetime) {
				t.Fatalf("certificate status %+v", out)
			}
			// Exercise the output encoding used by status --json too.
			data, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var decoded []CertStatus
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded[0].RenewDue != tc.due {
				t.Fatalf("JSON renewDue: %s", data)
			}
		})
	}
}
