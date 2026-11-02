package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

func TestRecordedVpsOf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	st := &state.State{Target: "vps", BundleDir: "bundles/b1", AuthURL: "https://id.example.com", ProxyMode: proxy.ModeNginx,
		PostgresMode: PostgresModeBundled, PostgresHost: BundledPostgresHost}

	// No bundle on disk: staging cannot be seen, so it is off.
	got := recordedVpsOf(st, false)
	want := &RecordedVps{AuthURL: "https://id.example.com", ProxyMode: proxy.ModeNginx, PostgresMode: PostgresModeBundled, PostgresHost: BundledPostgresHost}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("= %+v, want %+v", got, want)
	}

	conf := filepath.Join(home, ".versola", "active", "bundles", "b1", "proxy", "conf.d")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "versola.conf"), []byte(proxy.ACMEStaging), 0o600); err != nil {
		t.Fatal(err)
	}
	got = recordedVpsOf(st, true)
	if !got.ACMEStaging || !got.OpenBaoByHand {
		t.Fatalf("staging bundle, by hand: %+v", got)
	}
}

func TestRecordedVpsPostgresText(t *testing.T) {
	for r, want := range map[RecordedVps]string{
		{PostgresMode: PostgresModeBundled, PostgresHost: BundledPostgresHost}: "in a container of this deployment, on " + BundledPostgresHost,
		{PostgresHost: "db.example.com:5432"}:                                  "your own, at db.example.com:5432",
		{}:                                                                     "not recorded (give --postgres-host)",
	} {
		if got := r.PostgresText(); got != want {
			t.Errorf("%+v: %q, want %q", r, got, want)
		}
	}
}

func TestVpsSettingsLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := VpsSettingsLines(&state.State{Target: "local", AuthURL: "http://localhost:2821"}); got != nil {
		t.Errorf("local: %q", got)
	}
	if got := VpsSettingsLines(nil); got != nil {
		t.Errorf("nil: %q", got)
	}
	// No admin credentials saved: OpenBao set up by hand.
	got := VpsSettingsLines(&state.State{Target: "vps", AuthURL: "https://id.example.com", ProxyMode: proxy.ModeExternal})
	want := []string{"Auth URL: https://id.example.com", "Proxy: external",
		`OpenBao: set up by hand (--setup-openbao; credentials from "versola secrets login vps")`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("= %q, want %q", got, want)
	}
}
