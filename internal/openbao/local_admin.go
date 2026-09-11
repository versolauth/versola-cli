package openbao

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LocalAdmin is the root token and unseal key Init returns for the
// "local" target's OpenBao, saved so a LATER `configure local` (after the
// container restarts, which reseals it -- see Unseal's own comment) can
// unseal and finish provisioning without asking anyone anything.
//
// Deliberately local-only: vps's OpenBao is a real production secret
// store, and develop.md's manual one-time setup exists specifically so a
// human deliberately performs that admin action once, not so this CLI can
// silently hold a root token for it. local's OpenBao, by contrast, is a
// throwaway container this same CLI already creates and destroys freely
// (see compose.fragment.yml.template) -- there's no comparable trust
// boundary being crossed by also holding its root token.
type LocalAdmin struct {
	RootToken string `json:"rootToken"`
	UnsealKey string `json:"unsealKey"`
}

// ErrNoLocalAdmin means local's OpenBao has never been auto-provisioned by
// this machine -- either it predates ProvisionLocal (set up by hand,
// following develop.md's old manual steps) or this is a fresh machine.
var ErrNoLocalAdmin = errors.New("no local OpenBao admin credentials stored")

func localAdminPath() (string, error) {
	dir, err := credentialsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local-admin.json"), nil
}

// LoadLocalAdmin reads the saved root token/unseal key for local, if any.
func LoadLocalAdmin() (*LocalAdmin, error) {
	path, err := localAdminPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoLocalAdmin
		}
		return nil, fmt.Errorf("couldn't read local OpenBao admin credentials: %w", err)
	}
	var a LocalAdmin
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("couldn't parse %s: %w", path, err)
	}
	return &a, nil
}

// SaveLocalAdmin stores a's root token/unseal key, creating
// ~/.versola/openbao if needed -- same directory Credentials lives in
// (see credentialsDir), just a different file, since both are "this
// machine's access to OpenBao" in the same sense Credentials' own comment
// describes.
func SaveLocalAdmin(a *LocalAdmin) error {
	dir, err := credentialsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("couldn't create %s: %w", dir, err)
	}

	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode local OpenBao admin credentials: %w", err)
	}
	b = append(b, '\n')

	path, err := localAdminPath()
	if err != nil {
		return err
	}
	// 0o600, same reasoning as Credentials.SecretID's own file: this one
	// holds a root token, more sensitive still.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	return nil
}
