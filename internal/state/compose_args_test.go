package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
)

func TestComposeArgs(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")

	// No proxy.yml next to it (local, or vps configured before the CLI
	// generated one): just the one file.
	got := ComposeArgs(compose, "up", "-d", "central")
	want := []string{"compose", "-f", compose, "up", "-d", "central"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without proxy.yml: got %v, want %v", got, want)
	}

	// With proxy.yml: both files, always -- so down/status/... include it.
	proxyFile := filepath.Join(dir, proxy.ComposeFile)
	if err := os.WriteFile(proxyFile, []byte("name: versola-vps\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = ComposeArgs(compose, "down")
	want = []string{"compose", "-f", compose, "-f", proxyFile, "down"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with proxy.yml: got %v, want %v", got, want)
	}
}

// replicas.yml goes between compose.yml and proxy.yml, and only when it exists.
func TestComposeArgsWithReplicas(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	replicas := filepath.Join(dir, ReplicasFile)
	proxyFile := filepath.Join(dir, proxy.ComposeFile)
	for _, f := range []string{replicas, proxyFile} {
		if err := os.WriteFile(f, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ComposeArgs(compose, "up")
	want := []string{"compose", "-f", compose, "-f", replicas, "-f", proxyFile, "up"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if err := os.Remove(proxyFile); err != nil {
		t.Fatal(err)
	}
	got = ComposeArgs(compose, "up")
	want = []string{"compose", "-f", compose, "-f", replicas, "up"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without proxy.yml: got %v, want %v", got, want)
	}
}
