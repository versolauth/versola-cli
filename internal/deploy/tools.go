package deploy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/versolauth/versola-cli/internal/docker"
)

// ToolsImage returns the versola-tools image for a given Versola release.
// This is the one image this CLI names by hand; everything else it runs
// comes out of the compose file that image generates.
func ToolsImage(version string) string {
	return "ghcr.io/versolauth/versola-tools:" + version
}

// manifestUnknownErr wraps a docker failure whose stderr contained
// Docker's "manifest unknown" text -- i.e. the image (or this tag of it)
// was never published, as opposed to some other failure (network, daemon
// down, etc.) that happens to also come back as a non-zero exit.
type manifestUnknownErr struct{ inner error }

func (e *manifestUnknownErr) Error() string { return e.inner.Error() }
func (e *manifestUnknownErr) Unwrap() error { return e.inner }

func isManifestUnknown(err error) bool {
	var m *manifestUnknownErr
	return errors.As(err, &m)
}

// toolsTarget maps this CLI's target names to the TARGET value
// versola-tools' entrypoint.sh expects (see its own comment on TARGET) --
// the two aren't spelled the same because "docker-local" is a fact about
// how the local stack runs (bridge-network Docker containers) that
// predates "local" vs "vps" existing as CLI target names at all, and
// renaming gen-env.scala's branch to match now would just be churn for
// no benefit.
func toolsTarget(target string) string {
	if target == "vps" {
		return "vps"
	}
	return "docker-local"
}

// pullAndRunTools runs the versola-tools image the same way docker.Run
// does (stdout/stderr still streamed live to the user), but also tees
// stderr into a buffer so it can be inspected afterward -- specifically
// to recognize Docker's "manifest unknown" text and turn it into a
// clearer error in Configure, without changing behavior for every other
// docker call site (compose up/down, uninstall's rmi, etc.) that has no
// need for this.
func pullAndRunTools(dir, image, target, authURL string) error {
	// "manifest unknown" (when it happens at all) comes from Docker
	// failing to resolve the image before any pull output follows, so a
	// few KB is more than enough to catch it -- capped rather than a plain
	// bytes.Buffer so a normal, successful pull's progress output (which
	// can run to tens of KB across an image's layers) can't grow this
	// unbounded in memory. os.Stderr (the other leg of the MultiWriter
	// below) still gets every byte, same as a real terminal would.
	stderrBuf := newCappedBuffer(8 * 1024)
	// --platform linux/amd64: versola-tools, like the rest of the stack
	// (see compose.fragment.yml.template's platform pins on central/auth/
	// edge/nginx in the versola repo), is only published for amd64. Without
	// this, Docker has to guess whether to emulate on non-amd64 hosts (e.g.
	// Apple Silicon Macs) -- it doesn't always guess right, and the failure
	// mode when it doesn't is a raw "no matching manifest" error instead of
	// a working, if slower, emulated container. Being explicit here makes
	// configure work the same way on arm64 as on amd64, without requiring
	// DOCKER_DEFAULT_PLATFORM to be set in the environment first.
	//
	// -e TARGET=...: tells entrypoint.sh which of gen-env.scala's
	// non-interactive branches to generate and which compose fragment to
	// emit (see its own comment on TARGET) -- without this it always
	// defaults to docker-local, which was fine back when "local" was the
	// only target this CLI supported at all.
	args := []string{"run", "--rm", "--platform", "linux/amd64", "-e", "TARGET=" + toolsTarget(target)}
	// -e ENV_NAME=...: the literal environment name gen-env.scala writes
	// into the generated configs -- deliberately not derived from target
	// (vps isn't itself an environment: the same VPS could run "prod"
	// today and "qa" tomorrow -- see gen-env.scala's own comment on `env`,
	// and goshacodes' review on versolauth/versola#176). Only vps needs
	// this: docker-local's env is always fixed to "docker-local"
	// regardless. Hardcoded to "prod" here for now, matching the one real
	// VPS this deploys to today -- this is the one place a future
	// `--env` flag on `configure vps` would plug in, once there's a
	// second real target that isn't prod.
	if target == "vps" {
		args = append(args, "-e", "ENV_NAME=prod")
	}
	// -e AUTH_URL=...: the public domain this deployment is reachable at.
	// Unlike ENV_NAME above, this has no sensible hardcoded default here
	// -- it's specific to whoever's deploying (see goshacodes' review on
	// versolauth/versola#176: "this is our domain, users of cli will have
	// other domains"), so it comes from the caller (Configure, which got
	// it from --auth-url) rather than being invented in this package.
	// versola-tools' own entrypoint.sh fails loudly if this is missing
	// for vps -- not re-validated here, Configure already does before
	// this function is ever called.
	if target == "vps" {
		args = append(args, "-e", "AUTH_URL="+authURL)
	}
	args = append(args, "-v", dir+":/out", image)
	c := docker.Cmd(args...)
	c.Stderr = io.MultiWriter(os.Stderr, stderrBuf)

	if err := c.Run(); err != nil {
		if strings.Contains(stderrBuf.String(), "manifest unknown") {
			return &manifestUnknownErr{inner: err}
		}
		return err
	}
	return nil
}

// cappedBuffer is a bytes.Buffer that silently stops accumulating past a
// fixed size. It still reports every byte as written (never a short
// write) so it's safe to use as one leg of an io.MultiWriter -- the other
// leg (os.Stderr here) keeps receiving the full, uncapped output.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		c.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
