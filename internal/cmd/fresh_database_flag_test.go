package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/exitcode"
)

// --allow-fresh-database over a deployment on the bundled Postgres is refused
// at once: under the lock, after the record is read, before the confirmation
// question and before anything is pulled. Run on an empty home with the
// record written there, so that should the refusal go away the run stops at
// the question (no --yes, no terminal: it is answered by end of input, exit
// code 4) and never reaches Docker. The exit code is what shows the order:
// a usage error (2), not an aborted question (4). The question itself goes to
// stdout, which Run does not capture.
func TestAllowFreshDatabaseRefusedOnBundledBeforeAnyQuestion(t *testing.T) {
	writeVpsState(t, `{"schemaVersion":4,"target":"vps","version":"0.6.2","bundleDir":"bundles/b1",
		"authUrl":"https://id.example.com","proxyMode":"external","postgresMode":"bundled","postgresHost":"127.0.0.1:5432"}`)
	// The flag lives on the package's own command: put it back for the tests after this one.
	f := configureCmd.Flags().Lookup("allow-fresh-database")
	t.Cleanup(func() {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	var stderr bytes.Buffer
	code := Run([]string{"configure", "vps", "0.7.6", "--allow-fresh-database"}, &stderr)
	if code != exitcode.Usage {
		t.Errorf("exit code %d, want %d:\n%s", code, exitcode.Usage, stderr.String())
	}
	for _, want := range []string{"--allow-fresh-database is for a deployment on your own Postgres", "`versola uninstall --purge`", "`versola bootstrap vps 0.7.6 ...`"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
}

// --allow-fresh-database answers a refusal only `configure` on vps can make:
// it is refused on local as a usage error, kept out of --help (an advanced
// flag: the refusal that names it comes with the way out), and `bootstrap`,
// which is for a first deployment, does not have it.
func TestAllowFreshDatabaseFlag(t *testing.T) {
	f := configureCmd.Flags().Lookup("allow-fresh-database")
	if f == nil {
		t.Fatal("configure has no --allow-fresh-database")
	}
	if !f.Hidden {
		t.Error("--allow-fresh-database is in the short --help")
	}
	if bootstrapCmd.Flags().Lookup("allow-fresh-database") != nil {
		t.Error("bootstrap has --allow-fresh-database")
	}
	if err := configureCmd.Flags().Set("allow-fresh-database", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	err := rejectVpsOnlyFlags(configureCmd, "local")
	if err == nil || !strings.HasPrefix(err.Error(), "--allow-fresh-database only applies to vps deployments") {
		t.Fatalf("on local: %v", err)
	}
	if code := exitcode.Code(err); code != exitcode.Usage {
		t.Errorf("exit code %d, want %d", code, exitcode.Usage)
	}
	if err := rejectVpsOnlyFlags(configureCmd, "vps"); err != nil {
		t.Errorf("refused on vps: %v", err)
	}
}
