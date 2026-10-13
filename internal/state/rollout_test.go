package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A rolling upgrade's preparation records the replicas that keep running at
// the version they run, and where they came from; the old bundle stays.
func TestFinalizeStagedForARollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	oldBundle := filepath.Join(dir, "bundle-old")
	if err := os.MkdirAll(oldBundle, 0o700); err != nil {
		t.Fatal(err)
	}
	prev := &State{Target: "vps", Version: "0.6.2", BundleDir: "bundle-old", ProxyMode: "external",
		AuthURL: "https://id.example.com", PostgresHost: "127.0.0.1:5432", MountedBundleDirs: []string{"bundle-old"}}
	prev.SetSlots("auth", []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}})
	if err := prev.Save(); err != nil {
		t.Fatal(err)
	}

	newBundle, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize(FinalizeOptions{Target: "vps", Version: "0.7.0", BundleDir: newBundle,
		AuthURL: "https://id.example.com", ProxyMode: "external", PostgresHost: "127.0.0.1:5432",
		Slots:           map[string][]Slot{"auth": prev.ActiveSlots("auth"), "edge": prev.ActiveSlots("edge")},
		PreviousVersion: "0.6.2"}); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "0.7.0" || s.PreviousVersion != "0.6.2" || !s.RolloutUnderway() || s.MigratedAt != nil {
		t.Errorf("state: %+v", s)
	}
	// Slot 1 of edge alone, at the old version: recorded explicitly, or it
	// would read as the new one.
	if got := s.ActiveSlots("edge"); !reflect.DeepEqual(got, []Slot{{N: 1, Version: "0.6.2"}}) {
		t.Errorf("edge slots: %v", got)
	}
	if got := s.ActiveSlots("auth"); !reflect.DeepEqual(got, []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}}) {
		t.Errorf("auth slots: %v", got)
	}
	if s.MixedVersions() == "" {
		t.Error("a prepared rollout must read as mixed versions (up and replica refuse it)")
	}
	if s.RunsFromCurrentBundle() {
		t.Error("the new bundle is not what runs yet")
	}
	if _, err := os.Stat(oldBundle); err != nil {
		t.Errorf("the running bundle was removed: %v", err)
	}
}

func TestRolloutUnderway(t *testing.T) {
	if (&State{Version: "1"}).RolloutUnderway() {
		t.Error("plain deployment")
	}
	if !(&State{Version: "1", Surge: map[string]int{"auth": 2}}).RolloutUnderway() {
		t.Error("a stand-in alone")
	}
}
