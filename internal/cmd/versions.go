package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/release"
	"github.com/versolauth/versola-cli/internal/state"
)

var versionsAll bool
var versionsLimit int

// versionsCmd lists Versola releases, so picking a version for "bootstrap",
// "configure" or "rollout" needs no browser (versolauth/versola-cli#66).
var versionsCmd = &cobra.Command{
	Use:     "versions",
	GroupID: groupGetStarted,
	Short:   "List the Versola releases you can deploy",
	Long: `versions lists Versola's releases, newest first, one per line, with
three tab-separated columns: the version (what "bootstrap", "configure"
and "rollout" take), its release date, and markers -- "latest" for the
release "versola bootstrap local" deploys on a first install when no
version is given, "deployed" for the version this machine's deployment
record names (local or vps), "pre-release" for one (shown only with
--all). The markers column may be empty.

  versola versions
  versola versions --all --limit 30
  versola versions | cut -f1

This lists Versola itself, not versola-cli: "versola version" prints the
CLI's own version, "versola upgrade" updates it.`,
	Args: cobra.NoArgs,
	RunE: runVersions,
}

func init() {
	versionsCmd.Flags().BoolVar(&versionsAll, "all", false, "include pre-releases")
	versionsCmd.Flags().IntVar(&versionsLimit, "limit", 10, fmt.Sprintf("how many releases to show (at most %d)", release.MaxList))
}

func runVersions(cmd *cobra.Command, args []string) error {
	return listVersions(os.Stdout, os.Stderr, versionsAll, versionsLimit)
}

func listVersions(stdout, stderr io.Writer, all bool, limit int) error {
	if limit < 1 || limit > release.MaxList {
		return fmt.Errorf("--limit must be between 1 and %d", release.MaxList)
	}
	c := releaseClient(latestTimeout)
	rs, err := c.List(release.VersolaRepo, all, limit)
	if err != nil {
		return fmt.Errorf("couldn't list the Versola releases: %w (they are also at https://github.com/%s/releases)", err, release.VersolaRepo)
	}
	if len(rs) == 0 {
		if all {
			fmt.Fprintln(stderr, "No Versola releases found.")
		} else {
			fmt.Fprintln(stderr, "No Versola releases found (--all includes pre-releases).")
		}
		return nil
	}
	// Both markers are extras: a failed "latest" lookup or an unreadable
	// state.json leaves its marker out rather than failing a listing that
	// is otherwise complete. "latest" is GitHub's own answer, not the first
	// line here -- it is the release bootstrap would deploy, which is what
	// the marker promises.
	latest, _ := c.Latest(release.VersolaRepo)
	deployed := ""
	if st, err := state.Load(); err == nil {
		deployed = st.Version
	}
	writeVersions(stdout, rs, latest, deployed)
	return nil
}

// writeVersions prints rs as "tag<TAB>date<TAB>markers" lines: the tag first,
// so "cut -f1" gives a plain list of versions, and always three columns, so
// "cut -f3" works on every line.
func writeVersions(w io.Writer, rs []release.Release, latest, deployed string) {
	for _, r := range rs {
		var marks []string
		if latest != "" && r.Tag == latest {
			marks = append(marks, "latest")
		}
		if r.Prerelease {
			marks = append(marks, "pre-release")
		}
		if deployed != "" && r.Tag == deployed {
			marks = append(marks, "deployed")
		}
		date := ""
		if !r.Published.IsZero() {
			date = r.Published.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Tag, date, strings.Join(marks, ", "))
	}
}
