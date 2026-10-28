package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

// downEnv records a deployment of the given target under a temporary home,
// feeds answer to stdin and stands in for Docker; it returns the compose
// arguments `down` ran with (nil if it never got that far).
func runDownWith(t *testing.T, target, answer string, yes, volumes bool) (ran []string, err error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".versola", "active", "bundle-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &state.State{Target: target, Version: "0.7.2", BundleDir: "bundle-1", AuthURL: "https://id.example.com", ProxyMode: "nginx"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	w.WriteString(answer)
	w.Close()
	oldIn, oldCompose, oldYes, oldVol, oldForce := os.Stdin, downCompose, downYes, removeVolumes, downForce
	os.Stdin = r
	downCompose = func(args []string) error { ran = args; return nil }
	downYes, removeVolumes, downForce = yes, volumes, false
	t.Cleanup(func() {
		os.Stdin, downCompose, downYes, removeVolumes, downForce = oldIn, oldCompose, oldYes, oldVol, oldForce
		r.Close()
	})

	err = runDown(downCmd, nil)
	return ran, err
}

func TestDownOnVpsAsksFirst(t *testing.T) {
	if ran, err := runDownWith(t, "vps", "n\n", false, false); err == nil || !strings.Contains(err.Error(), "aborted") || ran != nil {
		t.Errorf("answer n: ran %v, err %v; want aborted and nothing run", ran, err)
	}
	if ran, err := runDownWith(t, "vps", "", false, false); err == nil || ran != nil {
		t.Errorf("no answer (closed stdin): ran %v, err %v; want refused", ran, err)
	}
	if ran, err := runDownWith(t, "vps", "y\n", false, false); err != nil || ran == nil || ran[len(ran)-1] != "down" {
		t.Errorf("answer y: ran %v, err %v; want compose down", ran, err)
	}
}

func TestDownOnVpsYesSkipsTheQuestion(t *testing.T) {
	// Stdin holds a "n" that would abort if it were read.
	ran, err := runDownWith(t, "vps", "n\n", true, true)
	if err != nil || ran == nil || ran[len(ran)-1] != "--volumes" {
		t.Errorf("--yes --volumes: ran %v, err %v; want compose down --volumes", ran, err)
	}
}

func TestDownOnLocalNeverAsks(t *testing.T) {
	ran, err := runDownWith(t, "local", "n\n", false, false)
	if err != nil || ran == nil {
		t.Errorf("local: ran %v, err %v; want it to stop without a question", ran, err)
	}
}
