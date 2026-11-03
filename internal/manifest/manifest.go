// Package manifest reads release.json, the compatibility manifest that a
// versola-tools image writes next to the configs it generates (versola
// docker/versola-tools/release.json, copied to /out by the entrypoint).
//
// A release declares there which earlier versions it can replace while they
// run (rollingFrom) and which need a fresh database (freshDatabaseFrom). The
// CLI reads it from the tools output of the version it is about to deploy,
// i.e. before anything is changed, and decides from it whether a rolling
// upgrade needs the operator's confirmation, and whether a deployment may be
// reconfigured to that version at all.
//
// An absent file is not an error and never means "compatible": it is the
// answer of every release published before the manifest existed, and the
// caller keeps the behaviour it had then (see Check).
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File is the manifest's name in a versola-tools output directory.
const File = "release.json"

// maxSize bounds what Load reads: a real manifest is a few hundred bytes, and the
// file is written by a container whose image the operator merely chose.
const maxSize = 64 << 10

// maxNotes bounds the notes URL that is printed.
const maxNotes = 200

// SchemaVersion is the layout of release.json this package reads. A manifest
// with another one is refused rather than guessed at: a field that changed its
// meaning could turn "needs a fresh database" into "compatible".
const SchemaVersion = 1

// Manifest is a release's declaration. Entries of the two lists are exact
// versions ("0.7.3") or prefix globs ("0.6.*": everything starting "0.6.").
type Manifest struct {
	SchemaVersion     int      `json:"schemaVersion"`
	Version           string   `json:"version"`
	RollingFrom       []string `json:"rollingFrom"`
	FreshDatabaseFrom []string `json:"freshDatabaseFrom"`
	// Notes is the release's notes URL, named in what the CLI says about it.
	Notes string `json:"notes"`
}

// Verdict is what a manifest says about upgrading from one version.
type Verdict int

const (
	// Undeclared: the release says nothing about that version (or has no
	// manifest at all). The caller behaves as it did before manifests.
	Undeclared Verdict = iota
	// Rolling: the release declares it can replace that version while it runs.
	Rolling
	// FreshDatabase: the release declares that moving from that version needs
	// a fresh database.
	FreshDatabase
)

// Load reads the manifest from a versola-tools output directory. nil, nil when
// there is none (an older release). Any other problem -- unreadable, not a
// regular file, too large, not JSON, another schemaVersion -- is an error that
// names the file: a manifest that cannot be understood must not be taken for an
// absent one. Only a regular file is read (a symlink or a FIFO in the output
// directory would be followed or block), and at most maxSize bytes of it. Notes
// that are not a short https URL of printable ASCII are dropped: they are
// printed to the operator, and a manifest from a registry image may hold
// anything, control characters included.
func Load(dir string) (*Manifest, error) {
	path := filepath.Join(dir, File)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", File, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", File)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", File, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", File, err)
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", File, maxSize)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", File, err)
	}
	if m.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s has schemaVersion %d; this versola understands %d -- upgrade versola (`versola upgrade`) to deploy this release", File, m.SchemaVersion, SchemaVersion)
	}
	if !printableURL(m.Notes) {
		m.Notes = ""
	}
	return &m, nil
}

// printableURL: an https URL of printable ASCII (no spaces, no control
// characters), short enough to print on one line.
func printableURL(s string) bool {
	if !strings.HasPrefix(s, "https://") || len(s) > maxNotes {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return false
		}
	}
	return true
}

// Check says what the manifest declares about upgrading from version. A nil
// manifest is Undeclared. A version in both lists is FreshDatabase: the
// refusal is the safe reading of a contradiction (the release's own CI also
// rejects overlapping lists).
func (m *Manifest) Check(from string) Verdict {
	if m == nil {
		return Undeclared
	}
	if matchesAny(m.FreshDatabaseFrom, from) {
		return FreshDatabase
	}
	if matchesAny(m.RollingFrom, from) {
		return Rolling
	}
	return Undeclared
}

func matchesAny(patterns []string, version string) bool {
	for _, p := range patterns {
		if prefix, glob := strings.CutSuffix(p, "*"); glob {
			if strings.HasPrefix(version, prefix) {
				return true
			}
		} else if p == version {
			return true
		}
	}
	return false
}
