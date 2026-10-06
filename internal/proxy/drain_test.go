package proxy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitDrained(t *testing.T) {
	const busy = "PID USER COMMAND\n 1 root nginx: master process\n 9 nginx nginx: worker process is shutting down\n"
	const idle = "PID USER COMMAND\n 1 root nginx: master process\n 12 nginx nginx: worker process\n"

	run := func(outs []string, errAt int) (calls int, slept time.Duration) {
		ps := func() (string, error) {
			calls++
			if calls == errAt {
				return "", errors.New("no ps")
			}
			return outs[min(calls-1, len(outs)-1)], nil
		}
		waitDrained(ps, 30*time.Second, 500*time.Millisecond, 30*time.Second, func(d time.Duration) { slept += d })
		return
	}

	if calls, slept := run([]string{idle}, 0); calls != 1 || slept != drainSettle {
		t.Errorf("nothing shutting down: %d calls, slept %v", calls, slept)
	}
	if calls, slept := run([]string{busy, busy, idle}, 0); calls != 3 || slept != drainSettle+time.Second {
		t.Errorf("drains on the third look: %d calls, slept %v", calls, slept)
	}
	// Never drains: stops at the bound, not forever.
	if calls, slept := run([]string{busy}, 0); slept != drainSettle+30*time.Second || calls != 60 {
		t.Errorf("never drains: %d calls, slept %v", calls, slept)
	}
	// ps unavailable: the fixed wait, once.
	if calls, slept := run([]string{idle}, 1); calls != 1 || slept != drainSettle+30*time.Second {
		t.Errorf("no ps: %d calls, slept %v", calls, slept)
	}
}

func TestUpstreamsRenderedAlone(t *testing.T) {
	c := Config{Mode: ModeNginx, Auth: []Backend{{Service: "auth", Port: 8080}, {Service: "auth-2", Port: 8180}}, Edge: []Backend{{Service: "edge", Port: 8095}}}
	b, err := c.Upstreams()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Config{Mode: ModeNginx, AuthURL: mustURL(t, "http://1.2.3.4", ModeNginx), Auth: c.Auth, Edge: c.Edge}.Files()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(all[UpstreamsFile]) {
		t.Errorf("Upstreams differs from what Files writes:\n%s\n---\n%s", b, all[UpstreamsFile])
	}
	if _, err := (Config{Mode: ModeNginx}).Upstreams(); err == nil {
		t.Error("no backends: want an error")
	}
}

func TestHasUpstreamsFile(t *testing.T) {
	dir := t.TempDir()
	if HasUpstreamsFile(dir) {
		t.Error("empty bundle")
	}
	if err := Write(dir, Config{Mode: ModeNginx, AuthURL: mustURL(t, "http://1.2.3.4", ModeNginx), Auth: []Backend{{Service: "auth", Port: 8080}}, Edge: []Backend{{Service: "edge", Port: 8095}}}); err != nil {
		t.Fatal(err)
	}
	if !HasUpstreamsFile(dir) {
		t.Error("a bundle Write made")
	}
}

// Reload: nginx rejecting the new file puts the old one back; a failed
// reload keeps the new one; with no old file a rejected one is removed.
func TestReloadRollback(t *testing.T) {
	cfg := Config{Mode: ModeNginx, Auth: []Backend{{Service: "auth", Port: 8080}, {Service: "auth-2", Port: 8180}}, Edge: []Backend{{Service: "edge", Port: 8095}}}
	oldOut, oldRun := dockerOutput, dockerRun
	defer func() { dockerOutput, dockerRun = oldOut, oldRun }()

	setup := func(t *testing.T, old string) (dir string) {
		dir = t.TempDir()
		if old != "" {
			if err := os.MkdirAll(filepath.Join(dir, "proxy/conf.d"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, UpstreamsFile), []byte(old), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(filepath.Join(dir, "proxy/conf.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	read := func(dir string) string { b, _ := os.ReadFile(filepath.Join(dir, UpstreamsFile)); return string(b) }

	// nginx -t fails: the old file is back, no reload.
	dir := setup(t, "OLD")
	reloaded := false
	dockerOutput = func(args ...string) ([]byte, error) { return nil, &exec.ExitError{Stderr: []byte("bad line 3")} }
	dockerRun = func(args ...string) error { reloaded = true; return nil }
	if err := Reload(dir, cfg); err == nil || !strings.Contains(err.Error(), "bad line 3") {
		t.Errorf("error: %v", err)
	}
	if read(dir) != "OLD" || reloaded {
		t.Errorf("file %q, reloaded %v", read(dir), reloaded)
	}

	// no old file and nginx -t fails: nothing is left behind.
	dir = setup(t, "")
	if err := Reload(dir, cfg); err == nil {
		t.Error("want an error")
	}
	if _, err := os.Stat(filepath.Join(dir, UpstreamsFile)); !os.IsNotExist(err) {
		t.Errorf("a rejected file was left: %v", err)
	}

	// -t passes, reload fails: the new file stays, and the error says so.
	dir = setup(t, "OLD")
	dockerOutput = func(args ...string) ([]byte, error) { return nil, nil }
	dockerRun = func(args ...string) error { return errors.New("exec failed") }
	err := Reload(dir, cfg)
	if err == nil || !strings.Contains(err.Error(), "reload failed") || !strings.Contains(read(dir), "8180") {
		t.Errorf("error %v, file %q", err, read(dir))
	}

	// all good: new file, a reload.
	dir = setup(t, "OLD")
	var ran []string
	dockerRun = func(args ...string) error { ran = args; return nil }
	if err := Reload(dir, cfg); err != nil || !strings.Contains(read(dir), "8180") || strings.Join(ran, " ") != "exec "+ContainerFor(ModeNginx)+" nginx -s reload" {
		t.Errorf("error %v, ran %v", err, ran)
	}
}
