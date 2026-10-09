package deploy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPlanDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	dir, cleanup, err := planDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Join(home, ".versola") || !strings.HasPrefix(filepath.Base(dir), "plan-") {
		t.Errorf("unexpected location %s", dir)
	}
	if runtime.GOOS != "windows" { // Windows has no POSIX modes
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("the candidates are real secrets: the directory must be 0700, got %v %v", info, err)
		}
	}
	// Two plans never share a directory.
	other, cleanupOther, err := planDir()
	if err != nil || other == dir {
		t.Fatalf("%s %s %v", dir, other, err)
	}
	defer cleanupOther(&bytes.Buffer{})

	if err := os.WriteFile(filepath.Join(dir, "auth.generated-secrets.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	cleanup(&log)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory with the candidates must be gone: %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("a clean removal says nothing: %q", log.String())
	}
}

func TestPlanSecretsRefusesBadInputBeforeDoingAnything(t *testing.T) {
	var log bytes.Buffer
	if _, err := PlanSecrets(context.Background(), "k8s", "0.6.3", &log); err == nil || !strings.Contains(err.Error(), "unsupported target") {
		t.Errorf("target: %v", err)
	}
	if _, err := PlanSecrets(context.Background(), "vps", "", &log); err == nil {
		t.Error("an empty version must be refused")
	}
	if log.Len() != 0 {
		t.Errorf("nothing should have run: %q", log.String())
	}
}

func TestSweepStalePlanDirs(t *testing.T) {
	base := t.TempDir()
	mk := func(name string, age time.Duration) string {
		path := filepath.Join(base, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old, fresh := mk("plan-old", 3*time.Hour), mk("plan-fresh", time.Minute)
	active, other := mk("active", 5*time.Hour), mk("bundle-1", 5*time.Hour)

	sweepStalePlanDirs(base, time.Hour)

	for path, wantGone := range map[string]bool{old: true, fresh: false, active: false, other: false} {
		_, err := os.Stat(path)
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: gone=%v, want %v", filepath.Base(path), gone, wantGone)
		}
	}
}

// readDotenv's error is shown to the user, and a candidates file holds secret
// values: a line it cannot parse must not end up in the message.
func TestReadDotenvErrorDoesNotEchoTheLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.generated-secrets.env")
	content := "A=1\nTHIS-LINE-HAS-NO-EQUALS-SIGN-S3CR3T\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readDotenv(path)
	if err == nil || strings.Contains(err.Error(), "S3CR3T") || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("%v", err)
	}
	// And it still parses what it parsed before.
	if err := os.WriteFile(path, []byte("A=1\nB=x==\n\nC=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readDotenv(path)
	if err != nil || got["A"] != "1" || got["B"] != "x==" || got["C"] != "" || len(got) != 3 {
		t.Errorf("%v %v", got, err)
	}
}
