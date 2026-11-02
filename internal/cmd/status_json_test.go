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
	t.Cleanup(func() {
		statusContainers, statusCertReport, statusProbeCerts, statusCertsWarning, statusNow = oc, ocr, op, ow, on
	})
	statusContainers = func(string) ([]deploy.ContainerStatus, error) { return containers, dockerErr }
	statusCertReport = func(*state.State) ([]deploy.CertInfo, error) {
		now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		return []deploy.CertInfo{{Name: "server.crt", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)}}, nil
	}
	statusProbeCerts = func(*state.State) []deploy.CertInfo { return nil }
	statusCertsWarning = func(*state.State) string { return "nothing renews the mutual-TLS certificates" }
	statusNow = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
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
