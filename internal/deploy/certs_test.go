package deploy

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeCerts struct {
	output     string
	initErr    error
	restartErr map[string]error
	readyErr   map[string]error
	calls      []string
}

func (f *fakeCerts) runInit() ([]byte, error) {
	f.calls = append(f.calls, "init")
	return []byte(f.output), f.initErr
}
func (f *fakeCerts) restart(s string) error {
	f.calls = append(f.calls, "restart "+s)
	return f.restartErr[s]
}
func (f *fakeCerts) waitReady(s string) error {
	f.calls = append(f.calls, "ready "+s)
	return f.readyErr[s]
}

var certSteps = []certsStep{{"central"}, {"auth"}, {"auth-2"}, {"edge"}}

const changedOutput = "mtls-init: server is due for renewal\nmtls-init: issued server\nmtls-init: valid\nmtls-init: changed\n"

func TestRenewCertsNothingDueIsSilent(t *testing.T) {
	f := &fakeCerts{output: "mtls-init: server is valid, leaving it\nmtls-init: central-server is valid, leaving it\n"}
	var out bytes.Buffer
	if err := renewCerts(f, certSteps, false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing (it runs from cron)", out.String())
	}
	if !reflect.DeepEqual(f.calls, []string{"init"}) {
		t.Errorf("calls = %v, want only mtls-init", f.calls)
	}
}

func TestRenewCertsRestartsInOrderWaitingForEach(t *testing.T) {
	f := &fakeCerts{output: changedOutput}
	var out bytes.Buffer
	if err := renewCerts(f, certSteps, false, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"init",
		"restart central", "ready central",
		"restart auth", "ready auth",
		"restart auth-2", "ready auth-2",
		"restart edge", "ready edge"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %v\nwant    %v", f.calls, want)
	}
	if !strings.Contains(out.String(), "Renewed server") {
		t.Errorf("doesn't say what was renewed: %q", out.String())
	}
}

// `changed` has to be a line of its own, not a word in someone's message.
func TestParseMTLSInit(t *testing.T) {
	if _, changed := parseMTLSInit([]byte("mtls-init: unchanged? no, mtls-init: changed later\n")); changed {
		t.Error("a line merely containing the marker counted")
	}
	issued, changed := parseMTLSInit([]byte("  mtls-init: issued central-server \nmtls-init: issued edge-client\nmtls-init: changed\r\n"))
	if !changed || !reflect.DeepEqual(issued, []string{"central-server", "edge-client"}) {
		t.Errorf("= %v, %v", issued, changed)
	}
}

func TestRenewCertsRestartFlagRestartsWithoutRenewal(t *testing.T) {
	f := &fakeCerts{output: "mtls-init: server is valid, leaving it\n"}
	if err := renewCerts(f, certSteps[:1], true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls, []string{"init", "restart central", "ready central"}) {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestRenewCertsInitFailureStopsWithItsOutput(t *testing.T) {
	f := &fakeCerts{output: "step: no such file\n", initErr: errors.New("exit status 1")}
	err := renewCerts(f, certSteps, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "step: no such file") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("restarted after a failed renewal: %v", f.calls)
	}
}

// A failed restart stops the rest -- central first, so auth never restarts
// against a central that is down -- and says what still runs on the old
// certificates, and how to finish: another `renew` finds nothing due.
func TestRenewCertsRestartFailureNamesWhatIsLeft(t *testing.T) {
	f := &fakeCerts{output: changedOutput, restartErr: map[string]error{"auth": errors.New("boom")}}
	err := renewCerts(f, certSteps, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("no error")
	}
	for _, w := range []string{"couldn't restart auth", "auth, auth-2, edge still run on the old ones", "certs renew --restart"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q: %v", w, err)
		}
	}
	if got := f.calls[len(f.calls)-1]; got != "restart auth" {
		t.Errorf("went on after the failure: %v", f.calls)
	}
}

func TestRenewCertsNotReadyAfterRestart(t *testing.T) {
	f := &fakeCerts{output: changedOutput, readyErr: map[string]error{"central": errors.New("timeout")}}
	err := renewCerts(f, certSteps, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "central never became ready") || !strings.Contains(err.Error(), "auth, auth-2, edge still run") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if c == "restart auth" {
			t.Error("auth restarted against a central that is not ready")
		}
	}
	// The last service failing leaves nothing but its own recovery to say.
	f = &fakeCerts{output: changedOutput, readyErr: map[string]error{"edge": errors.New("timeout")}}
	err = renewCerts(f, certSteps, false, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "still run") || !strings.Contains(err.Error(), "certs renew --restart") {
		t.Errorf("err = %v", err)
	}
}

func TestCertsUnits(t *testing.T) {
	service, timer, err := CertsUnits("/opt/my tools/versola", "/usr/local/bin:/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Type=oneshot", `Environment="PATH=/usr/local/bin:/usr/bin"`, `ExecStart="/opt/my tools/versola" certs renew`} {
		if !strings.Contains(service, w) {
			t.Errorf("service lacks %q:\n%s", w, service)
		}
	}
	for _, w := range []string{"OnCalendar=daily", "Persistent=true", "Unit=" + CertsServiceUnit, "WantedBy=timers.target"} {
		if !strings.Contains(timer, w) {
			t.Errorf("timer lacks %q:\n%s", w, timer)
		}
	}
	if s, _, _ := CertsUnits("/bin/100%", "/bin"); !strings.Contains(s, "ExecStart=/bin/100%% certs renew") {
		t.Errorf("a %% is a systemd specifier:\n%s", s)
	}
	if _, _, err := CertsUnits("/bin/v\nExecStart=/bin/evil", "/bin"); err == nil {
		t.Error("a newline in the binary path went into a unit")
	}
}

func TestCertsCronLine(t *testing.T) {
	if got := CertsCronLine("/usr/local/bin/versola"); got != "17 3 * * * /usr/local/bin/versola certs renew" {
		t.Error(got)
	}
	if got := CertsCronLine("/opt/my tools/100%/versola"); got != `17 3 * * * '/opt/my tools/100\%/versola' certs renew` {
		t.Error(got)
	}
}
