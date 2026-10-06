package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestActiveSlotsImplicit(t *testing.T) {
	st := &State{Version: "0.6.2"}
	want := []Slot{{N: 1, Version: "0.6.2"}}
	if got := st.ActiveSlots("auth"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Slots recorded for another service don't leak into this one.
	st.Slots = map[string][]Slot{"edge": {{N: 2, Version: "0.6.3"}}}
	if got := st.ActiveSlots("auth"); !reflect.DeepEqual(got, want) {
		t.Errorf("auth: got %v, want %v", got, want)
	}
}

func TestActiveSlotsRecorded(t *testing.T) {
	recorded := []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.3"}}
	st := &State{Version: "0.6.3", Slots: map[string][]Slot{"auth": recorded}}
	got := st.ActiveSlots("auth")
	if !reflect.DeepEqual(got, recorded) {
		t.Fatalf("got %v, want %v", got, recorded)
	}
	// A copy: a caller sorting or editing the result must not rewrite the state.
	got[0].N = 9
	if st.Slots["auth"][0].N != 1 {
		t.Error("ActiveSlots returned the state's own slice")
	}
}

// state.json written before slots existed must read back as one implicit
// slot, and a state without slots must not write the key at all -- so an
// older CLI reading it back sees what it always saw.
func TestSlotsJSONCompatibility(t *testing.T) {
	var old State
	if err := json.Unmarshal([]byte(`{"schemaVersion":1,"target":"vps","version":"0.6.2"}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.ActiveSlots("edge"); !reflect.DeepEqual(got, []Slot{{N: 1, Version: "0.6.2"}}) {
		t.Errorf("old state: got %v", got)
	}

	out, err := json.Marshal(State{SchemaVersion: 1, Target: "vps", Version: "0.6.2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "slots") {
		t.Errorf("a state without slots must not write them: %s", out)
	}

	withSlots := State{SchemaVersion: 1, Target: "vps", Version: "0.6.3", Slots: map[string][]Slot{"auth": {{N: 2, Version: "0.6.3"}}}}
	out, err = json.Marshal(withSlots)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Slots, withSlots.Slots) {
		t.Errorf("round trip: got %v, want %v", back.Slots, withSlots.Slots)
	}
}

// A state.json from a newer versola may hold what this one would drop on
// its next Save.
func TestLoadRefusesNewerSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(v int) {
		t.Helper()
		b := fmt.Sprintf(`{"schemaVersion":%d,"target":"vps","version":"0.6.2"}`, v)
		if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(SchemaVersion)
	if _, err := Load(); err != nil {
		t.Errorf("the current layout: %v", err)
	}
	write(SchemaVersion + 1)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "newer versola") {
		t.Errorf("a newer layout: got %v", err)
	}
}

// configure must not overwrite what a newer versola wrote, nor prune the
// bundles it names.
func TestFinalizeRefusesNewerSchema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bundle-old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bundle-new"), 0o700); err != nil {
		t.Fatal(err)
	}
	written := fmt.Sprintf(`{"schemaVersion":%d,"target":"vps","version":"0.6.2","bundleDir":"bundle-old"}`, SchemaVersion+1)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Finalize("vps", "0.6.3", filepath.Join(dir, "bundle-new"), "https://id.example.com", "external")
	if !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("got %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil || string(b) != written {
		t.Errorf("state.json was rewritten: %s, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bundle-old")); err != nil {
		t.Errorf("a bundle of the newer state was pruned: %v", err)
	}
}

func TestSetSlots(t *testing.T) {
	st := &State{Version: "0.6.2"}
	st.SetSlots("auth", []Slot{{N: 3, Version: "0.6.2"}, {N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}})
	want := []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}, {N: 3, Version: "0.6.2"}}
	if got := st.ActiveSlots("auth"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Back to the implicit single replica: nothing recorded.
	st.SetSlots("auth", []Slot{{N: 1, Version: "0.6.2"}})
	if st.Slots != nil {
		t.Errorf("slots still recorded: %v", st.Slots)
	}
	// Another service's record survives.
	st.SetSlots("edge", []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}})
	st.SetSlots("auth", []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}})
	st.SetSlots("auth", []Slot{{N: 1, Version: "0.6.2"}})
	if len(st.Slots["edge"]) != 2 || st.Slots["auth"] != nil {
		t.Errorf("slots: %v", st.Slots)
	}
	// One replica at another version is not the implicit state.
	st.SetSlots("auth", []Slot{{N: 2, Version: "0.6.2"}})
	if len(st.Slots["auth"]) != 1 {
		t.Errorf("slot 2 alone must be recorded: %v", st.Slots)
	}
}

func TestSaveSlotsKeepsTheRestOfTheRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := &State{SchemaVersion: SchemaVersion, Target: "vps", Version: "0.6.2", AuthURL: "https://id.example.com", ProxyMode: "nginx"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := SaveSlots("auth", []Slot{{N: 1, Version: "0.6.2"}, {N: 2, Version: "0.6.2"}}); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthURL != "https://id.example.com" || got.ProxyMode != "nginx" || len(got.ActiveSlots("auth")) != 2 {
		t.Errorf("state: %+v", got)
	}
	if err := SaveSlots("auth", []Slot{{N: 1, Version: "0.6.2"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ = Load(); got.Slots != nil {
		t.Errorf("slots after going back to one: %v", got.Slots)
	}
}

func TestRunsFromCurrentBundle(t *testing.T) {
	for name, c := range map[string]struct {
		st   State
		want bool
	}{
		"legacy layout":            {State{}, true},
		"after a finished up":      {State{BundleDir: "bundle-2", MountedBundleDirs: []string{"bundle-2"}}, true},
		"configured, not yet up":   {State{BundleDir: "bundle-2", MountedBundleDirs: []string{"bundle-1"}}, false},
		"up started, not finished": {State{BundleDir: "bundle-2", MountedBundleDirs: []string{"bundle-1", "bundle-2"}}, false},
		"record before the field":  {State{BundleDir: "bundle-2"}, false},
	} {
		if got := c.st.RunsFromCurrentBundle(); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}
