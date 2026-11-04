// Package manifest reads release.json, the compatibility manifest that the
// versola-tools image of every release writes next to its configs (versola
// issue #823, this CLI's #83). It is a list of entries, one per release,
// newest first; the entry of a release says from which running versions that
// release can be rolled to without stopping (rollingFrom) and from which no
// in-place upgrade exists at all (freshDatabaseFrom). The CLI asks one
// question of it: running version FROM, deploying TO -- what does the release
// TO say?
//
// The answer is never "compatible" by default. A version in neither list, no
// entry for TO, no manifest at all (an older release) and a downgrade are all
// Unknown, and Unknown keeps what the CLI did before the manifest existed: the
// operator confirms with --assume-compatible.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/versolauth/versola-cli/internal/semver"
)

// FileName is the manifest's name in a versola-tools output directory.
const FileName = "release.json"

// supportedSchema is the schemaVersion this CLI understands.
const supportedSchema = 1

// maxSize bounds what is read: the file is a few hundred bytes per release.
const maxSize = 1 << 20

// Release is one entry of the manifest.
type Release struct {
	Version           string   `json:"version"`
	RollingFrom       []string `json:"rollingFrom"`
	FreshDatabaseFrom []string `json:"freshDatabaseFrom"`
	Notes             string   `json:"notes"`
}

// Manifest is the parsed file.
type Manifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Releases      []Release `json:"releases"`
}

// Parse reads a manifest. A schemaVersion other than the one this CLI knows
// is an error that names it: such a file may declare a break this code cannot
// read, so it is not guessed at.
func Parse(data []byte) (*Manifest, error) {
	if len(data) > maxSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", FileName, maxSize)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", FileName, err)
	}
	if m.SchemaVersion != supportedSchema {
		return nil, fmt.Errorf("%s has schemaVersion %d, and this versola understands %d -- update versola (`versola upgrade`)", FileName, m.SchemaVersion, supportedSchema)
	}
	return &m, nil
}

// Load reads the manifest in dir, a versola-tools output directory. A missing
// file is (nil, nil): the release predates the manifest.
func Load(dir string) (*Manifest, error) {
	f, err := os.Open(filepath.Join(dir, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("couldn't read %s: %w", FileName, err)
	}
	defer f.Close()
	// One byte more than the limit is enough to tell that it was exceeded
	// (Parse says so); a file the tools container made huge is not read whole.
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("couldn't read %s: %w", FileName, err)
	}
	return Parse(data)
}

// Verdict is what a release says about an upgrade from one version.
type Verdict string

const (
	// Unknown: the release does not say (see Result.Reason).
	Unknown Verdict = "unknown"
	// Rolling: the release lists the running version in rollingFrom.
	Rolling Verdict = "rolling"
	// FreshDatabase: the release lists it in freshDatabaseFrom.
	FreshDatabase Verdict = "freshDatabase"
)

// Result is the answer of Check.
type Result struct {
	From, To string
	Verdict  Verdict
	// Notes is the release's notes link, only when it is a plain https URL
	// (it is printed to a terminal).
	Notes string
	// Reason says why the verdict is Unknown.
	Reason string
}

// Declared is true for a verdict the release itself gave.
func (r Result) Declared() bool { return r.Verdict == Rolling || r.Verdict == FreshDatabase }

// Check says what m, the manifest of release to, states about an upgrade from
// the running version from. A nil m is a release without a manifest.
//
// Only an upgrade is spoken about: from has to be lower than to. A glob such
// as 0.7.* also matches the versions above the release that lists it, and an
// older release cannot know the schema of a newer one, so a downgrade (or the
// same version) is Unknown whatever the lists say.
func (m *Manifest) Check(from, to string) Result {
	r := Result{From: from, To: to, Verdict: Unknown}
	if m == nil {
		r.Reason = fmt.Sprintf("%s does not publish a compatibility manifest", to)
		return r
	}
	var entry *Release
	for i := range m.Releases {
		if m.Releases[i].Version == to {
			entry = &m.Releases[i]
			break
		}
	}
	if entry == nil {
		r.Reason = fmt.Sprintf("the manifest of %s has no entry for %s", to, to)
		return r
	}
	r.Notes = safeLink(entry.Notes)
	cmp, ok := semver.Compare(from, to)
	if !ok {
		r.Reason = fmt.Sprintf("%q and %q are not both release versions", from, to)
		return r
	}
	if cmp >= 0 {
		r.Reason = fmt.Sprintf("%s is not lower than %s, and a release only speaks about upgrades to it", from, to)
		return r
	}
	switch {
	case matches(entry.FreshDatabaseFrom, from, true):
		// Wins over rollingFrom should a release ever list both.
		r.Verdict = FreshDatabase
	case matches(entry.RollingFrom, from, false):
		r.Verdict = Rolling
	default:
		r.Reason = fmt.Sprintf("the release does not state compatibility with %s", from)
	}
	return r
}

// matches reports whether v is listed: an exact version, or a prefix glob
// ("0.7.*": every version that starts with "0.7."). A glob matches a
// pre-release (0.8.0-rc1) only when prereleaseToo: a break is declared for the
// whole line, while "rolls" is a claim nobody verified for a release candidate
// whose schema may still change, and needs the version spelled out.
func matches(patterns []string, v string, prereleaseToo bool) bool {
	for _, p := range patterns {
		if prefix, glob := strings.CutSuffix(p, "*"); glob {
			if strings.HasSuffix(prefix, ".") && strings.HasPrefix(v, prefix) && (prereleaseToo || !strings.Contains(v, "-")) {
				return true
			}
			continue
		}
		if p == v {
			return true
		}
	}
	return false
}

// safeLink returns s when it is a plain https link that is safe to print.
func safeLink(s string) string {
	if !strings.HasPrefix(s, "https://") || len(s) > 300 {
		return ""
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return ""
		}
	}
	return s
}

// Report is the verdict as the `--json` reports carry it.
type Report struct {
	From          string `json:"from"`
	Rolling       *bool  `json:"rolling"`
	FreshDatabase bool   `json:"freshDatabase"`
	Notes         string `json:"notes,omitempty"`
}

// Report converts r; rolling is null when the release does not say.
func (r Result) Report() Report {
	rep := Report{From: r.From, FreshDatabase: r.Verdict == FreshDatabase, Notes: r.Notes}
	switch r.Verdict {
	case Rolling:
		t := true
		rep.Rolling = &t
	case FreshDatabase:
		f := false
		rep.Rolling = &f
	}
	return rep
}
