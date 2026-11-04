package deploy

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/state"
)

const testLink = "https://id.example.com/login/central-admin?first_login_token=TOKEN123"

func TestParseFirstLoginLink(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"present", "x\nfirst-login link for admin: " + testLink + "\ny", testLink},
		{"behind a log prefix", "2026-11-04 10:00:00 INFO first-login link for admin: " + testLink + " trailing", testLink},
		{"the first one wins", "first-login link for admin: https://a/x?first_login_token=1\nfirst-login link for admin: https://a/x?first_login_token=2\n", "https://a/x?first_login_token=1"},
		{"a real token alphabet (url-safe base64, 43 characters)", `{"message":"first-login link for admin: https://id.example.com/login/central-admin?first_login_token=Ab-_cD0123456789Ab-_cD0123456789Ab-_cD01234","span_id":"1"}`, "https://id.example.com/login/central-admin?first_login_token=Ab-_cD0123456789Ab-_cD0123456789Ab-_cD01234"},
		// What auth really writes: one JSON object per line, the message quoted
		// and followed by more fields (util VersolaApp.jsonLoggerLayer).
		{"a json log line", `{"timestamp":"2026-11-04T10:00:00Z","system":"auth","level":"INFO","message":"first-login link for admin: ` + testLink + `","span_id":"abc","trace_id":"def","logger":"versola.AuthBootstrapService"}`, testLink},
		{"a json log line with a stack trace after it", `{"message":"first-login link for admin: ` + testLink + `" "stack_trace":"x"}`, testLink},
		{"a json line with an escaped quote after the url", `{"message":"first-login link for admin: ` + testLink + `\"","span_id":"1"}`, testLink},
		{"a sentence ending in a dot", "first-login link for admin: " + testLink + ".", testLink},
		{"angle brackets are not part of a link", "first-login link for admin: <" + testLink + ">", ""},
		{"absent", "Bootstrapping admin user 'admin'...\nSet temporary bootstrap password\n", ""},
		{"empty", "", ""},
		{"nothing after the marker", "first-login link for admin: \n", ""},
		{"not a url", "first-login link for admin: javascript:alert(1)\n", ""},
		{"a malformed line does not hide an earlier good one", "first-login link for admin: " + testLink + "\nfirst-login link for admin: nonsense\n", testLink},
	}
	for _, c := range cases {
		if got := parseFirstLoginLink(c.text); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// fakeAdminOps records the reset in order. logs[i] is what the i-th read of
// auth's log returns; the last one repeats.
type fakeAdminOps struct {
	calls       []string
	recreateErr map[bool]error
	readyErr    []error // per wait, in order; missing is nil
	logs        []string
	logErr      error
	clock       time.Time
	waits       int
	reads       int
}

func (f *fakeAdminOps) recreateAuth(reset bool) error {
	if reset {
		f.calls = append(f.calls, "recreate reset")
	} else {
		f.calls = append(f.calls, "recreate plain")
	}
	return f.recreateErr[reset]
}

func (f *fakeAdminOps) waitReady() error {
	f.calls = append(f.calls, "ready")
	var err error
	if f.waits < len(f.readyErr) {
		err = f.readyErr[f.waits]
	}
	f.waits++
	return err
}

func (f *fakeAdminOps) authLogs(time.Time) (string, error) {
	f.calls = append(f.calls, "logs")
	if f.logErr != nil {
		return "", f.logErr
	}
	i := f.reads
	if i >= len(f.logs) {
		i = len(f.logs) - 1
	}
	f.reads++
	if i < 0 {
		return "", nil
	}
	return f.logs[i], nil
}

func (f *fakeAdminOps) now() time.Time        { return f.clock }
func (f *fakeAdminOps) sleep(d time.Duration) { f.clock = f.clock.Add(d) }

func newFakeAdminOps(logs ...string) *fakeAdminOps {
	return &fakeAdminOps{logs: logs, clock: time.Unix(1000, 0), recreateErr: map[bool]error{}}
}

func TestResetAdminPasswordPrintsTheLinkOnce(t *testing.T) {
	ops := newFakeAdminOps("", "first-login link for admin: "+testLink+"\n")
	var out bytes.Buffer
	if err := resetAdminPassword(ops, &out, "/b/auth.secrets.env"); err != nil {
		t.Fatal(err)
	}
	want := []string{"recreate reset", "ready", "logs", "logs", "recreate plain", "ready"}
	if !slices.Equal(ops.calls, want) {
		t.Fatalf("calls %v, want %v", ops.calls, want)
	}
	if n := strings.Count(out.String(), testLink); n != 1 {
		t.Fatalf("the link is printed %d times, want once:\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "ADMIN_BOOTSTRAP_PASSWORD") {
		t.Fatalf("a link was issued, the password file is not the answer:\n%s", out.String())
	}
}

func TestResetAdminPasswordRefusedWithAPermanentPassword(t *testing.T) {
	ops := newFakeAdminOps("BOOTSTRAP_RESET ignored: admin user 'admin' already has a permanent password\n")
	var out bytes.Buffer
	err := resetAdminPassword(ops, &out, "/b/auth.secrets.env")
	if err == nil || !strings.Contains(err.Error(), "permanent password") {
		t.Fatalf("want a refusal naming the permanent password, got %v", err)
	}
	if ops.calls[len(ops.calls)-2] != "recreate plain" {
		t.Fatalf("auth must be recreated without the variable even when the reset is refused: %v", ops.calls)
	}
	if strings.Contains(out.String(), "first_login_token") {
		t.Fatalf("no link on a refusal:\n%s", out.String())
	}
}

func TestResetAdminPasswordReleaseWithoutLinks(t *testing.T) {
	ops := newFakeAdminOps("Bootstrapping admin user 'admin'...\n")
	var out bytes.Buffer
	if err := resetAdminPassword(ops, &out, "/b/auth.secrets.env"); err != nil {
		t.Fatalf("a release without links is an answer, not a failure: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "/b/auth.secrets.env") || !strings.Contains(s, "logged no first-login link") {
		t.Fatalf("want the path and that no link was logged:\n%s", s)
	}
	// It waited for auth's answer for the whole window, on the fake clock.
	if ops.clock.Sub(time.Unix(1000, 0)) < adminResetLogWait {
		t.Fatalf("gave up after %s, want %s", ops.clock.Sub(time.Unix(1000, 0)), adminResetLogWait)
	}
}

func TestResetAdminPasswordAlwaysRemovesTheVariable(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		mod  func(*fakeAdminOps)
		want string
	}{
		{"recreate with the reset fails", func(f *fakeAdminOps) { f.recreateErr[true] = boom }, "couldn't restart auth"},
		{"auth never ready", func(f *fakeAdminOps) { f.readyErr = []error{boom} }, "never became ready"},
		{"the log cannot be read", func(f *fakeAdminOps) { f.logErr = boom }, "couldn't read auth's log"},
	}
	for _, c := range cases {
		ops := newFakeAdminOps()
		c.mod(ops)
		var out bytes.Buffer
		err := resetAdminPassword(ops, &out, "/b/auth.secrets.env")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to say %q", c.name, err, c.want)
		}
		if !slices.Contains(ops.calls, "recreate plain") {
			t.Errorf("%s: auth was left with BOOTSTRAP_RESET set: %v", c.name, ops.calls)
		}
		if strings.Contains(out.String(), "first_login_token") {
			t.Errorf("%s: printed a link on a failure", c.name)
		}
	}
}

func TestResetAdminPasswordCleanupFailureIsLoud(t *testing.T) {
	ops := newFakeAdminOps("first-login link for admin: " + testLink + "\n")
	ops.recreateErr[false] = errors.New("docker said no")
	var out bytes.Buffer
	err := resetAdminPassword(ops, &out, "/b/auth.secrets.env")
	if err == nil || !strings.Contains(err.Error(), "versola up") || !strings.Contains(err.Error(), "BOOTSTRAP_RESET") {
		t.Fatalf("want the way out and what stays set, got %v", err)
	}
	// Recreated fine but never ready: the variable is gone, and the message must not claim otherwise.
	ops = newFakeAdminOps("first-login link for admin: " + testLink + "\n")
	ops.readyErr = []error{nil, errors.New("timeout")}
	err = resetAdminPassword(ops, &out, "/b/auth.secrets.env")
	if err == nil || strings.Contains(err.Error(), "BOOTSTRAP_RESET") || !strings.Contains(err.Error(), "versola up") {
		t.Fatalf("a slow start after the plain recreate: got %v", err)
	}
	// A refusal plus a failed cleanup reports both.
	ops = newFakeAdminOps("BOOTSTRAP_RESET ignored: admin user 'admin' already has a permanent password\n")
	ops.recreateErr[false] = errors.New("docker said no")
	err = resetAdminPassword(ops, &out, "/b/auth.secrets.env")
	if err == nil || !strings.Contains(err.Error(), "docker said no") || !strings.Contains(err.Error(), "permanent password") {
		t.Fatalf("refusal and cleanup failure: got %v", err)
	}
	if strings.Contains(out.String(), "first_login_token") {
		t.Fatalf("a link printed while auth resets on every start would be misleading:\n%s", out.String())
	}
}

func TestAdminResetRecreateArgs(t *testing.T) {
	dir := t.TempDir()
	compose := dir + "/compose.yml"
	override := dir + "/" + adminResetFile
	with := adminResetRecreateArgs(compose, override, "auth", true)
	without := adminResetRecreateArgs(compose, override, "auth", false)
	if !slices.Contains(with, override) {
		t.Errorf("the reset start must include the override: %v", with)
	}
	if slices.Contains(without, override) {
		t.Errorf("the plain start must not include it: %v", without)
	}
	// The override goes after the files ComposeArgs adds, so it wins.
	if with[len(with)-8] != override {
		t.Errorf("the override must be the last -f: %v", with)
	}
	tail := []string{"up", "-d", "--no-deps", "--force-recreate", "-t", stopTimeoutSeconds, "auth"}
	for _, a := range [][]string{with, without} {
		if !slices.Equal(a[len(a)-len(tail):], tail) {
			t.Errorf("want the recreate of auth last, got %v", a)
		}
	}
	if o := adminResetOverride("auth"); !strings.Contains(o, "BOOTSTRAP_RESET") || strings.Contains(strings.ToLower(o), "password") {
		t.Errorf("the override is one variable and holds no secret:\n%s", o)
	}
}

func TestAdminResetPasswordLocalPrintsTheFixedLogin(t *testing.T) {
	var out bytes.Buffer
	if err := AdminResetPassword(&state.State{Target: "local"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "admin / Admin1234!") {
		t.Fatalf("got:\n%s", out.String())
	}
}

func TestAdminResetPasswordNothingDeployed(t *testing.T) {
	if err := AdminResetPassword(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "nothing has been configured") {
		t.Fatalf("got %v", err)
	}
}
