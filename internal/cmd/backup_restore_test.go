package cmd

import (
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// The `configure` that `backup restore` prints after it has put OpenBao back
// has to be one that configure accepts: every flag it names exists on the
// command, and no flag it leaves out is one a record holds that configure
// remembers under another name.
func TestRestoreNextStepNamesRealConfigureFlags(t *testing.T) {
	days := 30
	rec := &state.State{AuthURL: "https://id.example.com", ProxyMode: "nginx", CentralURL: "https://c.example.com:8093",
		MTLSDomain: "mtls.example.com", CentralMTLSDomain: "cm.example.com", CentralDomain: "central.example.com",
		CertLifetimeDays: &days, EstEnabled: true, ImageRegistry: "cr.example.com/versola", PostgresDB: "versola"}
	line := deploy.RestoredConfigureCommand("0.7.6", rec, true, true)
	if !strings.HasPrefix(line, "versola configure vps 0.7.6 ") {
		t.Fatalf("not a configure command: %s", line)
	}
	flags := 0
	for _, w := range strings.Fields(line) {
		if !strings.HasPrefix(w, "--") {
			continue
		}
		flags++
		if configureCmd.Flags().Lookup(strings.TrimPrefix(w, "--")) == nil {
			t.Errorf("configure has no %s: %s", w, line)
		}
	}
	if flags != 12 {
		t.Errorf("%d flags in %s", flags, line)
	}
}

func TestBackupRestoreCommand(t *testing.T) {
	for _, name := range []string{"replace-database", "yes"} {
		if backupRestoreCmd.Flags().Lookup(name) == nil {
			t.Errorf("backup restore has no --%s", name)
		}
	}
	if err := backupRestoreCmd.Args(backupRestoreCmd, nil); err == nil {
		t.Errorf("restore without an archive was accepted")
	}
	// A bare name that is not here stays as it is (VerifyBackup then says it is missing).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	if got := resolveArchive("versola-backup-nothing.tar.gz"); got != "versola-backup-nothing.tar.gz" {
		t.Errorf("resolveArchive = %q", got)
	}
}
