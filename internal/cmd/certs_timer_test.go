package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRun answers the commands the timer checks ask, by their joined text.
func fakeRun(t *testing.T, answers map[string]string) {
	t.Helper()
	old, oldUser := runOutput, currentUser
	t.Cleanup(func() { runOutput, currentUser = old, oldUser })
	currentUser = func() string { return "ops" }
	runOutput = func(name string, args ...string) (string, error) {
		if out, ok := answers[name+" "+strings.Join(args, " ")]; ok {
			return out, nil
		}
		return "", errors.New("not found")
	}
}

func withUnitDir(t *testing.T, installed bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if installed {
		units := filepath.Join(dir, "systemd", "user")
		if err := os.MkdirAll(units, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(units, "versola-certs-renew.timer"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCertsTimerWarningIsNeverSilent(t *testing.T) {
	// Nothing installed: said, with what to run.
	withUnitDir(t, false)
	fakeRun(t, nil)
	if w := certsTimerWarning(); !strings.Contains(w, "nothing renews") || !strings.Contains(w, "--install-timer") {
		t.Errorf("no timer: %q", w)
	}
	// The unit exists but is not enabled: still nothing renews.
	withUnitDir(t, true)
	fakeRun(t, map[string]string{"systemctl --user is-enabled versola-certs-renew.timer": "disabled\n"})
	if w := certsTimerWarning(); !strings.Contains(w, "nothing renews") {
		t.Errorf("disabled timer: %q", w)
	}
	// Enabled: quiet (linger cannot be read here, so nothing is claimed about it).
	fakeRun(t, map[string]string{"systemctl --user is-enabled versola-certs-renew.timer": "enabled\n"})
	if w := certsTimerWarning(); w != "" {
		t.Errorf("enabled timer: %q", w)
	}
	// A crontab line counts.
	withUnitDir(t, false)
	fakeRun(t, map[string]string{"crontab -l": "17 3 * * * /usr/local/bin/versola certs renew\n"})
	if w := certsTimerWarning(); w != "" {
		t.Errorf("cron: %q", w)
	}
	// A crontab of something else does not.
	fakeRun(t, map[string]string{"crontab -l": "0 * * * * backup\n"})
	if w := certsTimerWarning(); w == "" {
		t.Error("an unrelated crontab silenced the warning")
	}
}

func TestPrintCertsWarningsOnlyForVps(t *testing.T) {
	withUnitDir(t, false)
	fakeRun(t, nil)
	var out bytes.Buffer
	printCertsWarnings(&out, "local")
	if out.Len() != 0 {
		t.Errorf("local: %q", out.String())
	}
	printCertsWarnings(&out, "vps")
	if !strings.HasPrefix(out.String(), "WARNING: nothing renews") {
		t.Errorf("vps: %q", out.String())
	}
}

func TestScheduleCertsRenewalSkips(t *testing.T) {
	var out bytes.Buffer
	scheduleCertsRenewal(&out, "local", false)
	scheduleCertsRenewal(&out, "vps", true)
	if out.Len() != 0 {
		t.Errorf("printed %q for local / --no-certs-timer", out.String())
	}
}

func TestUpAndBootstrapHaveTheTimerOptOut(t *testing.T) {
	for _, f := range []string{"no-certs-timer"} {
		if upCmd.Flags().Lookup(f) == nil || bootstrapCmd.Flags().Lookup(f) == nil {
			t.Errorf("--%s missing on up or bootstrap", f)
		}
	}
	if upCmd.Flags().Lookup("no-certs-timer").DefValue != "false" {
		t.Error("the timer is not on by default for up")
	}
}
