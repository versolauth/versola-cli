package deploy

import (
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestDownAction(t *testing.T) {
	bundled := &state.State{Target: "vps", Version: "0.7.2", AuthURL: "https://id.example.com", PostgresMode: PostgresModeBundled}
	own := &state.State{Target: "vps", Version: "0.7.2", AuthURL: "https://id.example.com"}

	got := DownAction(bundled, false)
	for _, want := range []string{"0.7.2", "https://id.example.com", "Postgres", "volumes are kept", "stops answering until `versola up`"} {
		if !strings.Contains(got, want) {
			t.Errorf("bundled, no --volumes: %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "delete") {
		t.Errorf("without --volumes nothing is deleted: %q", got)
	}

	got = DownAction(bundled, true)
	if !strings.Contains(got, "deletes the stack's own volumes") || !strings.Contains(got, "The Postgres, OpenBao and TLS certificate volumes are kept") {
		t.Errorf("--volumes must say what goes and that Postgres stays: %q", got)
	}

	got = DownAction(own, true)
	if strings.Contains(got, "Postgres") {
		t.Errorf("a deployment on its own Postgres has none to name: %q", got)
	}

	rolling := &state.State{Target: "vps", Version: "0.7.2", PreviousVersion: "0.7.1", AuthURL: "https://id.example.com"}
	got = DownAction(rolling, false)
	if !strings.Contains(got, "0.7.1 -> 0.7.2") || !strings.Contains(got, "versola rollout") || strings.Contains(got, "until `versola up`") {
		t.Errorf("mid-rollout text must name both versions and point at rollout, not up: %q", got)
	}
}
