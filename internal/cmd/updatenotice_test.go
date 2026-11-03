package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.3", "0.3.2", 1},
		{"0.3.2", "0.3.3", -1},
		{"0.3.2", "0.3.2", 0},
		{"0.10.0", "0.9.9", 1}, // numbers, not strings
		{"1.0.0", "0.99.99", 1},
		{"0.4.0", "0.4.0-rc1", 1}, // a release is above its pre-release
		{"0.4.0-rc1", "0.4.0", -1},
		{"0.4.0-rc2", "0.4.0-rc1", 1},
		{"0.4.0-rc.10", "0.4.0-rc.9", 1}, // numeric identifiers compare as numbers
		{"0.4.0-rc", "0.4.0-rc.1", -1},   // fewer identifiers sort first
		{"0.4.0-1", "0.4.0-rc", -1},      // numbers below words
		{"0.4.0-rc1", "0.3.9", 1},        // a pre-release of a higher core is still higher
		{"v0.3.3", "0.3.3", 0},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, %v; want %d", c.a, c.b, got, ok, c.want)
		}
	}
}

func TestParseVersionRejects(t *testing.T) {
	for _, s := range []string{
		"", "dev", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "-1.2.3", "+1.2.3",
		"1.2.3-", "1.2.3-rc.", "1.2.3-a_b", "1.2.3-rc 1", "1.2.3/../x", "1.2.3\x1b[2J", "1.2.3-é",
	} {
		if _, _, ok := parseVersion(s); ok {
			t.Errorf("parseVersion(%q) accepted it", s)
		}
	}
	if newerVersion("junk", "0.3.2") || newerVersion("0.3.3", "dev") {
		t.Error("an unreadable version produced a notice")
	}
}

func TestNewerVersionIsStrict(t *testing.T) {
	if newerVersion("0.3.2", "0.3.2") {
		t.Error("the same version is not newer")
	}
	// Someone on a pre-release must not be told that the older stable is "newer".
	if newerVersion("0.3.3", "0.4.0-rc1") {
		t.Error("an older stable release was offered to a newer pre-release")
	}
	if !newerVersion("0.4.0", "0.4.0-rc1") {
		t.Error("the release after its own pre-release was not offered")
	}
}

func gateTree() (root, status, upgrade, backupCreate *cobra.Command) {
	root = &cobra.Command{Use: "versola"}
	status = &cobra.Command{Use: "status"}
	status.Flags().Bool("json", false, "")
	upgrade = &cobra.Command{Use: "upgrade"}
	backup := &cobra.Command{Use: "backup"}
	backupCreate = &cobra.Command{Use: "create"}
	backup.AddCommand(backupCreate)
	root.AddCommand(status, upgrade, backup, &cobra.Command{Use: "version"}, &cobra.Command{Use: "completion"}, &cobra.Command{Use: "uninstall"})
	return
}

func TestUpdateNoticeGate(t *testing.T) {
	root, status, upgrade, backupCreate := gateTree()
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	none := env(nil)

	if !updateNoticeAllowed(status, true, none, "0.3.2") {
		t.Fatal("the plain case must be allowed")
	}
	if !updateNoticeAllowed(backupCreate, true, none, "0.3.2") {
		t.Error("a subcommand of an ordinary command must be allowed")
	}
	deny := map[string]bool{
		"not a terminal":       !updateNoticeAllowed(status, false, none, "0.3.2"),
		"dev build":            !updateNoticeAllowed(status, true, none, "dev"),
		"opt-out set":          !updateNoticeAllowed(status, true, env(map[string]string{noUpdateNotifierEnv: "1"}), "0.3.2"),
		"opt-out set to empty": !updateNoticeAllowed(status, true, env(map[string]string{noUpdateNotifierEnv: ""}), "0.3.2"),
		"CI":                   !updateNoticeAllowed(status, true, env(map[string]string{"CI": "true"}), "0.3.2"),
		"upgrade":              !updateNoticeAllowed(upgrade, true, none, "0.3.2"),
		"uninstall":            !updateNoticeAllowed(findSub(root, "uninstall"), true, none, "0.3.2"),
		"version":              !updateNoticeAllowed(versionCmdOf(root), true, none, "0.3.2"),
		"completion":           !updateNoticeAllowed(completionCmdOf(root), true, none, "0.3.2"),
	}
	_ = status.Flags().Set("json", "true")
	deny["--json"] = !updateNoticeAllowed(status, true, none, "0.3.2")
	for name, denied := range deny {
		if !denied {
			t.Errorf("the notice was allowed with: %s", name)
		}
	}
}

// noticeEnv wires everything the lookup touches to a sandbox: the release API
// (counting requests), the cache file, the terminal and the version.
type noticeEnv struct {
	requests *atomic.Int32
	cache    string
	now      time.Time
}

func newNoticeEnv(t *testing.T, apiStatus int, latest string) *noticeEnv {
	t.Helper()
	e := &noticeEnv{requests: new(atomic.Int32), now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		if apiStatus != http.StatusOK {
			http.Error(w, "no", apiStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": latest})
	})
	e.cache = filepath.Join(t.TempDir(), ".versola", "update-check.json")
	oldPath, oldTTY, oldNow, oldVer := updateCachePath, outputIsInteractive, updateNow, version
	updateCachePath = func() (string, error) { return e.cache, nil }
	outputIsInteractive = func() bool { return true }
	updateNow = func() time.Time { return e.now }
	version = "0.3.2"
	// The gate reads the environment: CI (set on GitHub Actions) and the
	// opt-out (set on some developer machines) would close it for every test.
	t.Setenv("CI", "")
	t.Setenv(noUpdateNotifierEnv, "")
	if err := os.Unsetenv(noUpdateNotifierEnv); err != nil { // "set to empty" counts as set
		t.Fatal(err)
	}
	t.Cleanup(func() {
		updateCachePath, outputIsInteractive, updateNow, version = oldPath, oldTTY, oldNow, oldVer
	})
	return e
}

func (e *noticeEnv) writeCache(t *testing.T, c updateCache) {
	t.Helper()
	b, _ := json.Marshal(c)
	if err := os.MkdirAll(filepath.Dir(e.cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cache, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *noticeEnv) readCache(t *testing.T) updateCache {
	t.Helper()
	b, err := os.ReadFile(e.cache)
	if err != nil {
		t.Fatalf("no cache written: %v", err)
	}
	var c updateCache
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("cache is not JSON: %v", err)
	}
	return c
}

func runNotice(t *testing.T) string {
	t.Helper()
	root, status, _, _ := gateTree()
	_ = root
	u := startUpdateCheck(status)
	if u == nil {
		return "<no check>"
	}
	var out bytes.Buffer
	u.finish(&out)
	return out.String()
}

const wantNotice = "A newer versola-cli is available: 0.3.3 (you have 0.3.2). Run versola upgrade.\n"

func TestNoticePrintsOnceADay(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.3")

	if got := runNotice(t); got != wantNotice {
		t.Fatalf("first run printed %q, want %q", got, wantNotice)
	}
	if e.requests.Load() != 1 {
		t.Fatalf("%d requests, want 1", e.requests.Load())
	}
	c := e.readCache(t)
	if c.Latest != "0.3.3" || !c.CheckedAt.Equal(e.now) || !c.NotifiedAt.Equal(e.now) {
		t.Fatalf("cache = %+v", c)
	}

	// Later the same day: nothing asked, nothing said.
	e.now = e.now.Add(3 * time.Hour)
	if got := runNotice(t); got != "" {
		t.Fatalf("the same day printed %q", got)
	}
	if e.requests.Load() != 1 {
		t.Fatalf("the same day asked GitHub again (%d requests)", e.requests.Load())
	}

	// The next day: asked again, said again (still not upgraded).
	e.now = e.now.Add(24 * time.Hour)
	if got := runNotice(t); got != wantNotice {
		t.Fatalf("the next day printed %q", got)
	}
	if e.requests.Load() != 2 {
		t.Fatalf("%d requests, want 2", e.requests.Load())
	}
}

func TestNoticeFromCacheAfterAFreshCheckThatWasNotShown(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.3")
	// Checked an hour ago (by a run that could not print: it failed, say),
	// never shown: the notice comes from the cache, without a request.
	e.writeCache(t, updateCache{CheckedAt: e.now.Add(-time.Hour), Latest: "0.3.3"})
	if got := runNotice(t); got != wantNotice {
		t.Fatalf("printed %q", got)
	}
	if e.requests.Load() != 0 {
		t.Fatalf("asked GitHub with a fresh cache (%d requests)", e.requests.Load())
	}
}

func TestNoticeSilentWhenCurrentOrAhead(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.2")
	if got := runNotice(t); got != "" {
		t.Errorf("current version printed %q", got)
	}
	version = "0.4.0-rc1"
	e.now = e.now.Add(48 * time.Hour)
	e.writeCache(t, updateCache{CheckedAt: e.now, Latest: "0.3.3"})
	if got := runNotice(t); got != "" {
		t.Errorf("a pre-release newer than latest printed %q", got)
	}
}

func TestNoticeFailedLookupIsSilentAndRemembered(t *testing.T) {
	e := newNoticeEnv(t, http.StatusInternalServerError, "")
	e.writeCache(t, updateCache{CheckedAt: e.now.Add(-48 * time.Hour), Latest: "0.3.3", NotifiedAt: e.now.Add(-time.Hour)})
	if got := runNotice(t); got != "" {
		t.Fatalf("a failed lookup printed %q", got)
	}
	c := e.readCache(t)
	if !c.CheckedAt.Equal(e.now) {
		t.Fatalf("the failed lookup was not remembered: %+v", c)
	}
	if c.Latest != "0.3.3" {
		t.Fatalf("the failed lookup lost the known release: %+v", c)
	}
	// Offline machines pay for it once a day, not on every command.
	runNotice(t)
	if e.requests.Load() != 1 {
		t.Fatalf("%d requests, want 1", e.requests.Load())
	}
}

func TestNoticeSlowLookupDoesNotHoldTheCommand(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.3")
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		time.Sleep(2 * time.Second)
	})
	old := updateCheckTimeout
	updateCheckTimeout = 100 * time.Millisecond
	t.Cleanup(func() { updateCheckTimeout = old })

	start := time.Now()
	// updateNow is frozen, so the deadline computed from it is in the past
	// relative to time.Now: finish must not wait at all for a lookup that
	// has not answered.
	got := runNotice(t)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("the command was held for %v", elapsed)
	}
	if got != "" {
		t.Fatalf("a lookup that never answered printed %q", got)
	}
}

func TestNoticeIgnoresATamperedCache(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.2")
	e.writeCache(t, updateCache{CheckedAt: e.now, Latest: "9.9.9\x1b]0;pwned\x07"})
	if got := runNotice(t); got != "" {
		t.Fatalf("a latest that is not a version was printed: %q", got)
	}
	// A cache that is not JSON is the same as none: ask again.
	if err := os.WriteFile(e.cache, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runNotice(t); got != "" || e.requests.Load() != 1 {
		t.Fatalf("corrupt cache: printed %q after %d requests", got, e.requests.Load())
	}
}

func TestNoticeNotAskedWhenGateIsClosed(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.3")
	outputIsInteractive = func() bool { return false }
	if got := runNotice(t); got != "<no check>" || e.requests.Load() != 0 {
		t.Fatalf("no terminal: %q, %d requests", got, e.requests.Load())
	}
	outputIsInteractive = func() bool { return true }
	t.Setenv(noUpdateNotifierEnv, "1")
	if got := runNotice(t); got != "<no check>" || e.requests.Load() != 0 {
		t.Fatalf("opt-out: %q, %d requests", got, e.requests.Load())
	}
	if _, err := os.Stat(e.cache); err == nil {
		t.Fatal("a cache was written although nothing was checked")
	}
}

func TestCacheFileIsPrivate(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.3")
	runNotice(t)
	fi, err := os.Stat(e.cache)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("cache mode %v", perm)
	}
}

// Through Run: the notice follows the command's own output, only after
// success, and the exit code is untouched.
func TestRunPrintsTheNoticeAfterASuccessfulCommandOnly(t *testing.T) {
	e := newNoticeEnv(t, http.StatusOK, "0.3.3")
	// "versions" lists the Versola repo (any path on the fake API answers
	// with a tag_name object or a list); give it the right shape per path.
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		if strings.Contains(r.URL.Path, "/releases/latest") {
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "0.3.3"})
			return
		}
		_, _ = w.Write([]byte("[]"))
	})

	var stderr bytes.Buffer
	if code := Run([]string{"versions", "--limit", "1"}, &stderr); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), wantNotice) {
		t.Fatalf("no notice after a successful command: %q", stderr.String())
	}

	// A failing command: no notice.
	e.now = e.now.Add(48 * time.Hour)
	stderr.Reset()
	if code := Run([]string{"versions", "--limit", "0"}, &stderr); code == 0 {
		t.Fatal("--limit 0 should fail")
	}
	if strings.Contains(stderr.String(), "newer versola-cli") {
		t.Fatalf("a failed command printed the notice: %q", stderr.String())
	}

	// "version" never asks. (The failed command above started a lookup in the
	// background that nobody waited for; let it land before counting.)
	waitForQuiet(e)
	before := e.requests.Load()
	stderr.Reset()
	Run([]string{"version"}, &stderr)
	if e.requests.Load() != before || strings.Contains(stderr.String(), "newer versola-cli") {
		t.Fatalf("`version` took part in the notice (%d new requests, %q)", e.requests.Load()-before, stderr.String())
	}
}

func versionCmdOf(root *cobra.Command) *cobra.Command    { return findSub(root, "version") }
func completionCmdOf(root *cobra.Command) *cobra.Command { return findSub(root, "completion") }

func findSub(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	panic("no command " + name)
}

// waitForQuiet waits until the fake API has stopped getting requests: a
// command that failed leaves its lookup running in the background, and a test
// that counts requests has to let it land first.
func waitForQuiet(e *noticeEnv) {
	last := int32(-1)
	for e.requests.Load() != last {
		last = e.requests.Load()
		time.Sleep(150 * time.Millisecond)
	}
}
