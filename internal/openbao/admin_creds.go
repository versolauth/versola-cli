package openbao

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AdminCreds is the root token and unseal key Init returns for a
// target's OpenBao, saved so a LATER `configure <target>` (after the
// container restarts, which reseals it -- see Unseal's own comment) can
// unseal and finish provisioning without asking anyone anything.
//
// This used to be local-only, on purpose: vps's OpenBao is a real
// production secret store, and develop.md's manual one-time setup
// existed specifically so a human deliberately performed that admin
// action once, not so this CLI silently held a root token for it. The
// team's own call (see deploy.ProvisionOpenBao's doc comment) traded
// that stricter boundary for a much shorter path to a first deployment
// on vps too -- "on a vps, usually a small team just getting started,
// who shouldn't have to learn AppRole administration before they can
// deploy" was the reasoning -- so this now holds credentials for either
// target, the same way Credentials (credentials.go) already did.
type AdminCreds struct {
	RootToken string `json:"rootToken"`
	UnsealKey string `json:"unsealKey"`
}

// ErrNoAdminCreds means target's OpenBao has never been auto-provisioned
// by this machine -- either it predates ProvisionOpenBao (set up by
// hand, following develop.md's old manual steps) or this is a fresh
// machine.
var ErrNoAdminCreds = errors.New("no OpenBao admin credentials stored for this target")

// adminCredsPath is ~/.versola/openbao/<target>-admin.json -- the "-admin"
// suffix keeps it alongside, but distinct from, <target>.json
// (credentialsPath in credentials.go), which holds the AppRole
// credentials this admin token is used to mint, not the admin token
// itself.
func adminCredsPath(target string) (string, error) {
	// Same validTargets check credentialsPath (credentials.go) makes, and
	// for the same reason: target reaches a filesystem path directly here
	// too.
	if !validTargets[target] {
		return "", fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported`, target)
	}
	dir, err := credentialsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, target+"-admin.json"), nil
}

// LoadAdminCreds reads the saved root token/unseal key for target, if
// any.
func LoadAdminCreds(target string) (*AdminCreds, error) {
	path, err := adminCredsPath(target)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoAdminCreds
		}
		return nil, fmt.Errorf("couldn't read OpenBao admin credentials: %w", err)
	}
	var a AdminCreds
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("couldn't parse %s: %w", path, err)
	}
	return &a, nil
}

// SaveAdminCreds stores a's root token/unseal key for target, creating
// ~/.versola/openbao if needed -- same directory Credentials lives in
// (see credentialsDir), just a different file, since both are "this
// machine's access to OpenBao" in the same sense Credentials' own comment
// describes.
func SaveAdminCreds(target string, a *AdminCreds) error {
	// Same shared ~/.versola/openbao directory as Credentials -- see
	// ensureCredentialsDir's own comment for why this isn't a plain
	// MkdirAll.
	if _, err := ensureCredentialsDir(); err != nil {
		return err
	}

	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode OpenBao admin credentials: %w", err)
	}
	b = append(b, '\n')

	path, err := adminCredsPath(target)
	if err != nil {
		return err
	}
	// 0o600, same reasoning as Credentials.SecretID's own file: this one
	// holds a root token, more sensitive still. atomicWriteFile, not a
	// plain os.WriteFile -- see its own comment: a process killed
	// mid-write here would otherwise risk leaving this file empty,
	// destroying the previous root token/unseal key without the new one
	// ever landing either.
	if err := atomicWriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	return nil
}
