package deploy

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeCerts records what a renewal does, in order. outputs is what each
// mtls-init phase prints ("" is the default phase).
type fakeCerts struct {
	outputs    map[string]string
	initErr    map[string]error
	restartErr map[string]error
	readyErr   map[string]error
	drainErr   map[string]error
	calls      []string
}

func (f *fakeCerts) runInit(phase string) ([]byte, error) {
	f.calls = append(f.calls, "init "+phase)
	return []byte(f.outputs[phase]), f.initErr[phase]
}
func (f *fakeCerts) restart(s string) error {
	f.calls = append(f.calls, "restart "+s)
	return f.restartErr[s]
}
func (f *fakeCerts) waitReady(s string) error {
	f.calls = append(f.calls, "ready "+s)
	return f.readyErr[s]
}
func (f *fakeCerts) drain(s certsStep) error {
	f.calls = append(f.calls, "drain "+s.Service)
	return f.drainErr[s.Service]
}
func (f *fakeCerts) undrain(s certsStep) error {
	f.calls = append(f.calls, "undrain "+s.Service)
	return nil
}
func (f *fakeCerts) wait(d time.Duration) { f.calls = append(f.calls, "wait "+d.String()) }

func step(base string, slot int) certsStep {
	name := base
	if slot > 1 {
		name = base + "-" + string(rune('0'+slot))
	}
	return certsStep{Service: name, Base: base, Slot: slot}
}

// local: everything restarted in order, nothing drained.
var localPlan = certsPlan{
	Central: []certsStep{step("central", 1)},
	Auth:    []certsStep{step("auth", 1), step("auth", 2)},
	Edge:    []certsStep{step("edge", 1)},
}

// vps: two auth and two edge replicas behind the proxy.
var vpsPlan = certsPlan{
	Central: []certsStep{step("central", 1)},
	Auth:    []certsStep{step("auth", 1), step("auth", 2)},
	Edge:    []certsStep{step("edge", 1), step("edge", 2)},
	Rolling: true,
}

const changedOutput = "mtls-init: server is due for renewal\nmtls-init: issued server\nmtls-init: valid\nmtls-init: changed\n"

func TestRenewCertsNothingDueIsSilent(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{"": "mtls-init: server is valid, leaving it\nmtls-init: central-server is valid, leaving it\n"}}
	var out bytes.Buffer
	if err := renewCerts(f, localPlan, false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing (it runs from cron)", out.String())
	}
	if !reflect.DeepEqual(f.calls, []string{"init "}) {
		t.Errorf("calls = %v, want only mtls-init", f.calls)
	}
	// Rolling: the same, after the two phases that look.
	f = &fakeCerts{}
	out.Reset()
	if err := renewCerts(f, vpsPlan, false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !reflect.DeepEqual(f.calls, []string{"init prepare", "init identity"}) {
		t.Errorf("out %q, calls = %v", out.String(), f.calls)
	}
}

func TestRenewCertsLocalRestartsInOrderWaitingForEach(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{"": changedOutput}}
	var out bytes.Buffer
	if err := renewCerts(f, localPlan, false, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"init ",
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

// A server leaf that is due: the pin is staged, the edges restart (one at a
// time, drained) to learn it, and only then is the leaf replaced, central and
// auth restarted onto it. The edges are never restarted after the commit.
func TestRenewCertsStagesThePinBeforeReplacingTheLeaf(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{
		phasePrepare: "mtls-init: server is due for renewal\nmtls-init: staged server\nmtls-init: changed\n",
		phaseCommit:  "mtls-init: committed server\nmtls-init: changed\n",
	}}
	var out bytes.Buffer
	if err := renewCerts(f, vpsPlan, false, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"init prepare", "init identity",
		"drain edge", "restart edge", "ready edge", "undrain edge",
		"drain edge-2", "restart edge-2", "ready edge-2", "undrain edge-2",
		"init commit",
		"restart central", "ready central",
		"drain auth", "restart auth", "ready auth", "undrain auth",
		"drain auth-2", "restart auth-2", "ready auth-2", "undrain auth-2"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %v\nwant    %v", f.calls, want)
	}
	if !strings.Contains(out.String(), "Staged a new server") {
		t.Errorf("output = %q", out.String())
	}
}

// With a published mutual-TLS name, edges outside this host learn the pin from
// central: it restarts to publish it, and the replacement waits.
func TestRenewCertsWaitsForRemoteEdgesToLearnThePin(t *testing.T) {
	plan := vpsPlan
	plan.PinWait = 15 * time.Minute
	f := &fakeCerts{outputs: map[string]string{
		phasePrepare: "mtls-init: staged central-server\nmtls-init: changed\n",
		phaseCommit:  "mtls-init: committed central-server\n",
	}}
	if err := renewCerts(f, plan, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	i := callIndex(f.calls, "wait 15m0s")
	if i < 0 {
		t.Fatalf("no wait: %v", f.calls)
	}
	before, after := f.calls[:i], f.calls[i+1:]
	if !hasCall(before, "restart central") || !hasCall(before, "restart edge-2") || hasCall(before, "init commit") {
		t.Errorf("before the wait: %v", before)
	}
	if after[0] != "init commit" || !hasCall(after, "restart central") || !hasCall(after, "restart auth-2") {
		t.Errorf("after the wait: %v", after)
	}
}

// A renewal cut off before the commit is finished by running it again: nothing
// was replaced, the leaf is still staged, and the same steps follow.
func TestRenewCertsStageFailureSaysNothingWasReplaced(t *testing.T) {
	f := &fakeCerts{
		outputs:    map[string]string{phasePrepare: "mtls-init: staged server\n"},
		restartErr: map[string]error{"edge-2": errors.New("boom")},
	}
	err := renewCerts(f, vpsPlan, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "couldn't restart edge-2") || !strings.Contains(err.Error(), "run `versola certs renew` again to continue (nothing has been replaced yet)") {
		t.Fatalf("err = %v", err)
	}
	if hasCall(f.calls, "init commit") {
		t.Errorf("committed after a failed edge restart: %v", f.calls)
	}
	// A failing commit is the same.
	f = &fakeCerts{
		outputs: map[string]string{phasePrepare: "mtls-init: staged server\n"},
		initErr: map[string]error{phaseCommit: errors.New("exit status 1")},
	}
	err = renewCerts(f, vpsPlan, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "mtls-init failed") || !strings.Contains(err.Error(), "nothing has been replaced yet") {
		t.Fatalf("err = %v", err)
	}
	if hasCall(f.calls, "restart central") {
		t.Errorf("central restarted without a commit: %v", f.calls)
	}
}

// After the commit a failure leaves old leaves in whoever has not restarted:
// `--restart` is what finishes it.
func TestRenewCertsFailureAfterCommitNamesWhatIsLeft(t *testing.T) {
	f := &fakeCerts{
		outputs:    map[string]string{phasePrepare: "mtls-init: staged server\n"},
		restartErr: map[string]error{"auth": errors.New("boom")},
	}
	err := renewCerts(f, vpsPlan, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("no error")
	}
	for _, w := range []string{"couldn't restart auth", "auth-2 still run on the old ones", "certs renew --restart", "stays out of the proxy"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q: %v", w, err)
		}
	}
}

// Only the edge's identity changed: the edge restarts, nothing else.
func TestRenewCertsIdentityOnlyRestartsEdge(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{phaseIdentity: "mtls-init: issued edge-client\nmtls-init: changed\n"}}
	if err := renewCerts(f, vpsPlan, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "central") || strings.Contains(c, "auth") || c == "init commit" {
			t.Errorf("restarted something besides the edge: %v", f.calls)
		}
	}
	if !hasCall(f.calls, "restart edge") || !hasCall(f.calls, "restart edge-2") {
		t.Errorf("calls = %v", f.calls)
	}
}

// A versola-tools from before the phases replaces the leaf in place on the
// first call: everything restarts, edge last.
func TestRenewCertsOldToolsReplacesInPlace(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{phasePrepare: changedOutput}}
	if err := renewCerts(f, vpsPlan, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if hasCall(f.calls, "init commit") {
		t.Errorf("committed with nothing staged: %v", f.calls)
	}
	if callIndex(f.calls, "restart central") > callIndex(f.calls, "restart auth") || callIndex(f.calls, "restart auth-2") > callIndex(f.calls, "restart edge") {
		t.Errorf("not central, auth, edge: %v", f.calls)
	}
}

// A service with one replica is restarted where it stands: nothing is
// drained, since there would be nowhere to send its requests.
func TestRenewCertsSingleReplicaIsNotDrained(t *testing.T) {
	plan := vpsPlan
	plan.Auth, plan.Edge = plan.Auth[:1], plan.Edge[:1]
	f := &fakeCerts{outputs: map[string]string{phasePrepare: changedOutput}}
	if err := renewCerts(f, plan, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if hasCall(f.calls, "drain auth") || hasCall(f.calls, "drain edge") {
		t.Errorf("drained the only replica: %v", f.calls)
	}
}

func TestRenewCertsDrainFailureStopsBeforeRestart(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{phasePrepare: changedOutput}, drainErr: map[string]error{"auth": errors.New("nginx said no")}}
	err := renewCerts(f, vpsPlan, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "couldn't take auth out of the proxy") || !strings.Contains(err.Error(), "auth, auth-2, edge") && !strings.Contains(err.Error(), "still run on the old ones") {
		t.Fatalf("err = %v", err)
	}
	if hasCall(f.calls, "restart auth") {
		t.Errorf("restarted a replica it could not drain: %v", f.calls)
	}
}

func hasCall(calls []string, c string) bool { return callIndex(calls, c) >= 0 }

func callIndex(calls []string, c string) int {
	for i, x := range calls {
		if x == c {
			return i
		}
	}
	return -1
}

// `changed` has to be a line of its own, not a word in someone's message.
func TestParseMTLSInit(t *testing.T) {
	if r := parseMTLSInit([]byte("mtls-init: unchanged? no, mtls-init: changed later\n")); r.changed {
		t.Error("a line merely containing the marker counted")
	}
	r := parseMTLSInit([]byte("  mtls-init: issued central-server \nmtls-init: issued edge-client\nmtls-init: staged server\nmtls-init: committed server\nmtls-init: changed\r\n"))
	if !r.changed || !reflect.DeepEqual(r.issued, []string{"central-server", "edge-client"}) ||
		!reflect.DeepEqual(r.staged, []string{"server"}) || !reflect.DeepEqual(r.committed, []string{"server"}) {
		t.Errorf("= %+v", r)
	}
}

func TestRenewCertsRestartFlagRestartsWithoutRenewal(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{"": "mtls-init: server is valid, leaving it\n"}}
	plan := localPlan
	plan.Auth, plan.Edge = nil, nil
	if err := renewCerts(f, plan, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls, []string{"init ", "restart central", "ready central"}) {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestRenewCertsInitFailureStopsWithItsOutput(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{"": "step: no such file\n"}, initErr: map[string]error{"": errors.New("exit status 1")}}
	err := renewCerts(f, localPlan, false, &bytes.Buffer{})
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
	f := &fakeCerts{outputs: map[string]string{"": changedOutput}, restartErr: map[string]error{"auth": errors.New("boom")}}
	err := renewCerts(f, localPlan, false, &bytes.Buffer{})
	if err == nil {
		t.Fatal("no error")
	}
	for _, w := range []string{"couldn't restart auth", "auth-2, edge still run on the old ones", "certs renew --restart"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q: %v", w, err)
		}
	}
	if got := f.calls[len(f.calls)-1]; got != "restart auth" {
		t.Errorf("went on after the failure: %v", f.calls)
	}
}

func TestRenewCertsNotReadyAfterRestart(t *testing.T) {
	f := &fakeCerts{outputs: map[string]string{"": changedOutput}, readyErr: map[string]error{"central": errors.New("timeout")}}
	err := renewCerts(f, localPlan, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "central never became ready") || !strings.Contains(err.Error(), "auth, auth-2, edge still run") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if c == "restart auth" {
			t.Error("auth restarted against a central that is not ready")
		}
	}
	// The last service failing leaves nothing but its own recovery to say.
	f = &fakeCerts{outputs: map[string]string{"": changedOutput}, readyErr: map[string]error{"edge": errors.New("timeout")}}
	err = renewCerts(f, localPlan, false, &bytes.Buffer{})
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
