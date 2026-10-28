package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

// runDownWith records a deployment of the given target under a temporary home,
// feeds answer to stdin, stands in for Docker and runs `down`; it returns the
// compose arguments it ran with (nil if it never got that far) and its error.
func runDownWith(t *testing.T, target, answer string, yes, volumes bool) (ran []string, err error) {
	return runDownState(t, &state.State{Target: target, Version: "0.7.2", BundleDir: "bundle-1", AuthURL: "https://id.example.com", ProxyMode: "nginx"}, answer, yes, volumes, false, true)
}

func runDownState(t *testing.T, st *state.State, answer string, yes, volumes, force, compose bool) (ran []string, err error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".versola", "active", "bundle-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if compose {
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	downYes, removeVolumes, downForce = yes, volumes, force
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

func TestDownDuringRolloutNeedsForceThenStillAsks(t *testing.T) {
	st := &state.State{Target: "vps", Version: "0.7.2", PreviousVersion: "0.7.1", BundleDir: "bundle-1", AuthURL: "https://id.example.com", ProxyMode: "nginx"}
	if ran, err := runDownState(t, st, "y\n", false, false, false, true); err == nil || !strings.Contains(err.Error(), "rolling upgrade") || ran != nil {
		t.Errorf("no --force: ran %v, err %v; want the rollout refusal before any question", ran, err)
	}
	if ran, err := runDownState(t, st, "n\n", false, false, true, true); err == nil || !strings.Contains(err.Error(), "aborted") || ran != nil {
		t.Errorf("--force still asks on vps: ran %v, err %v", ran, err)
	}
	if ran, err := runDownState(t, st, "y\n", false, false, true, true); err != nil || ran == nil {
		t.Errorf("--force and y: ran %v, err %v", ran, err)
	}
}

func TestDownWithoutComposeFileDoesNotAsk(t *testing.T) {
	st := &state.State{Target: "vps", Version: "0.7.2", BundleDir: "bundle-1", ProxyMode: "nginx"}
	if ran, err := runDownState(t, st, "n\n", false, false, false, false); err != nil || ran != nil {
		t.Errorf("nothing to stop: ran %v, err %v; want no question and no docker call", ran, err)
	}
}
