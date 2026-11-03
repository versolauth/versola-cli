package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/versolauth/versola-cli/internal/fsutil"
)

// The daily "a newer versola-cli is available" notice (versolauth/versola-cli#77).
//
// It is a notice and nothing more: the binary is only ever replaced by an
// explicit "versola upgrade". A CLI that runs "migrate" against production
// must not swap itself out during an incident, and a newer CLI may write a
// state layout an older one cannot read.
//
// How it stays out of the way:
//   - the lookup starts before the command runs (rootCmd's PersistentPreRun),
//     so a long command such as "bootstrap" has the answer long before it
//     ends, and a short one waits for it only for the rest of a 2-second
//     budget;
//   - it prints only after the command succeeded, to stderr, after the
//     command's own output, and never for --json, never without a terminal on
//     both stdout and stderr (a script's output is not a place for a hint);
//   - a failed lookup is silent, and is remembered like a successful one, so
//     a machine that cannot reach GitHub (a VPS with no outbound access) pays
//     the 2 seconds once a day, not on every command.

const (
	// updateCheckEvery is both how often GitHub is asked and how often the
	// notice may be printed: once a day, not on every command until the
	// customer upgrades (what other CLIs are complained about for).
	updateCheckEvery = 24 * time.Hour
	// noUpdateNotifierEnv turns the notice off when it is set, to any value.
	noUpdateNotifierEnv = "VERSOLA_NO_UPDATE_NOTIFIER"
)

// updateCheckTimeout bounds the lookup, connecting and reading together. A
// variable only so a test of the slow case need not take two seconds.
var updateCheckTimeout = 2 * time.Second

// updateCache is ~/.versola/update-check.json.
type updateCache struct {
	// CheckedAt is when GitHub was last asked, whether or not it answered.
	CheckedAt time.Time `json:"checkedAt"`
	// Latest is the newest release it named; kept across failed lookups.
	Latest string `json:"latest,omitempty"`
	// NotifiedAt is when the notice was last printed.
	NotifiedAt time.Time `json:"notifiedAt,omitzero"`
}

// Seams for tests.
var (
	// outputIsInteractive: a terminal on stdout and on stderr, like gh and
	// flyctl require.
	outputIsInteractive = func() bool {
		return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
	}
	updateCachePath = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".versola", "update-check.json"), nil
	}
	updateNow = time.Now
)

// pendingUpdateCheck is the lookup of the command being run, nil when the
// gate says no. Set by rootCmd's PersistentPreRun, consumed by Run.
var pendingUpdateCheck *updateCheck

// noticeExcluded are the top-level commands that never show the notice:
// "version" and the shell-completion machinery are called by scripts and
// shells, "upgrade" deals with the question itself, and "uninstall --purge"
// removes ~/.versola, which a cache file written afterwards would recreate.
var noticeExcluded = map[string]bool{
	"version":          true,
	"upgrade":          true,
	"uninstall":        true,
	"help":             true,
	"completion":       true,
	"__complete":       true,
	"__completeNoDesc": true,
}

// updateNoticeAllowed is the gate: every condition that makes the notice
// (and the lookup behind it) unwelcome.
func updateNoticeAllowed(cmd *cobra.Command, interactive bool, lookupEnv func(string) (string, bool), current string) bool {
	if !interactive || current == "dev" {
		return false
	}
	if _, set := lookupEnv(noUpdateNotifierEnv); set {
		return false
	}
	if v, _ := lookupEnv("CI"); v != "" {
		return false
	}
	top := cmd
	for top.HasParent() && top.Parent().HasParent() {
		top = top.Parent()
	}
	if noticeExcluded[top.Name()] {
		return false
	}
	if f := cmd.Flags().Lookup("json"); f != nil && f.Value.String() == "true" {
		return false
	}
	return true
}

type updateResult struct {
	latest string
	err    error
}

type updateCheck struct {
	path     string
	cache    updateCache
	result   chan updateResult // nil when the cache was fresh and nothing was asked
	deadline time.Time
}

// startUpdateCheck reads the cache and, when it is older than a day, starts
// asking GitHub in the background. nil when the gate says no.
func startUpdateCheck(cmd *cobra.Command) *updateCheck {
	if !updateNoticeAllowed(cmd, outputIsInteractive(), os.LookupEnv, version) {
		return nil
	}
	path, err := updateCachePath()
	if err != nil {
		return nil
	}
	u := &updateCheck{path: path}
	if b, err := os.ReadFile(path); err == nil {
		// A file that doesn't parse is the same as no file: ask again.
		if json.Unmarshal(b, &u.cache) != nil {
			u.cache = updateCache{}
		}
	}
	age := updateNow().Sub(u.cache.CheckedAt)
	if age >= 0 && age < updateCheckEvery {
		return u
	}
	u.result = make(chan updateResult, 1)
	timeout := updateCheckTimeout // read here: the goroutine below must not touch the variable
	u.deadline = time.Now().Add(timeout)
	go func() {
		tag, err := releaseClient(timeout).Latest(upgradeRepo)
		u.result <- updateResult{latest: tag, err: err}
	}()
	return u
}

// finish prints the notice if there is something to say and it was not said
// today, and saves the cache. Called only after the command succeeded.
// Nothing here can fail the command: every error is dropped.
func (u *updateCheck) finish(w io.Writer) {
	now := updateNow()
	changed := false
	if u.result != nil {
		timer := time.NewTimer(max(0, time.Until(u.deadline)))
		defer timer.Stop()
		select {
		case r := <-u.result:
			// Only a tag that parses is kept: it is printed, and the file
			// is one anybody can edit.
			if r.err == nil {
				if _, _, ok := parseVersion(r.latest); ok {
					u.cache.Latest = r.latest
				}
			}
		case <-timer.C:
		}
		// Asked, whatever the answer: see the package comment on offline machines.
		u.cache.CheckedAt = now
		changed = true
	}
	since := now.Sub(u.cache.NotifiedAt)
	if newerVersion(u.cache.Latest, version) && (since >= updateCheckEvery || since < 0) {
		fmt.Fprintf(w, "A newer versola-cli is available: %s (you have %s). Run versola upgrade.\n", u.cache.Latest, version)
		u.cache.NotifiedAt = now
		changed = true
	}
	if !changed {
		return
	}
	b, err := json.Marshal(u.cache)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o700); err != nil {
		return
	}
	_ = fsutil.WriteFileAtomic(u.path, append(b, '\n'), 0o600)
}

// parseVersion splits a release tag ("0.3.3", "0.4.0-rc1", with an optional
// leading "v") into its numeric core and its pre-release identifiers. ok is
// false for anything else; in particular only [0-9A-Za-z.-] get through, so a
// tag that parsed is safe to print and to put in a URL.
func parseVersion(s string) (core [3]uint64, pre []string, ok bool) {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return core, nil, false
	}
	main, preStr, hasPre := strings.Cut(s, "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return core, nil, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || p == "" || (len(p) > 1 && p[0] == '0') {
			return core, nil, false
		}
		core[i] = n
	}
	if !hasPre {
		return core, nil, true
	}
	pre = strings.Split(preStr, ".")
	for _, id := range pre {
		if id == "" {
			return core, nil, false
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return core, nil, false
			}
		}
	}
	return core, pre, true
}

// compareVersions orders two release tags like semver does: the core
// numerically, a pre-release below the release it precedes, pre-release
// identifiers one by one (numbers below words, fewer below more). ok is false
// when either does not parse.
func compareVersions(a, b string) (cmp int, ok bool) {
	ac, ap, aok := parseVersion(a)
	bc, bp, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range ac {
		if ac[i] != bc[i] {
			if ac[i] < bc[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case len(ap) == 0 && len(bp) == 0:
		return 0, true
	case len(ap) == 0:
		return 1, true
	case len(bp) == 0:
		return -1, true
	}
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if c := comparePreIdentifier(ap[i], bp[i]); c != 0 {
			return c, true
		}
	}
	switch {
	case len(ap) < len(bp):
		return -1, true
	case len(ap) > len(bp):
		return 1, true
	}
	return 0, true
}

func comparePreIdentifier(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		if an < bn {
			return -1
		}
		if an > bn {
			return 1
		}
		return 0
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// newerVersion is true only when latest is strictly newer than current and
// both parse: an unreadable version never produces a notice (a "different
// from" test would offer a downgrade to someone on a newer pre-release).
func newerVersion(latest, current string) bool {
	c, ok := compareVersions(latest, current)
	return ok && c > 0
}
