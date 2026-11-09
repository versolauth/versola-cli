package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

const fixtureSecret = "fixture-secret-Zq81xP"

// recordDeployment saves st under a temporary home with a compose file and a
// secrets file holding fixtureSecret next to it, and returns the compose path.
func recordDeployment(t *testing.T, st *state.State) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".versola", "active", "bundle-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"compose.yml": "services: {}\n", "auth.secrets.env": "POSTGRES_PASSWORD=" + fixtureSecret + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "compose.yml")
}

// stubStatus replaces what status --json asks Docker; containers nil with
// dockerErr set is Docker down.
func stubStatus(t *testing.T, containers []deploy.ContainerStatus, dockerErr error) {
	t.Helper()
	oc, ocr, op, ow, on := statusContainers, statusCertReport, statusProbeCerts, statusCertsWarning, statusNow
	oservices := statusServices
	oOpenbao := statusOpenbao
	t.Cleanup(func() {
		statusContainers, statusCertReport, statusProbeCerts, statusCertsWarning, statusNow = oc, ocr, op, ow, on
		statusServices = oservices
		statusOpenbao = oOpenbao
	})
	statusContainers = func(string) ([]deploy.ContainerStatus, error) { return containers, dockerErr }
	statusServices = func(string) ([]string, error) { return []string{"auth"}, nil }
	statusOpenbao = func(string) (*deploy.ContainerStatus, error) { t.Fatal("unexpected OpenBao lookup"); return nil, nil }
	statusCertReport = func(*state.State) ([]deploy.CertInfo, error) {
		now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		return []deploy.CertInfo{{Name: "server.crt", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)}}, nil
	}
	statusProbeCerts = func(*state.State) []deploy.CertInfo { return nil }
	statusCertsWarning = func(*state.State) string { return "nothing renews the mutual-TLS certificates" }
	statusNow = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
}

func TestStatusServiceWarningsBothFormats(t *testing.T) {
	compose := recordDeployment(t, vpsFixture())
	stubStatus(t, []deploy.ContainerStatus{
		{Name: "central", Service: "central", Slot: 1, State: "restarting"},
		{Name: "auth", Service: "auth", Slot: 1, State: "running", Health: "unhealthy"},
		{Name: "init", Service: "mtls-init", State: "exited", Health: "unhealthy"},
	}, nil)
	statusServices = func(string) ([]string, error) { return []string{"central", "auth", "edge", "mtls-init"}, nil }
	oldTable := statusTable
	t.Cleanup(func() { statusTable = oldTable })
	statusTable = func(w io.Writer, _ string) error { _, err := fmt.Fprintln(w, "container table"); return err }
	var jsonOut, textOut bytes.Buffer
	if err := writeStatusJSON(&jsonOut, compose, true); err != nil {
		t.Fatalf("a service outage changed the JSON exit code: %v", err)
	}
	if err := writeStatusContainersText(&textOut, compose); err != nil {
		t.Fatalf("a service outage changed the text exit code: %v", err)
	}
	m := decodeOnly(t, jsonOut.Bytes())
	warnings := m["warnings"].([]any)
	if len(warnings) != 4 { // three services and the existing certificate warning
		t.Fatalf("warnings: %v", warnings)
	}
	for _, warning := range warnings[:3] {
		if !strings.Contains(textOut.String(), "WARNING: "+warning.(string)+"\n") {
			t.Errorf("text and JSON disagree: %q, text: %s", warning, textOut.String())
		}
	}
	if strings.Contains(textOut.String(), "mtls-init") || strings.Contains(jsonOut.String()+textOut.String(), fixtureSecret) {
		t.Fatal("init warning or secret in status output")
	}
}

func TestStatusInventoryFailureStillPrintsJSON(t *testing.T) {
	compose := recordDeployment(t, vpsFixture())
	stubStatus(t, []deploy.ContainerStatus{}, nil)
	statusServices = func(string) ([]string, error) { return nil, errors.New("couldn't read the compose configuration") }
	var out bytes.Buffer
	if err := writeStatusJSON(&out, compose, true); err == nil {
		t.Fatal("unreadable inventory reported as success")
	}
	m := decodeOnly(t, out.Bytes())
	if !strings.Contains(fmt.Sprint(m["warnings"]), "services could not be read") {
		t.Fatalf("warnings %v", m["warnings"])
	}
}

func TestStatusRetainedOpenbao(t *testing.T) {
	compose := recordDeployment(t, vpsFixture())
	stubStatus(t, []deploy.ContainerStatus{}, nil)
	statusServices = func(string) ([]string, error) { return []string{"openbao"}, nil }
	for _, containerState := range []string{"running", "exited", "restarting", "missing", "unknown"} {
		t.Run(containerState, func(t *testing.T) {
			statusOpenbao = func(target string) (*deploy.ContainerStatus, error) {
				if target != "vps" {
					t.Fatalf("target %s", target)
				}
				if containerState == "missing" {
					return nil, nil
				}
				if containerState == "unknown" {
					return nil, errors.New("Docker unavailable")
				}
				return &deploy.ContainerStatus{Name: "retained", Service: "openbao", State: containerState}, nil
			}
			var out bytes.Buffer
			err := writeStatusJSON(&out, compose, true)
			if (err != nil) != (containerState == "unknown") {
				t.Fatalf("error: %v", err)
			}
			m := decodeOnly(t, out.Bytes())
			warnings := fmt.Sprint(m["warnings"])
			switch containerState {
			case "running":
				if strings.Contains(warnings, "openbao") {
					t.Fatalf("healthy retained OpenBao: %s", warnings)
				}
			case "missing":
				if !strings.Contains(warnings, "service openbao has no container") {
					t.Fatal(warnings)
				}
			case "unknown":
				if !strings.Contains(warnings, "OpenBao's container could not be read") || strings.Contains(warnings, "has no container") {
					t.Fatal(warnings)
				}
			default:
				if !strings.Contains(warnings, "service openbao (retained)") || !strings.Contains(warnings, "versola logs openbao --tail 100") {
					t.Fatal(warnings)
				}
			}
		})
	}
}

// decodeOnly fails unless out is exactly one JSON object.
func decodeOnly(t *testing.T, out []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(out))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("more than one JSON object on stdout:\n%s", out)
	}
	return m
}

func vpsFixture() *state.State {
	return &state.State{Target: "vps", Version: "0.7.2", BundleDir: "bundle-1", AuthURL: "https://id.example.com",
		ProxyMode: "nginx", PostgresHost: "127.0.0.1:5432", PostgresMode: deploy.PostgresModeBundled}
}

func TestStatusJSON(t *testing.T) {
	compose := recordDeployment(t, vpsFixture())
	stubStatus(t, []deploy.ContainerStatus{{Name: "versola-auth", Service: "auth", Slot: 1, State: "running", Health: "healthy", Image: "ghcr.io/versolauth/versola-auth:0.7.2"}}, nil)
	var out bytes.Buffer
	if err := writeStatusJSON(&out, compose, true); err != nil {
		t.Fatal(err)
	}
	m := decodeOnly(t, out.Bytes())
	if m["deployed"] != true || m["version"] != "0.7.2" || m["target"] != "vps" {
		t.Errorf("got %v", m)
	}
	if c := m["containers"].([]any); len(c) != 1 || c[0].(map[string]any)["health"] != "healthy" {
		t.Errorf("containers %v", m["containers"])
	}
	if w := m["warnings"].([]any); len(w) != 1 || !strings.Contains(w[0].(string), "nothing renews") {
		t.Errorf("warnings %v", m["warnings"])
	}
	if c := m["certificates"].([]any); len(c) != 1 || c[0].(map[string]any)["renewDue"] != false {
		t.Errorf("certificates %v", m["certificates"])
	}
	if strings.Contains(out.String(), fixtureSecret) {
		t.Errorf("the secret reached the output:\n%s", out.String())
	}
}

func TestStatusJSONNothingDeployed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out bytes.Buffer
	_, exists, err := state.ComposeFile()
	if err != nil || exists {
		t.Fatalf("exists %v err %v", exists, err)
	}
	if err := writeStatusJSON(&out, "", false); err != nil {
		t.Fatal(err)
	}
	m := decodeOnly(t, out.Bytes())
	if m["deployed"] != false || m["cliVersion"] != version {
		t.Errorf("got %v", m)
	}
}

// Docker down: the object is still printed (containers null, a warning) and
// the command fails.
func TestStatusJSONDockerDown(t *testing.T) {
	compose := recordDeployment(t, &state.State{Target: "local", Version: "0.7.2", BundleDir: "bundle-1", ProxyMode: "local"})
	stubStatus(t, nil, errors.New("docker compose ps failed: exit status 1\nCannot connect to the Docker daemon"))
	var out bytes.Buffer
	err := writeStatusJSON(&out, compose, true)
	if err == nil {
		t.Fatal("no error with Docker down")
	}
	m := decodeOnly(t, out.Bytes())
	if v, ok := m["containers"]; !ok || v != nil {
		t.Errorf("containers %v (present %v)", v, ok)
	}
	if w := fmt.Sprint(m["warnings"]); !strings.Contains(w, "Docker could not list the containers") || strings.Contains(w, "Cannot connect") {
		t.Errorf("warnings %s", w)
	}
	if db := m["database"].(map[string]any); db["mode"] != "bundled" || db["host"] != nil {
		t.Errorf("local database %v", db)
	}
}

// stats --json: progress on stderr, the object alone on stdout.
func TestStatsJSONStdoutOnly(t *testing.T) {
	recordDeployment(t, vpsFixture())
	old := collectStats
	t.Cleanup(func() { collectStats = old })
	collectStats = func(st *state.State, progress io.Writer) (deploy.StatsReport, error) {
		fmt.Fprintln(progress, "Checking that Postgres has the connections for a stand-in replica...")
		return deploy.StatsReport{Target: st.Target, Containers: []deploy.ContainerStats{}, Warnings: []string{}}, nil
	}
	var stdout, stderr bytes.Buffer
	if err := writeStats(&stdout, &stderr, true); err != nil {
		t.Fatal(err)
	}
	if m := decodeOnly(t, stdout.Bytes()); m["target"] != "vps" {
		t.Errorf("got %v", m)
	}
	if !strings.Contains(stderr.String(), "Checking") {
		t.Errorf("progress not on stderr: %q", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), fixtureSecret) {
		t.Error("the secret reached the output")
	}
}

func TestStatsNothingDeployed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var stdout, stderr bytes.Buffer
	if err := writeStats(&stdout, &stderr, false); err == nil || !strings.Contains(err.Error(), "versola bootstrap") || stdout.Len() != 0 {
		t.Errorf("err %v, stdout %q", err, stdout.String())
	}
}

// The certificate probes run together, and their results keep the order of the
// endpoints (the report lists them in that order).
func TestProbeAllIsParallelAndKeepsOrder(t *testing.T) {
	addrs := []string{"a:443", "b:443", "c:443", "d:443"}
	var running, peak atomic.Int32
	out := probeAll(addrs, func(addr string) deploy.CertInfo {
		n := running.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		return deploy.CertInfo{Name: addr}
	})
	if peak.Load() != int32(len(addrs)) {
		t.Errorf("at most %d probes ran at once, want %d", peak.Load(), len(addrs))
	}
	for i, a := range addrs {
		if out[i].Name != a {
			t.Errorf("result %d is %q, want %q", i, out[i].Name, a)
		}
	}
	if got := probeAll(nil, nil); len(got) != 0 {
		t.Errorf("no endpoints: %v", got)
	}
}
