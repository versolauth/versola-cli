package deploy

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/exitcode"
)

// fakeBundledPg stands in for Docker: running is whether the postgres
// container exists, calls records what was done to it.
type fakeBundledPg struct {
	running  bool
	startErr error
	stopErr  error
	psErr    error
	ghost    bool // start "succeeds" but no container appears
	calls    []string
	sigs     chan os.Signal
}

func (f *fakeBundledPg) ctl() bundledPgCtl {
	return bundledPgCtl{
		runningID: func(string) (string, error) {
			if f.psErr != nil {
				return "", f.psErr
			}
			if f.running {
				return "abc123", nil
			}
			return "", nil
		},
		start: func(string) error {
			f.calls = append(f.calls, "start")
			if (f.startErr == nil && !f.ghost) || f.startErr == errStartedUnhealthy {
				f.running = true
			}
			if f.startErr == errStartedUnhealthy {
				return errors.New("unhealthy")
			}
			return f.startErr
		},
		stop: func(string) error {
			f.calls = append(f.calls, "stop")
			f.running = false
			return f.stopErr
		},
		signals: func() (<-chan os.Signal, func()) { return f.sigs, func() {} },
	}
}

var errStartedUnhealthy = errors.New("started but unhealthy")

func newFakeBundledPg(running bool) *fakeBundledPg {
	return &fakeBundledPg{running: running, sigs: make(chan os.Signal, 3)}
}

func TestWithBundledPostgresRunningIsLeftAlone(t *testing.T) {
	f := newFakeBundledPg(true)
	var got string
	var out strings.Builder
	err := withBundledPostgres(f.ctl(), "c.yml", &out, func(id string) error { got = id; return nil })
	if err != nil || got != "abc123" {
		t.Fatalf("err=%v container=%q", err, got)
	}
	if len(f.calls) != 0 || out.Len() != 0 {
		t.Fatalf("a running Postgres must be neither started nor stopped: calls=%v out=%q", f.calls, out.String())
	}
}

func TestWithBundledPostgresStartsAndStopsWhenDown(t *testing.T) {
	f := newFakeBundledPg(false)
	var out strings.Builder
	ran := false
	err := withBundledPostgres(f.ctl(), "c.yml", &out, func(id string) error {
		ran = true
		if !f.running || id != "abc123" {
			t.Errorf("the dump must run while Postgres is up: running=%v id=%q", f.running, id)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
	if strings.Join(f.calls, ",") != "start,stop" || f.running {
		t.Fatalf("want start then stop, Postgres down at the end: calls=%v running=%v", f.calls, f.running)
	}
}

func TestWithBundledPostgresStopsAfterFailedDump(t *testing.T) {
	f := newFakeBundledPg(false)
	boom := errors.New("pg_dump failed")
	err := withBundledPostgres(f.ctl(), "c.yml", &strings.Builder{}, func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("the dump's error must come back: %v", err)
	}
	if strings.Join(f.calls, ",") != "start,stop" {
		t.Fatalf("calls=%v", f.calls)
	}
}

func TestWithBundledPostgresStartFailureNamesTheWayOut(t *testing.T) {
	f := newFakeBundledPg(false)
	f.startErr = errStartedUnhealthy
	ran := false
	err := withBundledPostgres(f.ctl(), "c.yml", &strings.Builder{}, func(string) error { ran = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "versola logs postgres") {
		t.Fatalf("the error must name `versola logs postgres`: %v", err)
	}
	if ran {
		t.Fatal("no dump after a failed start")
	}
	// A service that never became healthy is still running: stopped too.
	if strings.Join(f.calls, ",") != "start,stop" || f.running {
		t.Fatalf("calls=%v running=%v", f.calls, f.running)
	}
}

func TestWithBundledPostgresStopFailureIsAWarningNotAFailure(t *testing.T) {
	f := newFakeBundledPg(false)
	f.stopErr = errors.New("daemon gone")
	var out strings.Builder
	if err := withBundledPostgres(f.ctl(), "c.yml", &out, func(string) error { return nil }); err != nil {
		t.Fatalf("the dump succeeded, so the backup must not fail: %v", err)
	}
	if !strings.Contains(out.String(), "versola down") {
		t.Fatalf("the warning must say how to stop it: %q", out.String())
	}
}

func TestWithBundledPostgresInterruptedStillStops(t *testing.T) {
	f := newFakeBundledPg(false)
	err := withBundledPostgres(f.ctl(), "c.yml", &strings.Builder{}, func(string) error {
		f.sigs <- os.Interrupt
		return nil
	})
	if err == nil || exitcode.Code(err) != exitcode.Interrupted {
		t.Fatalf("want the Interrupted exit code, got %v", err)
	}
	if strings.Join(f.calls, ",") != "start,stop" || f.running {
		t.Fatalf("Postgres must be stopped again before the command ends: calls=%v", f.calls)
	}
}

func TestWithBundledPostgresDockerErrorStartsNothing(t *testing.T) {
	f := newFakeBundledPg(false)
	f.psErr = errors.New("daemon unreachable")
	err := withBundledPostgres(f.ctl(), "c.yml", &strings.Builder{}, func(string) error { t.Fatal("no dump"); return nil })
	if err == nil || !strings.Contains(err.Error(), "daemon unreachable") {
		t.Fatalf("the Docker error must come back: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("nothing may be started or stopped when Docker could not be asked: %v", f.calls)
	}
}

func TestWithBundledPostgresStartedButNoContainerStillStops(t *testing.T) {
	f := newFakeBundledPg(false)
	f.ghost = true
	err := withBundledPostgres(f.ctl(), "c.yml", &strings.Builder{}, func(string) error { t.Fatal("no dump"); return nil })
	if err == nil || !strings.Contains(err.Error(), "versola logs postgres") {
		t.Fatalf("err=%v", err)
	}
	if strings.Join(f.calls, ",") != "start,stop" {
		t.Fatalf("calls=%v", f.calls)
	}
}

func TestWithBundledPostgresFailedDumpAndFailedStop(t *testing.T) {
	f := newFakeBundledPg(false)
	f.stopErr = errors.New("daemon gone")
	boom := errors.New("pg_dump failed")
	var out strings.Builder
	err := withBundledPostgres(f.ctl(), "c.yml", &out, func(string) error { return boom })
	if !errors.Is(err, boom) || !strings.Contains(out.String(), "versola down") {
		t.Fatalf("the dump's error must win and the stop warning be printed: err=%v out=%q", err, out.String())
	}
}

// Only the postgres service is ever named: this is what keeps the backup
// from starting the rest of the stack.
func TestBundledPgArgsNameOnlyPostgres(t *testing.T) {
	for _, tc := range []struct {
		got  []string
		want string
	}{
		{bundledPgPsArgs(), "ps -q postgres"},
		{bundledPgStartArgs(), "up -d --wait postgres"},
		{bundledPgStopArgs(), "stop postgres"},
	} {
		if g := strings.Join(tc.got, " "); g != tc.want {
			t.Errorf("got %q, want %q", g, tc.want)
		}
	}
}
