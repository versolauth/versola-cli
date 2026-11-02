package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

// runLogsWith records st (nil: nothing) under a temporary home, with or
// without its compose.yml, stands in for Docker and runs `versola logs args`
// with the flags as set; it returns the docker arguments it would have run
// (nil if it never got that far) and its error.
func runLogsWith(t *testing.T, st *state.State, compose bool, args []string, tail string, follow bool) (ran []string, composePath string, err error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".versola", "active", "bundle-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	composePath = filepath.Join(dir, "compose.yml")
	if compose {
		if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if st != nil {
		if err := st.Save(); err != nil {
			t.Fatal(err)
		}
	}
	oldServices, oldRun := logsServices, logsRun
	oldFollow, oldSince, oldTail, oldTs := logsFollow, logsSince, logsTail, logsTimestamps
	logsServices = func(string) ([]string, error) {
		return []string{"auth", "auth-2", "central", "edge", "openbao", "proxy", "step-ca"}, nil
	}
	logsRun = func(a []string) error { ran = a; return nil }
	logsFollow, logsSince, logsTail, logsTimestamps = follow, "", tail, false
	t.Cleanup(func() {
		logsServices, logsRun = oldServices, oldRun
		logsFollow, logsSince, logsTail, logsTimestamps = oldFollow, oldSince, oldTail, oldTs
	})
	err = runLogs(logsCmd, args)
	return ran, composePath, err
}

func TestLogsNothingDeployed(t *testing.T) {
	deployed := &state.State{Target: "local", Version: "0.7.2", BundleDir: "bundle-1"}
	for name, tc := range map[string]struct {
		st      *state.State
		compose bool
	}{
		"no state":       {nil, false},
		"no compose.yml": {deployed, false},
	} {
		ran, _, err := runLogsWith(t, tc.st, tc.compose, nil, "200", false)
		if !errors.Is(err, errNothingDeployed) || ran != nil {
			t.Errorf("%s: ran %v, err %v", name, ran, err)
		}
	}
	if !strings.Contains(errNothingDeployed.Error(), "versola bootstrap") {
		t.Errorf("no way out in %q", errNothingDeployed)
	}
}

func TestLogsRunsComposeLogs(t *testing.T) {
	st := &state.State{Target: "vps", Version: "0.7.2", BundleDir: "bundle-1", AuthURL: "https://id.example.com", ProxyMode: "nginx"}
	ran, composePath, err := runLogsWith(t, st, true, []string{"auth"}, "200", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"compose", "-f", composePath, "logs", "--tail", "200", "--follow", "auth", "auth-2"}
	if !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}

	ran, _, err = runLogsWith(t, st, true, []string{"postgres"}, "200", false)
	if err == nil || !strings.Contains(err.Error(), "your own Postgres") || ran != nil {
		t.Errorf("external postgres: ran %v, err %v", ran, err)
	}

	ran, composePath, err = runLogsWith(t, st, true, nil, "all", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"compose", "-f", composePath, "logs", "--tail", "all"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
}

// Even an unfiltered log request must reject a bad compose configuration
// before starting the streaming command, whose stderr goes to the terminal.
func TestLogsValidatesComposeBeforeStreamingAllServices(t *testing.T) {
	st := &state.State{Target: "local", Version: "0.7.2", BundleDir: "bundle-1"}
	_, _, err := runLogsWith(t, st, true, nil, "200", false)
	if err != nil {
		t.Fatal(err)
	}
	safe := errors.New("couldn't read the compose configuration")
	logsServices = func(string) ([]string, error) { return nil, safe }
	ran := false
	logsRun = func([]string) error { ran = true; return nil }
	if err := runLogs(logsCmd, nil); !errors.Is(err, safe) || ran {
		t.Fatalf("got error %v, streamed %v; want safe validation failure before streaming", err, ran)
	}
}
