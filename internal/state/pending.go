package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const pendingFileName = "secrets-pending.json"

// Pending is this machine's note that a first install of a target has begun
// writing secrets to OpenBao and has not finished: `configure` writes it before
// its first write and Finalize removes it once state.json records the
// deployment.
//
// It exists because state.json is written last, after everything else
// (compose file, topology, proxy), so a first install that fails partway
// leaves secrets in OpenBao and no record of a deployment. Without this note
// the next run would read "no record, OpenBao not empty" as an install of
// unknown origin and refuse to generate what the first attempt never got to
// write. With it, the next run knows what that store is: the rest of an
// install that has not started anything yet (configure never starts Versola's
// services), and finishes it.
//
// It is a file of its own, not a field of state.json: state.json is read by
// status, down, uninstall, migrate, up and replica as "the active
// deployment", with a bundle directory and containers. A half-made install
// must not look like one to them. It lives in Dir() (~/.versola/active), so
// `versola uninstall`, which removes that directory, removes it too.
//
// Per machine, like state.json: another machine does not see it.
type Pending struct {
	// Target is the CLI target the install is for ("vps"). A note for another
	// target says nothing about this one.
	Target string `json:"target"`
	// SecretsRevision is the schema revision the attempt settled secrets against.
	SecretsRevision int       `json:"secretsRevision"`
	StartedAt       time.Time `json:"startedAt"`
}

func pendingPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pendingFileName), nil
}

// SavePending records an unfinished first install, replacing a note already
// there. Written to a temp file and renamed, like state.json.
func SavePending(p Pending) error {
	path, err := pendingPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("couldn't create %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode the unfinished-install note: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("couldn't finalize %s: %w", path, err)
	}
	return nil
}

// LoadPending returns the note, or nil if there is none. A note that exists but
// cannot be read is an error and not "none": ignoring it would make an
// interrupted install look like one of unknown origin, or the reverse.
func LoadPending() (*Pending, error) {
	path, err := pendingPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("couldn't read %s: %w", path, err)
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil || p.Target == "" {
		return nil, fmt.Errorf("%s is not a valid note of an unfinished install -- if no install is in progress, delete it", path)
	}
	return &p, nil
}

// ClearPending removes the note. Not having one is fine.
func ClearPending() error {
	path, err := pendingPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("couldn't remove %s: %w", path, err)
	}
	return nil
}
