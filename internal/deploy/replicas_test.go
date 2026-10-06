package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

const localCompose = `name: versola-test
services:
  postgres:
    image: postgres:18
  central:
    image: central:1
    container_name: versola-central
    environment: {PORT: "8090", DPORT: "8091"}
    ports: ["127.0.0.1:8091:8091"]
  auth:
    image: auth:1
    container_name: versola-auth
    environment: {PORT: "8080", DPORT: "8081", APORT: "8082", KEEP: "yes"}
    ports: ["127.0.0.1:8081:8081", "8082"]
    depends_on: {central: {condition: service_started}}
    volumes: ["./cfg:/cfg:ro"]
  edge:
    image: edge:1
    container_name: versola-edge
    environment: {PORT: "8095", DPORT: "8096"}
    ports: ["127.0.0.1:8096:8096"]
`

const vpsCompose = `name: versola-test
services:
  central:
    image: central:1
    network_mode: host
    environment: {PORT: "8090", DPORT: "8091"}
  auth:
    image: auth:1
    container_name: versola-auth
    network_mode: host
    environment: {PORT: "8080", DPORT: "8081", APORT: "8082"}
  edge:
    image: edge:1
    network_mode: host
    environment: {PORT: "8095", DPORT: "8096"}
`

func replicasFromCompose(t *testing.T, compose string, authSlots, edgeSlots []int) (auth, edge []replica) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	topo := composeTopology(t, path)
	slots := func(ns []int) []state.Slot {
		var out []state.Slot
		for _, n := range ns {
			out = append(out, state.Slot{N: n})
		}
		return out
	}
	var err error
	if auth, err = replicasOf(topo, AuthService, slots(authSlots)); err != nil {
		t.Fatal(err)
	}
	if edge, err = replicasOf(topo, EdgeService, slots(edgeSlots)); err != nil {
		t.Fatal(err)
	}
	return auth, edge
}

// composeTopology reads a compose file through the real docker compose
// when there is one, else skips.
func composeTopology(t *testing.T, composePath string, extra ...string) topology.Topology {
	t.Helper()
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	args := []string{"compose", "-f", composePath}
	for _, f := range extra {
		args = append(args, "-f", f)
	}
	out, err := exec.Command("docker", append(args, "config", "--format", "json")...).Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	topo, err := topology.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func TestReplicasYAMLNothingForSlotOne(t *testing.T) {
	topo := testTopology(t)
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, any, err := replicasYAML("compose.yml", auth)
	if err != nil || any {
		t.Errorf("one replica needs no file: %v, %v", any, err)
	}
}

func TestReplicasYAMLText(t *testing.T) {
	topo := testTopology(t)
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, any, err := replicasYAML("compose.yml", auth)
	if err != nil || !any {
		t.Fatalf("%v, %v", any, err)
	}
	got := string(b)
	for _, want := range []string{
		"  auth-2:\n    extends:\n      file: \"compose.yml\"\n      service: auth\n",
		"      PORT: \"8180\"\n      DPORT: \"8181\"\n      APORT: \"8182\"\n",
		"    ports: !override\n      - target: 8181\n        published: \"8181\"\n        protocol: tcp\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "  auth:\n") {
		t.Error("slot 1 is the base service, not written")
	}
}

// What the file means is Compose's to say: render it, hand it to the real
// docker compose together with the base file, and read the result back
// through the same topology reader the CLI uses.
func TestReplicasYAMLThroughRealCompose(t *testing.T) {
	auth, edge := replicasFromCompose(t, localCompose, []int{1, 2}, []int{1, 3})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(localCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeReplicasFile(dir, "compose.yml", auth, edge); err != nil {
		t.Fatal(err)
	}
	topo := composeTopology(t, filepath.Join(dir, "compose.yml"), filepath.Join(dir, state.ReplicasFile))

	a1, _ := topo.Service("auth")
	if a1.Port != 8080 || len(a1.Publications()) != 1 || a1.ContainerName() != "versola-auth" {
		t.Errorf("auth (slot 1) must be untouched by the file: %+v %v", a1, a1.Publications())
	}
	a2, err := topo.Service("auth-2")
	if err != nil {
		t.Fatalf("auth-2: %v", err)
	}
	if a2.Port != 8180 || a2.DiagnosticsPort != 8181 || a2.AdditionalPort != 8182 || a2.ContainerName() != "versola-auth-2" {
		t.Errorf("auth-2: %+v", a2)
	}
	var pubs []string
	for _, p := range a2.Publications() {
		pubs = append(pubs, p.String())
	}
	if len(pubs) != 1 || pubs[0] != "8181 published on 127.0.0.1:8181" {
		t.Errorf("auth-2 publishes %v: the base's ports must be replaced, not added to", pubs)
	}
	if got := a2.UnfixedPorts(); len(got) != 1 || got[0] != 8182 {
		t.Errorf("auth-2 unfixed: %v", got)
	}
	e3, err := topo.Service("edge-3")
	if err != nil {
		t.Fatalf("edge-3: %v", err)
	}
	if e3.Port != 8295 || e3.DiagnosticsPort != 8296 || e3.ContainerName() != "versola-edge-3" {
		t.Errorf("edge-3: %+v", e3)
	}
	if got := e3.ProbeAddr(8296); got != "127.0.0.1:8296" {
		t.Errorf("edge-3 probe: %s", got)
	}

	// The CLI's own check accepts what it generated.
	c, err := replicasOf(topo, CentralService, []state.Slot{{N: 1}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	e, err := replicasOf(topo, EdgeService, []state.Slot{{N: 1}, {N: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkTopology(topo, false, nil, c, a, e); err != nil {
		t.Errorf("checkTopology on the generated replicas: %v", err)
	}
}

func TestReplicasYAMLHostNetworkThroughRealCompose(t *testing.T) {
	auth, edge := replicasFromCompose(t, vpsCompose, []int{1, 2}, []int{1})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(vpsCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeReplicasFile(dir, "compose.yml", auth, edge); err != nil {
		t.Fatal(err)
	}
	topo := composeTopology(t, filepath.Join(dir, "compose.yml"), filepath.Join(dir, state.ReplicasFile))
	a2, err := topo.Service("auth-2")
	if err != nil {
		t.Fatal(err)
	}
	if !a2.HostNetwork() || a2.Port != 8180 || a2.DiagnosticsPort != 8181 || len(a2.Publications()) != 0 {
		t.Errorf("auth-2 on vps: %+v", a2)
	}
}

// The file is removed, not left behind naming replicas that are gone.
func TestWriteReplicasFileRemovesStaleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, state.ReplicasFile)
	topo := testTopology(t)
	two, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReplicasFile(dir, "compose.yml", two); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("not written: %v", err)
	}
	one, _ := replicasOf(topo, AuthService, []state.Slot{{N: 1}})
	if err := writeReplicasFile(dir, "compose.yml", one); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a stale replicas.yml is left: %v", err)
	}
	// Nothing to remove is not an error.
	if err := writeReplicasFile(dir, "compose.yml", one); err != nil {
		t.Errorf("no file: %v", err)
	}
}

func TestServiceNameOK(t *testing.T) {
	for s, want := range map[string]bool{"auth": true, "auth-2": true, "a_b.c": true, "": false, "Auth": false, "-a": false, "a b": false, "a\nb: x": false, "a:b": false} {
		if got := serviceNameOK(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}

// A port published on a loopback address with no host port keeps that
// address on the replica: otherwise it would bind every address.
func TestReplicasYAMLKeepsHostIPOfUnfixedPort(t *testing.T) {
	topo, err := topology.Parse([]byte(`{"services":{"auth":{"container_name":"versola-auth","ports":[
		{"target":8081,"published":"8081","host_ip":"127.0.0.1"},
		{"target":8082,"host_ip":"127.0.0.1"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := replicasOf(topo, AuthService, []state.Slot{{N: 1}, {N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := replicasYAML("compose.yml", auth)
	if err != nil {
		t.Fatal(err)
	}
	want := "      - target: 8182\n        host_ip: \"127.0.0.1\"\n        protocol: tcp\n"
	if !strings.Contains(string(b), want) {
		t.Errorf("missing %q in\n%s", want, b)
	}
}
