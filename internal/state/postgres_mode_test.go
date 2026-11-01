package state

import (
	"encoding/json"
	"strings"
	"testing"
)

// PostgresMode is not a layout bump: a record without it loads as before, and
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
