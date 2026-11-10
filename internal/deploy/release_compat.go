package deploy

import (
	"fmt"

	"github.com/versolauth/versola-cli/internal/manifest"
)

// checkFreshDatabase is the part of `configure` that reads the release
// manifest in dir (versola-tools' output for version, just written): version
// cannot be upgraded in place from running when the release says so
// (freshDatabaseFrom), and then configure stops, since `migrate` would only
// fail on it later with a Flyway checksum mismatch and leave the operator to
// find out why. bundled is whether the deployment runs the bundled Postgres
// (it only changes the way out the refusal names). allow is --allow-fresh-database, for someone who has already
// restored or dropped the database: the check is then a note and configure
// goes on. Anything else the manifest says, or does not, is not configure's
// business: a rolling-or-unknown verdict only matters to `rollout`.
//
// A manifest this CLI cannot read (an unknown schemaVersion, broken JSON) is an
// error here too, since it may declare exactly such a break.
func checkFreshDatabase(dir, running, version string, bundled, allow bool) error {
	m, err := manifest.Load(dir)
	if err != nil {
		return fmt.Errorf("versola-tools %s: %w", version, err)
	}
	res := m.Check(running, version)
	if res.Verdict != manifest.FreshDatabase {
		return nil
	}
	// Never on the bundled Postgres, whatever the caller passes (the command
	// refuses the flag there earlier, RefuseFreshDatabaseFlag): no other
	// database can be put behind it, so the refusal below, which does not
	// offer the flag, is the answer.
	if allow && !bundled {
		fmt.Printf("Versola %s says it cannot be upgraded in place from %s: going on because of --allow-fresh-database -- the database behind it has to be a new, empty one.\n", version, running)
		return nil
	}
	return freshDatabaseError(res, false, bundled)
}
