package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/versolauth/versola-cli/internal/fsutil"
)

const versolaConfFile = "proxy/conf.d/versola.conf"

var (
	// An upstream block holds no nested braces, so it ends at the first `}`
	// whatever its indentation or a trailing comment: the match can never
	// run on into the map/server blocks that follow.
	legacyUpstreamBlock = regexp.MustCompile(`(?m)^[ \t]*upstream[ \t]+(?:auth_backend|edge_backend)[ \t]*\{[^{}]*\}[ \t]*(?:#[^\r\n]*)?\r?\n`)
	// Exactly the line local's old template wrote: a TLS resolver (vps, the
	// host's own list) is a different directive and stays.
	legacyResolverLine = regexp.MustCompile(`(?m)^[ \t]*resolver[ \t]+127\.0\.0\.11[ \t]+valid=10s[ \t]+ipv6=off;[ \t]*\r?\n`)
	anyUpstreamBlock   = regexp.MustCompile(`(?m)^\s*upstream\s+\w+\s*\{`)
)

// MigrateLegacyUpstreams moves a bundle written by a versola-cli from before
// UpstreamsFile existed -- upstream blocks inline in versola.conf -- to the
// current layout, in place: the blocks (and local's resolver line, which
// now lives with them) are cut out of versola.conf, and c's upstreams are
// written to their own file. Nothing else in versola.conf changes, so the
// certificate settings and the rest are exactly what the deployment has.
//
// check, when not nil, is run on the result (`nginx -t` in the running
// proxy); if it fails both files are put back as they were. It returns
// whether anything was migrated: a bundle already in the current layout is
// left alone.
//
// nginx.conf is not touched: it is a single-file bind mount, which a
// rewrite does not reach until the proxy is recreated (`versola up`).
func MigrateLegacyUpstreams(bundleDir string, c Config, check func() error) (bool, error) {
	confPath := filepath.Join(bundleDir, versolaConfFile)
	old, err := os.ReadFile(confPath)
	if err != nil {
		return false, fmt.Errorf("couldn't read %s: %w", confPath, err)
	}
	// Already in the current layout: upstreams.conf, and none left inline.
	// (Both present is a migration that was interrupted between its two
	// writes: finished here, upstreams.conf is simply written again.)
	if HasUpstreamsFile(bundleDir) && !anyUpstreamBlock.Match(old) {
		return false, nil
	}
	migrated := legacyUpstreamBlock.ReplaceAll(old, nil)
	if len(migrated) == len(old) {
		return false, errors.New("the reverse proxy's versola.conf has no upstream blocks this versola-cli recognizes -- run `versola configure` again")
	}
	if c.Mode == ModeLocal {
		// Only local's upstreams.conf carries a resolver (the one these
		// lines moved to); in nginx mode the line, if any, is the ACME one.
		migrated = legacyResolverLine.ReplaceAll(migrated, nil)
	}
	if anyUpstreamBlock.Match(migrated) {
		return false, errors.New("the reverse proxy's versola.conf has upstream blocks this versola-cli doesn't recognize -- run `versola configure` again")
	}
	upstreams, err := c.Upstreams()
	if err != nil {
		return false, err
	}

	upPath := filepath.Join(bundleDir, UpstreamsFile)
	// The new file first: until versola.conf loses its own blocks the two
	// would define each upstream twice, so a proxy restarted between the
	// writes must find the old layout intact -- which is why this order is
	// reversed on failure.
	if err := fsutil.WriteFileAtomic(upPath, upstreams, 0o644); err != nil {
		return false, fmt.Errorf("couldn't write %s: %w", upPath, err)
	}
	if err := fsutil.WriteFileAtomic(confPath, migrated, 0o644); err != nil {
		_ = os.Remove(upPath)
		return false, fmt.Errorf("couldn't write %s: %w", confPath, err)
	}
	if check != nil {
		if err := check(); err != nil {
			if rerr := fsutil.WriteFileAtomic(confPath, old, 0o644); rerr != nil {
				return false, fmt.Errorf("nginx rejected the migrated config: %w (and putting %s back failed: %v -- run `versola configure` again)", err, confPath, rerr)
			}
			_ = os.Remove(upPath)
			return false, fmt.Errorf("nginx rejected the migrated config (put back as it was): %w", err)
		}
	}
	return true, nil
}

// TestRunning runs `nginx -t` in the running proxy of mode: the check
// MigrateLegacyUpstreams takes when there is a proxy to ask.
func TestRunning(mode string) error {
	if _, err := dockerOutput("exec", ContainerFor(mode), "nginx", "-t"); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return fmt.Errorf("%s", bytes.TrimSpace(ee.Stderr))
		}
		return err
	}
	return nil
}
