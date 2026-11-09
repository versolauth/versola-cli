package deploy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestServiceWarningsStates(t *testing.T) {
	for _, tc := range []struct {
		name, state, health, problem string
	}{
		{"healthy", "running", "healthy", ""},
		{"no healthcheck", "running", "", ""},
		{"starting", "running", "starting", ""},
		{"restarting", "restarting", "unhealthy", "is restarting"},
		{"unhealthy", "running", "unhealthy", "is unhealthy"},
		{"stopped", "exited", "healthy", "is not running (state: exited)"},
		{"created", "created", "", "is not running (state: created)"},
		{"dead", "dead", "", "is not running (state: dead)"},
		{"paused", "paused", "healthy", "is not running (state: paused)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			containers := []ContainerStatus{{Name: "versola-auth-2", Service: "auth", Slot: 2, State: tc.state, Health: tc.health}}
			warnings := ServiceWarnings(nil, []string{"auth-2"}, containers)
			if tc.problem == "" {
				if len(warnings) != 0 {
					t.Fatalf("healthy service: %v", warnings)
				}
				return
			}
			want := "service auth-2 (versola-auth-2) " + tc.problem + "; see: `versola logs auth-2 --tail 100`"
			if !reflect.DeepEqual(warnings, []string{want}) {
				t.Fatalf("got %v, want %s", warnings, want)
			}
		})
	}
}

func TestServiceWarningsInventoryAndMissing(t *testing.T) {
	services := []string{"postgres", "proxy", "openbao", "edge", "central", "auth", "mtls-init", "migrate"}
	warnings := ServiceWarnings(&state.State{}, services, []ContainerStatus{
		{Name: "init", Service: "mtls-init", State: "exited", Health: "unhealthy"},
		{Name: "migrate", Service: "migrate", State: "exited"},
	})
	want := []string{"auth", "central", "edge", "openbao", "postgres", "proxy"}
	if len(warnings) != len(want) {
		t.Fatalf("got %v", warnings)
	}
	for i, service := range want {
		expected := "service " + service + " has no container; see: `versola logs " + service + " --tail 100`"
		if warnings[i] != expected {
			t.Errorf("warning %d: %q, want %q", i, warnings[i], expected)
		}
	}
	// An external database or secret store is not a missing Compose service.
	if got := ServiceWarnings(&state.State{Target: "vps"}, []string{"mtls-init"}, nil); len(got) != 0 {
		t.Fatalf("optional services invented: %v", got)
	}
}

func TestServiceWarningsRolloutSlots(t *testing.T) {
	st := &state.State{Surge: map[string]int{AuthService: 3}}
	st.SetSlots(AuthService, []state.Slot{{N: 2, Version: "a"}, {N: 3, Version: "b"}})
	containers := []ContainerStatus{
		{Name: "retired", Service: "auth", Slot: 1, State: "exited"},
		{Name: "active", Service: "auth", Slot: 2, State: "running"},
	}
	warnings := ServiceWarnings(st, []string{"auth", "auth-2", "auth-3"}, containers)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "service auth-3 has no container") {
		t.Fatalf("active slots, including stand-in: %v", warnings)
	}
}

func TestServiceWarningsEachContainer(t *testing.T) {
	// Multiple Docker replicas of one Compose service still each get a warning.
	containers := []ContainerStatus{
		{Name: "one", Service: "central", State: "restarting"},
		{Name: "two", Service: "central", State: "running", Health: "unhealthy"},
	}
	warnings := ServiceWarnings(nil, []string{"central"}, containers)
	if len(warnings) != 2 || !strings.Contains(warnings[0], "(one)") || !strings.Contains(warnings[1], "(two)") {
		t.Fatalf("got %v", warnings)
	}
}
