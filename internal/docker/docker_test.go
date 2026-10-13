package docker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeDocker puts a `docker` running script first on PATH for this test.
func fakeDocker(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestIsRunningWithinGivesUpOnAHungDocker(t *testing.T) {
	fakeDocker(t, "exec sleep 30")
	start := time.Now()
	if _, err := IsRunningWithin("versola-nginx", 200*time.Millisecond); err == nil {
		t.Fatal("want an error from a docker that never answers")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, want about the 200ms timeout", elapsed)
	}
}

func TestIsRunningWithinAnswers(t *testing.T) {
	for _, c := range []struct {
		name, script string
		want         bool
	}{
		{"running", "echo true", true},
		{"stopped", "echo false", false},
		{"no such container", "echo 'Error: No such object: x' >&2; exit 1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeDocker(t, c.script)
			got, err := IsRunningWithin("x", 5*time.Second)
			if err != nil || got != c.want {
				t.Errorf("got (%v, %v), want (%v, nil)", got, err, c.want)
			}
		})
	}
}

func TestIsNoSuchContainer(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"Error: No such object: versola-openbao-vps\n", true},
		{"Error: No such container: versola-openbao-vps\n", true},
		{"Error response from daemon: no such container: versola-openbao-vps", true},
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", false},
		{"permission denied while trying to connect to the Docker daemon socket", false},
		{"", false},
	} {
		if got := isNoSuchContainer(tc.stderr); got != tc.want {
			t.Errorf("isNoSuchContainer(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}

func TestLeaveWaitsForTheRunningCommandAndStartsNoOther(t *testing.T) {
	done, _ := track()
	if Busy() {
		t.Error("an unshielded command counted")
	}
	done()
	SetShielded(true)
	defer func() {
		SetShielded(false)
		trackMu.Lock()
		leaving = false
		trackMu.Unlock()
	}()
	done, err := track()
	if err != nil || !Busy() {
		t.Fatalf("a shielded command not counted: %v", err)
	}
	idle := Leave()
	select {
	case <-idle:
		t.Fatal("idle while a command still runs")
	default:
	}
	if _, err := track(); !errors.Is(err, ErrLeaving) {
		t.Errorf("a new command after Leave: %v", err)
	}
	done()
	select {
	case <-idle:
	default:
		t.Error("not idle after the last command finished")
	}
}
