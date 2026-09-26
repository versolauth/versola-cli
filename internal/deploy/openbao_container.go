package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/state"
)

// openbaoConfigPath is where the compose fragments bind-mount openbao.hcl
// from the bundle directory.
const openbaoConfigPath = "/openbao/config/openbao.hcl"

type openbaoAction int

const (
	// openbaoStart: no container yet, start one from this bundle.
	openbaoStart openbaoAction = iota
	// openbaoKeep: running on a config that still exists, leave it alone.
	openbaoKeep
	// openbaoRecreate: remove the container and start it again from this
	// bundle. Its data lives in the external openbao-file volume, so
	// nothing stored in it is lost; it comes back sealed and
	// ProvisionOpenBao unseals it with the saved key.
	openbaoRecreate
	// openbaoKeepStale: running, but on an openbao.hcl that no longer
	// exists, and this machine can't unseal it after a restart. Left
	// alone with a warning -- recreating it would leave it sealed.
	openbaoKeepStale
)

// decideOpenbao picks what Configure does with target's OpenBao container.
//
// Why this exists: OpenBao is started once and then left running across
// configures (see Configure), so its openbao.hcl stays bind-mounted from
// whichever bundle first started it. Later configures prune that bundle,
// and from then on the container can't start again: after a reboot or a
// `docker restart` it stays down, and the next configure used to fail on
// Docker's "container name already in use", because a stopped container
// isn't "running" but still holds its fixed container_name. Nothing else
// breaks while it's down -- services read their secrets from
// *.secrets.env, not from OpenBao -- so the fix only has to be here.
//
// A stopped container is always recreated: it's down anyway, recreating
// can't make that worse. A running one on a deleted config is recreated
// only when it can be unsealed right after (canUnseal); otherwise
// recreating would take a working, unsealed OpenBao and leave it sealed.
func decideOpenbao(exists, running, configMissing, canUnseal bool) openbaoAction {
	switch {
	case !exists:
		return openbaoStart
	case !running:
		return openbaoRecreate
	case !configMissing:
		return openbaoKeep
	case canUnseal:
		return openbaoRecreate
	default:
		return openbaoKeepStale
	}
}

// inspectOpenbao reports whether target's OpenBao container exists, is
// running, and whether the openbao.hcl it bind-mounts is gone from disk
// (see configGone for when that last one can be told at all).
func inspectOpenbao(target string) (exists, running, configMissing bool, err error) {
	format := `{{.State.Running}}|{{range .Mounts}}{{if eq .Destination "` + openbaoConfigPath + `"}}{{.Source}}{{end}}{{end}}`
	out, found, err := docker.Inspect(OpenbaoContainerName(target), format)
	if err != nil || !found {
		return false, false, false, err
	}
	runningStr, source, _ := strings.Cut(out, "|")
	bundles, err := state.Dir()
	if err != nil {
		return false, false, false, err
	}
	return true, runningStr == "true", configGone(source, bundles), nil
}

// configGone reports whether a bind-mount source is a file under
// bundlesDir (~/.versola/active) that no longer exists. A source outside
// bundlesDir is never reported as gone: that's how it looks when Docker
// runs in a VM with its own view of the paths (Docker Desktop on Windows
// reports e.g. /run/desktop/mnt/host/c/...), where this process can't
// stat it and a missing file proves nothing. Native Docker on Linux --
// vps, or local on a Linux machine -- reports the real path, so the check
// works for either target.
func configGone(source, bundlesDir string) bool {
	if source == "" || !filepath.IsAbs(source) {
		return false
	}
	rel, err := filepath.Rel(bundlesDir, source)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	_, err = os.Stat(source)
	return errors.Is(err, os.ErrNotExist)
}

// canUnsealOpenbao reports whether Configure can recreate target's
// running OpenBao and unseal it again right after: ProvisionOpenBao has to
// run (always for local, for vps unless --setup-openbao), there has to be
// a saved admin credentials file with both values in it, and its root
// token has to be accepted by the running instance -- proof the file
// belongs to this OpenBao and not to an older, since-reset one. When it
// can't, reason says why, for the warning Configure prints instead.
func canUnsealOpenbao(target, address string, setupOpenBaoByHand bool) (ok bool, reason string, err error) {
	if target == "vps" && setupOpenBaoByHand {
		return false, "with --setup-openbao this CLI doesn't unseal OpenBao itself", nil
	}
	admin, err := openbao.LoadAdminCreds(target)
	if errors.Is(err, openbao.ErrNoAdminCreds) {
		return false, fmt.Sprintf("there's no saved unseal key for it (~/.versola/openbao/%s-admin.json, fields rootToken and unsealKey)", target), nil
	}
	if err != nil {
		return false, "", fmt.Errorf("couldn't check for saved OpenBao admin credentials: %w", err)
	}
	if admin.RootToken == "" || admin.UnsealKey == "" {
		return false, fmt.Sprintf("~/.versola/openbao/%s-admin.json is missing its rootToken or unsealKey", target), nil
	}
	if err := openbao.CheckToken(context.Background(), address, admin.RootToken); err != nil {
		return false, fmt.Sprintf("the root token in ~/.versola/openbao/%s-admin.json doesn't work against this OpenBao (%v), so its unseal key probably doesn't either", target, err), nil
	}
	return true, "", nil
}
