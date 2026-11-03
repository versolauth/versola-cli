package deploy

import (
	"fmt"
	"io"

	"github.com/versolauth/versola-cli/internal/manifest"
	"github.com/versolauth/versola-cli/internal/state"
)

// compatCheck is one question to the release's compatibility manifest: may
// the deployment that runs from be moved to to?
type compatCheck struct {
	// dir is the versola-tools output of version to, where release.json is.
	dir      string
	from, to string
	// rollout: a rolling upgrade (the two versions run side by side on the
	// new schema, and an abort runs the old one there alone). Otherwise a
	// plain `configure`, which stops the old version at `up`.
	rollout bool
	// assume is --assume-compatible: the operator read the release notes.
	// Only a rollout of a release that declares nothing needs it.
	assume bool
	// allowFresh is --allow-fresh-database: the operator has already emptied
	// the database. A plain configure only: a rollout migrates the
	// live database while the old version serves from it, so it can never go
	// through a declared break.
	allowFresh bool
}

// requireCompatibility is the gate between the old version and the new one,
// asked after versola-tools of the new version has run (that is where its
// manifest comes from) and before anything is written: no secret, no record,
// no container has been touched yet, so a refusal costs the operator a
// versola-tools pull and nothing else.
//
//   - declared rolling: no confirmation is needed; one line says why;
//   - declared fresh database: refused, `--assume-compatible` or not -- it is
//     the release's own statement that the old database does not work with it;
//   - declared nothing (or no manifest at all, an older release): a rollout
//     still needs --assume-compatible, as it always did; a plain configure
//     goes on, as it always did. An absent manifest is never read as
//     "compatible".
//
// Progress is reported to w, secrets never are (the manifest holds none).
func requireCompatibility(c compatCheck, w io.Writer) error {
	m, err := manifest.Load(c.dir)
	if err != nil {
		return fmt.Errorf("the compatibility manifest of %s can't be used: %w", c.to, err)
	}
	// The image names its own version. A manifest for another one (an image
	// built wrong, a release.json copied from the neighbouring release) would give
	// the verdict of that release, and could waive --assume-compatible for this one.
	if m != nil && m.Version != "" && m.Version != c.to {
		return fmt.Errorf("the compatibility manifest in the versola-tools image of %s is for %s (release.json): a wrongly built image. Nothing was changed", c.to, m.Version)
	}
	notes := ""
	if m != nil {
		notes = m.Notes
	}
	switch m.Check(c.from) {
	case manifest.Rolling:
		if c.rollout {
			fmt.Fprintf(w, "%s -> %s: rolling upgrade supported by the release", c.from, c.to)
			if notes != "" {
				fmt.Fprintf(w, " (release notes: %s)", notes)
			}
			fmt.Fprintln(w)
		}
		return nil
	case manifest.FreshDatabase:
		if c.allowFresh && !c.rollout {
			fmt.Fprintf(w, "Warning: %s -> %s needs a fresh database, and --allow-fresh-database says the database is empty: `versola migrate` fails on an old database%s.\n", c.from, c.to, notesSuffix(notes))
			return nil
		}
		return freshDatabaseError(c, notes)
	}
	if !c.rollout || c.assume {
		return nil
	}
	why := fmt.Sprintf("%s does not state compatibility with %s", c.to, c.from)
	if m == nil {
		why = fmt.Sprintf("%s publishes no compatibility manifest", c.to)
	}
	return fmt.Errorf("%s, and Versola releases before 1.0 don't promise a rolling upgrade works (a rollout runs %s and %s side by side on the database as %s migrates it): "+
		"read the release notes of %s; if its migrations are additive and %s runs on its schema, run again with --assume-compatible. Otherwise deploy it with `versola configure`, `migrate` and `up` (a short stop; back the database up first). Nothing was changed",
		why, c.from, c.to, c.to, c.to, c.from)
}

func freshDatabaseError(c compatCheck, notes string) error {
	if c.rollout {
		return fmt.Errorf("%s -> %s needs a fresh database: a rolling upgrade is not possible%s. Nothing was changed. Back the database up, then deploy %s on a new one with `versola configure vps %s --allow-fresh-database`, `migrate` and `up`",
			c.from, c.to, notesSuffix(notes), c.to, c.to)
	}
	return fmt.Errorf("%s -> %s needs a fresh database: this deployment's database would fail `versola migrate`%s. Nothing was changed. Back the database up, then empty it (drop and recreate the database: restoring the old dump brings the old schema back), then run `versola configure vps %s --allow-fresh-database`",
		c.from, c.to, notesSuffix(notes), c.to)
}

func notesSuffix(notes string) string {
	if notes == "" {
		return ""
	}
	return " (release notes: " + notes + ")"
}

// compatibilityCheck is the question configure asks of the manifest in dir:
// there is one when a recorded vps deployment of another version is being
// replaced, by a plain configure or by a rollout's prepare step. Not for an
// abort, for a first install, for the same version again, or for a record that
// is a rollout under way (its preparation is done and was checked then).
func compatibilityCheck(dir, version string, stored *state.State, staged *stagedConfigure, pg PostgresOptions) (compatCheck, bool) {
	if stored == nil || stored.Target != "vps" || stored.Version == "" || stored.Version == version || stored.RolloutUnderway() {
		return compatCheck{}, false
	}
	c := compatCheck{dir: dir, from: stored.Version, to: version, allowFresh: pg.AllowFreshDatabase}
	if staged != nil {
		if staged.abort {
			return compatCheck{}, false
		}
		c.rollout, c.assume = true, staged.assumeCompatible
	}
	return c, true
}
