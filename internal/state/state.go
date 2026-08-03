// Package state locates the on-disk directory that "bootstrap" writes to
// and that "status"/"down" read from: the generated compose file, the
// configs versola-tools produced, and which version is currently active.
//
// There's only ever one active local deployment at a time — running
// bootstrap again overwrites it. This mirrors how bootstrap itself works
// today (docker compose up on a fixed project), and keeps status/down
// simple: they don't need to track multiple deployments, just read
// whatever bootstrap last wrote.
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir returns ~/.versola/active, the directory for the currently
// deployed stack.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't find home directory: %w", err)
	}
	return filepath.Join(home, ".versola", "active"), nil
}

// Prepare clears out ~/.versola/active (if it exists from a previous
// bootstrap) and recreates it, ready for versola-tools to write into.
// Clearing it first matters: a previous run's compose.yml/*.conf files
// must not linger and get silently reused if this run's versola-tools
// container fails partway through — bootstrap should fail loudly instead
// of quietly deploying a stale or half-written config.
func Prepare(version string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("couldn't clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("couldn't create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0o644); err != nil {
		return "", fmt.Errorf("couldn't write version file: %w", err)
	}
	return dir, nil
}

// ComposeFile returns the path to the compose file bootstrap generates,
// and whether it currently exists. It existing is how status/down/uninstall
// tell whether anything has been deployed yet.
func ComposeFile() (path string, exists bool, err error) {
	dir, err := Dir()
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(dir, "compose.yml")
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

// Version returns the version string bootstrap recorded for the active
// deployment, or "" if there isn't one (nothing deployed, or bootstrap
// hasn't been implemented yet).
func Version() string {
	dir, err := Dir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
