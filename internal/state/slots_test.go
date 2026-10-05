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
