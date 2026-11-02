package deploy

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/state"
)

var deployedServices = []string{"auth", "auth-3", "central", "central-2", "edge", "edge-2", "mtls-init", "openbao", "postgres", "proxy", "step-ca"}

func TestResolveLogTargets(t *testing.T) {
	st := &state.State{Target: "local"}
	for _, tc := range []struct {
		names []string
		want  []string
	}{
		{nil, nil},
		{[]string{"openbao"}, []string{"openbao"}},
		// central with a stand-in of a rolling upgrade.
		{[]string{"central"}, []string{"central", "central-2"}},
		// A base name is every slot of it.
		{[]string{"auth"}, []string{"auth", "auth-3"}},
		{[]string{"edge"}, []string{"edge", "edge-2"}},
		// One slot only.
		{[]string{"auth-3"}, []string{"auth-3"}},
		// Slot 1 by its number.
		{[]string{"auth-1"}, []string{"auth"}},
		// Not replicas of a service "step" or "mtls".
		{[]string{"step-ca"}, []string{"step-ca"}},
		{[]string{"mtls-init"}, []string{"mtls-init"}},
		// In the order given, without repeats.
		{[]string{"proxy", "auth", "auth-3", "auth-1"}, []string{"proxy", "auth", "auth-3"}},
	} {
		got, err := ResolveLogTargets(tc.names, deployedServices, st)
		if err != nil {
			t.Errorf("%v: %v", tc.names, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: got %v, want %v", tc.names, got, tc.want)
		}
	}
}

func TestResolveLogTargetsUnknownNameListsTheValidOnes(t *testing.T) {
	for _, name := range []string{"nginx", "auth-2", "versola-proxy"} {
		_, err := ResolveLogTargets([]string{"central", name}, deployedServices, &state.State{Target: "local"})
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if !strings.Contains(err.Error(), `"`+name+`"`) || !strings.Contains(err.Error(), strings.Join(deployedServices, ", ")) {
			t.Errorf("%s: error doesn't name it and list the services: %v", name, err)
		}
	}
}

func TestResolveLogTargetsExternalPostgres(t *testing.T) {
	services := []string{"auth", "central", "edge", "openbao", "proxy", "step-ca"}
	st := &state.State{Target: "vps", PostgresHost: "db.example.com:5432"}
	_, err := ResolveLogTargets([]string{"postgres"}, services, st)
	if err == nil || !strings.Contains(err.Error(), "your own Postgres (db.example.com:5432)") || strings.Contains(err.Error(), "it has:") {
		t.Errorf("external Postgres: %v", err)
	}
	// A bundled one that is somehow not in the compose file is an unknown
	// name like any other, not "your own".
	st.PostgresMode = PostgresModeBundled
	_, err = ResolveLogTargets([]string{"postgres"}, services, st)
	if err == nil || !strings.Contains(err.Error(), "it has:") {
		t.Errorf("bundled Postgres missing: %v", err)
	}
	// Bundled and there.
	got, err := ResolveLogTargets([]string{"postgres"}, append(services, "postgres"), st)
	if err != nil || !reflect.DeepEqual(got, []string{"postgres"}) {
		t.Errorf("bundled Postgres: %v, %v", got, err)
	}
}

func TestLogsArgs(t *testing.T) {
	dir := t.TempDir()
	composePath := dir + string(os.PathSeparator) + "compose.yml"
	base := []string{"compose", "-f", composePath}
	for _, tc := range []struct {
		name string
		o    LogsOptions
		want []string
	}{
		{"default tail, all services, prefix", LogsOptions{}, []string{"logs", "--tail", "200"}},
		{"follow keeps the default tail", LogsOptions{Follow: true}, []string{"logs", "--tail", "200", "--follow"}},
		{"tail all", LogsOptions{Tail: "all"}, []string{"logs", "--tail", "all"}},
		{"tail n", LogsOptions{Tail: "50"}, []string{"logs", "--tail", "50"}},
		{"since and timestamps", LogsOptions{Since: "10m", Timestamps: true}, []string{"logs", "--tail", "200", "--since", "10m", "--timestamps"}},
		{"one service: no prefix", LogsOptions{Services: []string{"central"}}, []string{"logs", "--tail", "200", "--no-log-prefix", "central"}},
		{"several: prefix", LogsOptions{Services: []string{"auth", "auth-3"}}, []string{"logs", "--tail", "200", "auth", "auth-3"}},
	} {
		got, err := LogsArgs(composePath, tc.o)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if want := append(append([]string{}, base...), tc.want...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
	}
	for _, bad := range []string{"-1", "ten", "10m"} {
		if _, err := LogsArgs(composePath, LogsOptions{Tail: bad}); err == nil || !strings.Contains(err.Error(), "--tail") {
			t.Errorf("--tail %s: %v", bad, err)
		}
	}
}

func TestRunLogsCtrlCIsNotAnError(t *testing.T) {
	old := logsSignalGrace
	logsSignalGrace = 50 * time.Millisecond
	t.Cleanup(func() { logsSignalGrace = old })
	failed := errors.New("exit status 130")

	// The signal arrived before docker's exit was collected.
	sigs := make(chan os.Signal, 1)
	run := func(...string) error { sigs <- os.Interrupt; return failed }
	if err := runLogs(nil, run, sigs); err != nil {
		t.Errorf("Ctrl-C before the exit: %v", err)
	}

	// The signal arrives just after docker exited (the race RunLogs waits for).
	sigs = make(chan os.Signal, 1)
	run = func(...string) error {
		go func() { time.Sleep(10 * time.Millisecond); sigs <- os.Interrupt }()
		return failed
	}
	if err := runLogs(nil, run, sigs); err != nil {
		t.Errorf("Ctrl-C just after the exit: %v", err)
	}

	// No signal: docker's failure is the command's.
	sigs = make(chan os.Signal, 1)
	if err := runLogs(nil, func(...string) error { return failed }, sigs); !errors.Is(err, failed) {
		t.Errorf("without a signal: %v", err)
	}
	if err := runLogs(nil, func(...string) error { return nil }, sigs); err != nil {
		t.Errorf("success: %v", err)
	}
}

func TestLogsHint(t *testing.T) {
	if got, want := LogsHint("central"), "see: `versola logs central --tail 100`"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
