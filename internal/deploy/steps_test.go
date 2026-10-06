package deploy

import (
	"reflect"
	"testing"
)

func rep(base string, slot int) replica {
	r := replica{Slot: slot, Base: base, Service: base}
	if slot > 1 {
		r.Service = base + "-" + string(rune('0'+slot))
	}
	return r
}

func TestAppStartSteps(t *testing.T) {
	auth := []replica{rep("auth", 1), rep("auth", 2), rep("auth", 3)}
	edge := []replica{rep("edge", 1), rep("edge", 2)}
	got := appStartSteps(auth, edge, false)
	want := []startStep{
		{"Starting auth and edge...", "auth/edge", []string{"up", "-d", "auth", "edge"}, []string{"auth", "edge"}},
		{"Starting auth-2...", "auth-2", []string{"up", "-d", "--no-deps", "auth-2"}, []string{"auth-2"}},
		{"Starting edge-2...", "edge-2", []string{"up", "-d", "--no-deps", "edge-2"}, []string{"edge-2"}},
		{"Starting auth-3...", "auth-3", []string{"up", "-d", "--no-deps", "auth-3"}, []string{"auth-3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps:\n got %+v\nwant %+v", got, want)
	}
}

func TestAppStartStepsLegacyGateway(t *testing.T) {
	got := appStartSteps([]replica{rep("auth", 1)}, []replica{rep("edge", 1)}, true)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Args, []string{"up", "-d", "auth", "edge", "nginx"}) {
		t.Errorf("steps: %+v", got)
	}
}

func TestAppStartStepsWithoutSlotOne(t *testing.T) {
	got := appStartSteps([]replica{rep("auth", 2)}, []replica{rep("edge", 1)}, false)
	if len(got) != 2 || !reflect.DeepEqual(got[0].Ready, []string{"edge"}) || !reflect.DeepEqual(got[1].Args, []string{"up", "-d", "--no-deps", "auth-2"}) {
		t.Errorf("steps: %+v", got)
	}
	if got := appStartSteps(nil, nil, false); len(got) != 0 {
		t.Errorf("no replicas, steps %+v", got)
	}
}
