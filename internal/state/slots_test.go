package state

import (
	"encoding/json"
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
