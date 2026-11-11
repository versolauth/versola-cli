package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// PostgresMode is optional in the record (a layout 5 field, see below): a
// record without it loads as before, and
// an external deployment's record does not gain the key.
func TestPostgresModeIsOptionalInTheRecord(t *testing.T) {
	b, err := json.Marshal(State{Target: "vps", PostgresHost: "db.example.com:5432"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "postgresMode") {
		t.Errorf("an external deployment's record must not carry the key: %s", b)
	}

	var old State
	if err := json.Unmarshal([]byte(`{"target":"vps","postgresHost":"127.0.0.1:5432"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.PostgresMode != "" {
		t.Errorf("a record without the key is external, got %q", old.PostgresMode)
	}

	b, err = json.Marshal(State{Target: "vps", PostgresHost: "127.0.0.1:5432", PostgresMode: "bundled"})
	if err != nil {
		t.Fatal(err)
	}
	var round State
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.PostgresMode != "bundled" || !strings.Contains(string(b), `"postgresMode":"bundled"`) {
		t.Errorf("round trip lost the mode: %s -> %+v", b, round)
	}
}

// The layout a record is written in follows what is in it. A record with
// PostgresMode or EstEnabled is layout 5: an older CLI (layout 4 at most)
// refuses it instead of reading it, ignoring the field and dropping it on its
// next Save, which would turn a deployment with its own Postgres in a
// container into an external Postgres on loopback with no container in the
// compose file (and switch EST off). Every other record stays in layout 4,
// which the CLIs of 0.2.10 and later read, so going back to an older CLI costs
// only the deployment that could not survive it.
func TestLayoutFollowsThePostgresMode(t *testing.T) {
	dir := activeDir(t)
	read := func() (layout int, hasMode bool) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			SchemaVersion int    `json:"schemaVersion"`
			PostgresMode  string `json:"postgresMode"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		return f.SchemaVersion, f.PostgresMode != ""
	}

	// Save, from a record of either kind.
	plain := &State{Target: "vps", Version: "0.7.5", PostgresHost: "db.example.com:5432"}
	if err := plain.Save(); err != nil {
		t.Fatal(err)
	}
	if l, mode := read(); l != 4 || mode {
		t.Errorf("an external deployment: layout %d (mode %v), want 4 without the mode", l, mode)
	}
	bundled := &State{Target: "vps", Version: "0.7.5", PostgresHost: "127.0.0.1:5432", PostgresMode: "bundled"}
	if err := bundled.Save(); err != nil {
		t.Fatal(err)
	}
	if l, mode := read(); l != 5 || !mode {
		t.Errorf("a deployment with its own Postgres: layout %d (mode %v), want 5 with the mode", l, mode)
	}
	// A record read at layout 4 and saved again does not move up, and one at 5 does not move down.
	if err := plain.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	if l, _ := read(); l != 4 {
		t.Errorf("a layout 4 record moved to layout %d", l)
	}
	if err := bundled.Save(); err != nil {
		t.Fatal(err)
	}
	if loaded, err = Load(); err != nil || loaded.PostgresMode != "bundled" {
		t.Fatalf("a layout 5 record: %+v, %v", loaded, err)
	}
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	if l, mode := read(); l != 5 || !mode {
		t.Errorf("a layout 5 record is now layout %d (mode %v)", l, mode)
	}
}

// Finalize, the other way records are written, goes through the same rule.
func TestFinalizeWritesThePostgresModeInLayout5(t *testing.T) {
	isolate(t)
	for _, c := range []struct {
		mode string
		want int
	}{{"", 4}, {"bundled", 5}} {
		b, err := Prepare()
		if err != nil {
			t.Fatal(err)
		}
		if err := Finalize(FinalizeOptions{Target: "vps", Version: "0.7.5", BundleDir: b, AuthURL: "https://id.example.com", ProxyMode: "nginx", PostgresHost: "127.0.0.1:5432", PostgresMode: c.mode}); err != nil {
			t.Fatal(err)
		}
		s, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if s.SchemaVersion != c.want || s.PostgresMode != c.mode {
			t.Errorf("mode %q: layout %d, mode %q; want layout %d", c.mode, s.SchemaVersion, s.PostgresMode, c.want)
		}
	}
}

// This build reads layout 5, and refuses the next one with the existing
// message, which names what it reads.
func TestLoadReadsLayout5AndRefuses6(t *testing.T) {
	dir := activeDir(t)
	writeState(t, dir, `{"schemaVersion":5,"target":"vps","version":"0.7.5","postgresMode":"bundled","postgresHost":"127.0.0.1:5432"}`)
	if s, err := Load(); err != nil || s.PostgresMode != "bundled" {
		t.Fatalf("layout 5: %+v, %v", s, err)
	}
	writeState(t, dir, `{"schemaVersion":6,"target":"vps","version":"9.9.9"}`)
	_, err := Load()
	if !errors.Is(err, ErrNewerSchema) || !strings.Contains(err.Error(), "state layout 6, this one reads up to 5") {
		t.Fatalf("layout 6: %v", err)
	}
}

// EST is the other layout-5 field: a CLI that did not know it would drop it at
// its next Save and the next rollout would switch EST off.
func TestEstEnabledIsALayout5Field(t *testing.T) {
	dir := activeDir(t)
	s := &State{Target: "vps", Version: "0.8.0", PostgresHost: "db.example.com:5432", EstEnabled: true}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"schemaVersion": 5`) || !strings.Contains(string(b), `"estEnabled": true`) {
		t.Errorf("a record with EST is not layout 5:\n%s", b)
	}
}

// A record written by a build of main between the bundled Postgres and this
// change is layout 4 with postgresMode in it: it loads, keeps the mode, and is
// layout 5 from its next Save on.
func TestLayout4RecordWithPostgresModeMovesToLayout5(t *testing.T) {
	dir := activeDir(t)
	writeState(t, dir, `{"schemaVersion":4,"target":"vps","version":"0.7.5","postgresMode":"bundled","postgresHost":"127.0.0.1:5432"}`)
	s, err := Load()
	if err != nil || s.PostgresMode != "bundled" {
		t.Fatalf("%+v, %v", s, err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if s, err = Load(); err != nil || s.SchemaVersion != 5 || s.PostgresMode != "bundled" {
		t.Errorf("after the next Save: %+v, %v", s, err)
	}
}

// Every field of the record has a layout. A field added to State without a
// word on its layout would be written in layout 4 and dropped by every older
// CLI that saves the record, which is what layout 5 exists to prevent: this
// fails until the new key is put in one of the two lists, and layoutOf says
// which fields are layout 5.
func TestEveryRecordFieldHasALayout(t *testing.T) {
	layout4 := map[string]bool{}
	for _, k := range strings.Fields(`schemaVersion target version configuredAt migratedAt bundleDir authUrl centralUrl mtlsDomain
		centralMtlsDomain centralDomain certLifetimeDays imageRegistry proxyMode postgresHost postgresDb postgresCa previousVersion
		surge rollingBack mountedBundleDirs slots secretsRevision`) {
		layout4[k] = true
	}
	layout5Keys := map[string]bool{"postgresMode": true, "estEnabled": true}

	typ := reflect.TypeOf(State{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if !layout4[tag] && !layout5Keys[tag] {
			t.Errorf("State.%s (json %q) has no layout: add it to layoutOf if it is a layout-5 field, and to this test's lists", typ.Field(i).Name, tag)
		}
		delete(layout4, tag)
		delete(layout5Keys, tag)
	}
	for k := range layout4 {
		t.Errorf("the list names %q, which State no longer has", k)
	}
	for k := range layout5Keys {
		t.Errorf("the list names %q, which State no longer has", k)
	}
	// And layoutOf answers layout 5 for exactly the layout-5 fields.
	for name, s := range map[string]*State{"mode": {PostgresMode: "bundled"}, "est": {EstEnabled: true}} {
		if layoutOf(s) != layout5 {
			t.Errorf("%s: layout %d, want 5", name, layoutOf(s))
		}
	}
	if layoutOf(&State{Target: "vps", PostgresHost: "db:5432"}) != BaseSchemaVersion {
		t.Errorf("a record with no layout-5 field is not layout %d", BaseSchemaVersion)
	}
	if layout5 != SchemaVersion {
		t.Errorf("the newest layout this build reads (%d) is not the one layoutOf writes for its fields (%d)", SchemaVersion, layout5)
	}
}
