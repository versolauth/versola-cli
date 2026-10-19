package state

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
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

// An abort's preparation carries the stand-in and the migrations over, and
// marks the record as going back.
func TestFinalizeStagedForAnAbort(t *testing.T) {
	dir := isolate(t)
	for _, b := range []string{"bundle-old", "bundle-new"} {
		if err := os.MkdirAll(filepath.Join(dir, b), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	migrated := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	prev := &State{Target: "vps", Version: "0.7.0", PreviousVersion: "0.6.2", BundleDir: "bundle-new", ProxyMode: "external",
		AuthURL: "https://id.example.com", PostgresHost: "127.0.0.1:5432", MountedBundleDirs: []string{"bundle-old", "bundle-new"},
		MigratedAt: &migrated, Surge: map[string]int{"auth": 2}, SecretsRevision: 3}
	prev.SetSlots("auth", []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.7.0"}})
	if err := prev.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize(FinalizeOptions{Target: "vps", Version: "0.6.2", BundleDir: back,
		AuthURL: "https://id.example.com", ProxyMode: "external", PostgresHost: "127.0.0.1:5432", SecretsRevision: 2,
		Slots:           map[string][]Slot{"auth": prev.ActiveSlots("auth"), "edge": prev.ActiveSlots("edge")},
		PreviousVersion: "0.7.0", Surge: prev.Surge, MigratedAt: prev.MigratedAt, RollingBack: true}); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "0.6.2" || s.PreviousVersion != "0.7.0" || !s.RollingBack || s.Surge["auth"] != 2 || s.MigratedAt == nil || !s.MigratedAt.Equal(migrated) {
		t.Errorf("state: %+v", s)
	}
	if s.SecretsRevision != 3 {
		t.Errorf("SecretsRevision lowered to %d by going back", s.SecretsRevision)
	}
	if got := s.ActiveSlots("edge"); !reflect.DeepEqual(got, []Slot{{N: 1, Version: "0.7.0"}}) {
		t.Errorf("edge slots: %v", got)
	}
	// Both bundles containers may mount stay until the abort finishes.
	want := []string{filepath.Base(back), "bundle-new", "bundle-old"}
	sort.Strings(want)
	if got := bundles(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("bundles: %v", got)
	}
}

func TestRestorePrevious(t *testing.T) {
	dir := isolate(t)
	for _, b := range []string{"bundle-old", "bundle-new", "bundle-openbao"} {
		if err := os.MkdirAll(filepath.Join(dir, b), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle-old", "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &State{Target: "vps", Version: "0.7.0", PreviousVersion: "0.6.2", BundleDir: "bundle-new", ProxyMode: "external",
		AuthURL: "https://id.example.com", PostgresHost: "127.0.0.1:5432", MountedBundleDirs: []string{"bundle-old", "bundle-new"}, SecretsRevision: 3}
	st.SetSlots("auth", []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}})
	st.SetSlots("edge", []Slot{{N: 1, Version: "0.6.2"}})
	st.SetSlots("central", []Slot{{N: 1, Version: "0.6.2"}})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	if err := RestorePrevious("bundle-old", at, "bundle-openbao"); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "0.6.2" || s.RolloutUnderway() || s.MixedVersions() != "" || !s.RunsFromCurrentBundle() || s.BundleDir != "bundle-old" {
		t.Errorf("state: %+v", s)
	}
	if s.MigratedAt == nil || !s.MigratedAt.Equal(at) || s.SecretsRevision != 3 {
		t.Errorf("migratedAt %v, secrets revision %d", s.MigratedAt, s.SecretsRevision)
	}
	if got := slotNs(s.ActiveSlots("auth")); !reflect.DeepEqual(got, []int{1, 2}) || s.ActiveSlots("edge")[0].Version != "0.6.2" || s.Slots["central"] != nil {
		t.Errorf("slots: %v %v", s.ActiveSlots("auth"), s.ActiveSlots("edge"))
	}
	if got := bundles(t, dir); !reflect.DeepEqual(got, []string{"bundle-old", "bundle-openbao"}) {
		t.Errorf("bundles: %v", got)
	}
	// Twice: nothing is recorded under way any more.
	if err := RestorePrevious("bundle-old", at); err == nil {
		t.Error("restored with no rollout recorded")
	}
}

func TestRestorePreviousNeedsTheOldBundle(t *testing.T) {
	if err := os.MkdirAll(isolate(t), 0o700); err != nil {
		t.Fatal(err)
	}
	st := &State{Target: "vps", Version: "0.7.0", PreviousVersion: "0.6.2", BundleDir: "bundle-new", MountedBundleDirs: []string{"bundle-old", "bundle-new"}}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := RestorePrevious("bundle-old", time.Now()); err == nil {
		t.Error("restored a bundle that is not there")
	}
	if s, _ := Load(); s.Version != "0.7.0" || s.PreviousVersion != "0.6.2" {
		t.Errorf("the record changed: %+v", s)
	}
}

func slotNs(ss []Slot) []int {
	var out []int
	for _, sl := range ss {
		out = append(out, sl.N)
	}
	return out
}
