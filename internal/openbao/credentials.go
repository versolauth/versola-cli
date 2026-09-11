// Package openbao is a thin client for reading and writing secrets in the
// OpenBao instance a deployment target uses, plus the AppRole credentials
// that authenticate to it.
//
// Only what versola-cli actually needs is implemented here: AppRole login
// and KV v2 get/put against a single, already-enabled secrets engine. This
// is not a general-purpose OpenBao/Vault SDK, and deliberately doesn't
// depend on one — see go.mod's comment-free dependency list, which this
// isn't meant to be the thing that breaks.
package openbao

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Credentials are what this machine uses to authenticate to one target's
// OpenBao server. Kept separate from state.State: state.go's own comment
// says only one deployment is ever "active" at a time, but OpenBao
// credentials for a target need to outlive that — configuring local after
// having configured vps (or back again) must not discard either target's
// access.
type Credentials struct {
	// Address is this target's OpenBao server, e.g. "http://localhost:8200"
	// for local. Stored per-target rather than derived from the target
	// name: local and vps are different OpenBao servers with nothing in
	// common to compute one address from the other.
	Address string `json:"address"`

	// RoleID identifies the AppRole this CLI logs in as. Not secret — safe
	// to store here in plain JSON, same as a username.
	RoleID string `json:"roleId"`

	// SecretID is the AppRole's password. Whoever administers OpenBao
	// hands this out once, out of band; this file is where it lives
	// afterward. Never logged, never included in any error message this
	// package returns.
	SecretID string `json:"secretId"`
}

// ErrNoCredentials means this target has no stored OpenBao credentials
// yet — distinguished from other read failures the same way
// state.ErrNotConfigured is, so callers can print a hint instead of a raw
// file-not-found error.
var ErrNoCredentials = errors.New("no OpenBao credentials stored for this target")

// credentialsDir is ~/.versola/openbao — deliberately not under
// state.Dir() (~/.versola/active), for the reason Credentials' own comment
// gives.
func credentialsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't find home directory: %w", err)
	}
	return filepath.Join(home, ".versola", "openbao"), nil
}

// validTargets are the only target names deploy.Configure supports.
// Enforced here too, not just there: target reaches a filesystem path
// (credentialsPath, below) directly, and `versola secrets login <target>
// ...` is a CLI entry point deploy.Configure's own check never sees.
// Without this, a typo'd or malicious target like "../active/state" would
// let credentialsPath escape ~/.versola/openbao entirely and overwrite an
// unrelated file -- state.json, in that example -- instead of writing a
// credentials file.
var validTargets = map[string]bool{"local": true, "vps": true}

// ensureCredentialsDir creates credentialsDir if needed and makes sure it's
// 0o700, even on a machine that ran an older CLI version which created it
// at 0o755 -- os.MkdirAll is a documented no-op, including on mode, when
// the directory already exists, so a plain MkdirAll alone would never
// tighten an existing installation's permissions, only a fresh one's.
//
// The Chmod is best-effort, not fatal: it only ever narrows permissions on
// a directory whose files are already individually 0o600 (see
// SaveCredentials/SaveLocalAdmin), so failing to also tighten the
// directory itself is a defense-in-depth miss, not a loss of the real
// protection -- and failing loudly here would turn an unusual pre-existing
// ownership on ~/.versola/openbao (e.g. created by a different user or
// process) into a hard failure of every `configure`/`secrets login`,
// which is worse than just not tightening it. Also a near-total no-op on
// Windows: os.Chmod there only toggles the read-only attribute, not an
// owner-only ACL -- fine, since 0o600 on the files themselves is this
// package's real cross-platform protection.
func ensureCredentialsDir() (string, error) {
	dir, err := credentialsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("couldn't create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		fmt.Printf("(couldn't tighten permissions on %s to 0700 -- %v -- continuing anyway, the credential files themselves are still 0600)\n", dir, err)
	}
	return dir, nil
}

func credentialsPath(target string) (string, error) {
	if !validTargets[target] {
		return "", fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported`, target)
	}
	dir, err := credentialsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, target+".json"), nil
}

// LoadCredentials reads the stored AppRole credentials for target.
func LoadCredentials(target string) (*Credentials, error) {
	path, err := credentialsPath(target)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, fmt.Errorf("couldn't read OpenBao credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("couldn't parse %s: %w", path, err)
	}
	return &c, nil
}

// SaveCredentials stores AppRole credentials for target, creating
// ~/.versola/openbao if this is the first target configured.
func SaveCredentials(target string, c *Credentials) error {
	if _, err := ensureCredentialsDir(); err != nil {
		return err
	}

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode OpenBao credentials: %w", err)
	}
	b = append(b, '\n')

	path, err := credentialsPath(target)
	if err != nil {
		return err
	}
	// 0o600, not the 0o644 state.go uses for state.json: this file holds
	// SecretID, which state.json never holds anything equivalent to.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	return nil
}
