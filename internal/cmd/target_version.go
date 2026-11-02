package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/release"
	"github.com/versolauth/versola-cli/internal/state"
)

// The <version> argument of "bootstrap" and "configure" is optional on local
// (versolauth/versola-cli#66): the version is the one thing a new customer
// cannot know on their first command, and sending them to a releases page to
// copy a number -- which they then type as "v0.7.5" -- is the first hour's
// most avoidable failure. On vps it stays required: a production server runs
// only a version someone named, and the confirmation question already shows
// it.
//
// What "no version" means on local, in order:
//
//   - a local deployment is recorded: keep its version. A redeploy (to pick up
//     a changed flag, or after "down") must not silently become an upgrade
//     because a release came out in between -- that is "rollout"'s job, or an
//     explicit version's. The latest release is shown, best-effort, so moving
//     on is one copy-paste away.
//   - otherwise (nothing deployed, a vps record, a record too old or too new
//     to read): the latest release, as GitHub defines it -- never a
//     pre-release. If GitHub can't be asked, stop and say how to name one; a
//     guess here would deploy something nobody chose.
//
// Either way the resolved version then goes through configure exactly as a
// typed one does (the tools image check included) and is recorded in
// state.json the same way.

// Timeouts for the latest-release lookup. When it decides what gets deployed,
// it may take as long as any other small GitHub request; when it only adds
// "0.8.0 is the latest" to a line, it must not hold up an offline redeploy.
const (
	latestTimeout     = 30 * time.Second
	latestHintTimeout = 5 * time.Second
)

// releaseAPI is GitHub's API base URL; tests point it at an httptest server.
var releaseAPI = release.GitHubAPI

func releaseClient(timeout time.Duration) *release.Client {
	return &release.Client{HTTP: &http.Client{Timeout: timeout}, BaseURL: releaseAPI, UserAgent: userAgent()}
}

// latestVersola returns the latest Versola release's tag.
func latestVersola(timeout time.Duration) (string, error) {
	return releaseClient(timeout).Latest(release.VersolaRepo)
}

// versionChoice is the Versola version a bootstrap/configure run deploys.
type versionChoice struct {
	Version string
	// Latest: nobody typed the version, the CLI took the latest release. If
	// that release has no versola-tools image yet, the error says so (see
	// explainMissingImage) instead of "this version doesn't exist".
	Latest bool
	// Note is the line printed before anything else; "" when the version was
	// typed.
	Note string
}

// resolveVersion works out the version from the arguments (target, then an
// optional version), the deployment this run would replace (nil when there is
// none or it can't be read) and a lookup of the latest release, which it
// calls only when the version was left out. Pure apart from latest, for the
// tests.
func resolveVersion(cmdName string, args []string, prev *state.State, latest func(time.Duration) (string, error)) (versionChoice, error) {
	target := args[0]
	if len(args) > 1 {
		return versionChoice{Version: args[1]}, nil
	}
	switch target {
	case "local":
	case "vps":
		// The deployed version makes the best example: on a server that is
		// already running, "configure vps <it>" is a redeploy, and every
		// flag is remembered.
		example := fmt.Sprintf("versola %s vps <version> --auth-url https://id.example.com", cmdName)
		if prev != nil && prev.Target == "vps" && prev.Version != "" {
			example = fmt.Sprintf("versola %s vps %s", cmdName, prev.Version)
		}
		return versionChoice{}, fmt.Errorf("versola %s vps needs the Versola version to deploy: a server only runs a version you name (only local can leave it out). For example:\n\n  %s\n\n\"versola versions\" lists the releases", cmdName, example)
	default:
		// The same words deploy.Configure would use, rather than asking
		// for a version of a target that does not exist.
		return versionChoice{}, fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported today`, target)
	}

	if prev != nil && prev.Target == "local" && prev.Version != "" {
		note := fmt.Sprintf("Keeping Versola %s (deployed; pass a version to move).", prev.Version)
		if v, err := latest(latestHintTimeout); err == nil {
			if v == prev.Version {
				note = fmt.Sprintf("Keeping Versola %s (deployed, and the latest release).", prev.Version)
			} else {
				note = fmt.Sprintf("Keeping Versola %s (deployed; %s is the latest, pass it to move).", prev.Version, v)
			}
		}
		return versionChoice{Version: prev.Version, Note: note}, nil
	}

	v, err := latest(latestTimeout)
	if err != nil {
		return versionChoice{}, fmt.Errorf("couldn't determine the latest Versola release (%v) -- pass a version explicitly, e.g. `versola %s local <version>`; `versola versions` lists them (or see https://github.com/%s/releases)", err, cmdName, release.VersolaRepo)
	}
	return versionChoice{
		Version: v,
		Latest:  true,
		Note:    fmt.Sprintf("Deploying Versola %s (the latest release; pass a version to pick another).", v),
	}, nil
}

// chooseVersion is resolveVersion against the recorded deployment, printing
// its note to w. Call it with the state lock held, so the deployment it reads
// is the one the run then replaces.
func chooseVersion(w io.Writer, cmdName string, args []string) (versionChoice, error) {
	var prev *state.State
	if len(args) == 1 {
		// Any error (nothing deployed, unreadable, too old, too new) means
		// "no deployed version to keep": the latest release is the answer,
		// and whatever made the record unreadable is still reported by
		// configure itself, as it is with a typed version.
		prev, _ = state.Load()
	}
	c, err := resolveVersion(cmdName, args, prev, latestVersola)
	if err != nil {
		return c, err
	}
	if c.Note != "" {
		fmt.Fprintln(w, c.Note)
	}
	return c, nil
}

// explainMissingImage: when the CLI picked the latest release itself and that
// release has no versola-tools image, "version X doesn't exist" would be
// wrong and alarming -- the release exists, its images are built by a job
// that starts when it is published and takes a while. Say that instead.
func explainMissingImage(err error, c versionChoice, cmdName string) error {
	var missing *deploy.ToolsImageMissingError
	if !c.Latest || !errors.As(err, &missing) {
		return err
	}
	return fmt.Errorf(`the latest Versola release, %s, has no versola-tools image yet: images are published shortly after a release.

Retry in a few minutes, or deploy the release before it: "versola versions"
lists them, then run "versola %s local <version>".`, c.Version, cmdName)
}
