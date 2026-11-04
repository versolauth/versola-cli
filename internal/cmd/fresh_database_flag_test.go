package cmd

import (
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/exitcode"
)

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
