package deploy

import (
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

func TestPrerequisiteFailureLocalKeepsPlainDoctor(t *testing.T) {
	err := prerequisiteFailure("local", "", "", PostgresOptions{}, ProxyOptions{}, "")
	if !strings.Contains(err.Error(), "`versola doctor`") || strings.Contains(err.Error(), "--target") {
		t.Errorf("local message = %q", err)
	}
}

func TestPrerequisiteFailureVpsNamesTheVpsTarget(t *testing.T) {
	err := prerequisiteFailure("vps", "https://id.example.com", BundledPostgresHost,
		PostgresOptions{Mode: PostgresModeBundled}, ProxyOptions{Mode: proxy.ModeNginx}, "")
	want := "`versola doctor --target vps --auth-url https://id.example.com`"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("message = %q, want it to contain %s", err, want)
	}
	if strings.Contains(err.Error(), "bootstrap local") || strings.Contains(err.Error(), "--postgres-host") {
		t.Errorf("message = %q", err)
	}
}

func TestDoctorVpsCommandCarriesTheFlagsOfTheRun(t *testing.T) {
	got := doctorVpsCommand("https://id.example.com", "db.internal:5432",
		PostgresOptions{CAFile: "/etc/my ca.pem"},
		ProxyOptions{Mode: proxy.ModeExternal, CentralURL: "https://central.example.com", Public: PublicDomains{MTLS: "mtls.example.com"}},
		"registry.example.com/versola")
	want := "versola doctor --target vps --auth-url https://id.example.com --proxy external" +
		" --postgres-host db.internal:5432 --postgres-ca '/etc/my ca.pem'" +
		" --registry registry.example.com/versola --central-url https://central.example.com --mtls-domain mtls.example.com"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestBundledPostgresPortFailureSaysTheWayOut(t *testing.T) {
	msg := bundledPostgresPortFailure(false).Error()
	for _, want := range []string{"5432", "Stop whatever holds it", "--postgres-host 127.0.0.1:5432"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "versola doctor") {
		t.Errorf("message %q points at doctor", msg)
	}
}

// A deployment recorded as running its own Postgres refuses --postgres-host
// (resolvePostgres), so the message must not advise it.
func TestBundledPostgresPortFailureOfARecordedDeploymentDoesNotAdviseTheFlag(t *testing.T) {
	msg := bundledPostgresPortFailure(true).Error()
	if strings.Contains(msg, "--postgres-host") || !strings.Contains(msg, "Stop whatever holds it") {
		t.Errorf("message = %q", msg)
	}
}

func TestRunsBundledPostgres(t *testing.T) {
	for _, c := range []struct {
		name string
		st   *state.State
		want bool
	}{
		{"no record", nil, false},
		{"bundled vps", &state.State{Target: "vps", PostgresMode: PostgresModeBundled}, true},
		{"external vps", &state.State{Target: "vps", PostgresHost: "db:5432"}, false},
		{"local", &state.State{Target: "local"}, false},
	} {
		if got := runsBundledPostgres(c.st); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
