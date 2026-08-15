// Package state locates and reads the on-disk directory describing the
// deployment this machine currently has: the compose file, the configs
// versola-tools produced, and a small record of what was deployed and how
// far along it got.
//
// There's only ever one active deployment at a time — configuring again
// overwrites it. That keeps the commands that read this directory
// (status/down/uninstall) simple: they don't track multiple deployments,
// they read whatever was last written.
//
// The record itself lives in state.json rather than being inferred from
// which files happen to exist. Deploying is no longer one command: the
// configs can exist while the database hasn't been migrated yet, and both
// can be true while nothing is running. "Which of those steps have
// happened" is a real question that only an explicit record can answer.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SchemaVersion is the layout version of state.json as written by this
// build of the CLI. It's recorded in the file so a future CLI reading an
// older deployment's state can tell what it's looking at instead of
// guessing from which fields happen to be present.
//
// Version 0 is reserved for deployments made before state.json existed at
// all, which recorded only a bare "version" file — see Load.
const SchemaVersion = 1

const stateFileName = "state.json"

// legacyVersionFileName is what deployments made by earlier builds of
// this CLI wrote instead of state.json. It still gets read (see Load)
// because `versola upgrade` replaces the binary in place: someone can
// deploy with an old CLI, upgrade, and then run status against state that
// predates this file format. Refusing to read it would turn a working
// deployment into an invisible one.
const legacyVersionFileName = "version"

// ErrNotConfigured means nothing has been deployed on this machine yet —
// no state.json, and no legacy version file either.
var ErrNotConfigured = errors.New("no deployment configured yet")

// State is what the CLI records about the current deployment.
type State struct {
	SchemaVersion int `json:"schemaVersion"`

	// Target is where this deployment is going: "local" today, "vps"
	// later. It's recorded rather than re-asked because every step after
	// the first needs it, and a deployment silently switching target
	// halfway through would be a very confusing failure.
	Target string `json:"target"`

	// Version is the Versola release being deployed, exactly as it's
	// tagged in the registry (no leading "v" — see bootstrap's error
	// message about that).
	Version string `json:"version"`

	ConfiguredAt time.Time `json:"configuredAt"`

	// MigratedAt records when the database migrations for this
	// deployment were last applied, or nil if they haven't been. A
	// pointer rather than a zero time.Time so "not migrated" is
	// unambiguous in the JSON (the field is simply absent) instead of
	// being represented by year 1.
	//
	// Nothing sets this yet: migrations currently run inside central's
	// own startup, so there's no separate step to record. It's here now
	// because `versola migrate` is what this whole split is for, and
	// having the field from the start means the first deployment made by
	// a CLI that does record it isn't a special case.
	MigratedAt *time.Time `json:"migratedAt,omitempty"`

	// BundleDir is the name (not a full path — join it onto Dir()) of the
	// subdirectory holding the compose file and configs versola-tools
	// generated for this deployment. Empty means "Dir() itself", which is
	// how a pre-BundleDir deployment (see loadLegacy) is represented.
	//
	// This exists because of a bug observed in the wild on Docker Desktop
	// for Windows: bind-mounting the *same path* into a container twice —
	// once per `configure` run, since Prepare used to wipe and recreate
	// one fixed directory every time — could make a freshly-started
	// container's own `cp` fail with "File exists" for a file `ls` in that
	// same container shows does not exist. Reproduced with the directory
	// completely removed (not just emptied) between runs, so it isn't
	// leftover files on the Windows side; something in Docker Desktop's
	// file-sharing layer was reusing stale metadata keyed by path. A path
	// that has never been mounted before doesn't have this problem, so
	// Prepare below gives every deployment its own directory name instead
	// of ever reusing one.
	//
	// A new directory each run does NOT mean a new Compose *project* each
	// run, despite Compose's own default of deriving the project name from
	// the compose file's parent directory -- the generated compose file
	// sets `name:` explicitly (see compose.fragment.yml.template) for
	// exactly this reason: every service in it has a fixed container_name,
	// and Docker refuses to create one under a name that already exists
	// under a *different* project. Without a fixed name, a second
	// `configure`/`up` while the previous run's containers were still up
	// would fail on container-name conflicts instead of updating them in
	// place, and once BundleDir moves on, nothing would even know the old
	// containers exist anymore to stop them.
	BundleDir string `json:"bundleDir,omitempty"`
}

// bundlePath resolves where this state's compose file and configs
// actually live, honoring BundleDir when set.
func (s *State) bundlePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if s.BundleDir == "" {
		return dir, nil
	}
	return filepath.Join(dir, s.BundleDir), nil
}

// Dir returns ~/.versola/active, the directory for the current
// deployment.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't find home directory: %w", err)
	}
	return filepath.Join(home, ".versola", "active"), nil
}

// Prepare clears out ~/.versola/active (if it exists from a previous
// deployment), recreates it, and records the deployment about to be
// configured. It returns the bundle directory, freshly created and empty,
// ready for versola-tools to write into.
//
// Clearing the top-level directory first matters: a previous run's
// state.json must not linger if this run fails before writing its own —
// configure should fail loudly instead of a later command reading stale
// state and misreporting what's actually deployed.
//
// The bundle directory itself, in contrast, is deliberately never reused
// across calls — see the comment on State.BundleDir for the Docker
// Desktop bug this sidesteps. Its name only has to be unique on this
// machine, not globally, so a nanosecond timestamp is enough; this CLI
// never runs two configure calls concurrently against the same
// deployment.
func Prepare(target, version string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("couldn't clear %s: %w", dir, err)
	}

	s := &State{
		SchemaVersion: SchemaVersion,
		Target:        target,
		Version:       version,
		ConfiguredAt:  time.Now().UTC(),
		BundleDir:     fmt.Sprintf("bundle-%d", time.Now().UnixNano()),
	}

	bundleDir, err := s.bundlePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return "", fmt.Errorf("couldn't create %s: %w", bundleDir, err)
	}

	if err := s.Save(); err != nil {
		return "", err
	}
	return bundleDir, nil
}

// Save writes the state record, replacing whatever was there.
func (s *State) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	// MarshalIndent, not Marshal: this file is small, is read by people
	// when something has gone wrong, and gets diffed in bug reports.
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode deployment state: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, stateFileName)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	return nil
}

// Load reads the current deployment's state.
//
// It returns ErrNotConfigured if nothing has been deployed yet, so
// callers can tell "nothing here" apart from "something here but
// unreadable" — the second is a real problem worth reporting, the first
// usually just means printing a hint about which command to run first.
// Match it with errors.Is rather than ==, so that a future caller that
// wraps it keeps working.
func Load() (*State, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}

	b, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return loadLegacy(dir)
		}
		return nil, fmt.Errorf("couldn't read deployment state: %w", err)
	}

	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("couldn't parse %s: %w", filepath.Join(dir, stateFileName), err)
	}
	return &s, nil
}

// loadLegacy reads a pre-state.json deployment, which recorded nothing
// but the version string in a file of its own. Anything that file didn't
// record is filled in with what was true of every deployment that could
// have written it: the only target that existed then was "local", and
// there was no separate migration step, so ConfiguredAt/MigratedAt stay
// zero rather than being invented.
func loadLegacy(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, legacyVersionFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotConfigured
		}
		return nil, fmt.Errorf("couldn't read deployment state: %w", err)
	}
	return &State{
		SchemaVersion: 0,
		Target:        "local",
		Version:       strings.TrimSpace(string(b)),
	}, nil
}

// ComposeFile returns the path to the compose file configure generates,
// and whether it currently exists. It existing is how status/down/
// uninstall tell whether anything has been deployed yet.
//
// The path is resolved through the current state record (see
// State.BundleDir) rather than assumed to sit directly in Dir(), since a
// deployment's configs no longer necessarily live at a fixed location.
func ComposeFile() (path string, exists bool, err error) {
	s, err := Load()
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			// Nothing recorded at all -- same as "the file isn't there"
			// from the caller's point of view, not a real error.
			return "", false, nil
		}
		return "", false, err
	}

	bundleDir, err := s.bundlePath()
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(bundleDir, "compose.yml")
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return path, false, nil
		}
		// Something other than "doesn't exist" -- e.g. a permissions
		// error -- shouldn't be silently treated as "nothing deployed";
		// every caller (status, down, uninstall) already checks this
		// error and would otherwise skip real work based on a false
		// negative here.
		return path, false, fmt.Errorf("couldn't check %s: %w", path, statErr)
	}
	return path, true, nil
}
