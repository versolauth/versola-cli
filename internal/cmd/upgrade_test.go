package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/exitcode"
)

// setVersion makes the binary report v for the duration of the test.
func setVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

// resetUpgradeFlags undoes what a test did to upgrade's flags: they are
// package-level, and cobra remembers a flag as "changed" for the process.
func resetUpgradeFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		upgradeAssumeYes, upgradeCheck, upgradeJSON, upgradeVersion = false, false, false, ""
		for _, n := range []string{"yes", "check", "json", "version"} {
			if f := upgradeCmd.Flags().Lookup(n); f != nil {
				f.Changed = false
			}
		}
	})
}

func latestReleaseAPI(t *testing.T, tag string) {
	t.Helper()
	withReleaseAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag})
			return
		}
		http.NotFound(w, r)
	})
}

func TestUpgradeCheckText(t *testing.T) {
	resetUpgradeFlags(t)
	latestReleaseAPI(t, "0.3.3")
	for _, c := range []struct{ current, want string }{
		{"0.3.2", "versola-cli 0.3.3 is available (you have 0.3.2): run versola upgrade\n"},
		{"0.3.3", "versola-cli 0.3.3 is current\n"},
		{"0.4.0-rc1", "versola-cli 0.4.0-rc1 is current\n"}, // ahead of the latest release: nothing to offer
		{"dev", "versola-cli 0.3.3 is the latest release; this is a \"dev\" build, which versola upgrade does not replace.\n"},
	} {
		setVersion(t, c.current)
		var out bytes.Buffer
		if err := runUpgradeCheck(&out); err != nil {
			t.Fatalf("%s: %v", c.current, err)
		}
		if out.String() != c.want {
			t.Errorf("%s: got %q, want %q", c.current, out.String(), c.want)
		}
	}
}

func TestUpgradeCheckJSON(t *testing.T) {
	resetUpgradeFlags(t)
	latestReleaseAPI(t, "0.3.3")
	upgradeJSON = true
	for _, c := range []struct {
		current  string
		upToDate bool
	}{{"0.3.2", false}, {"0.3.3", true}, {"0.4.0-rc1", true}, {"dev", false}} {
		setVersion(t, c.current)
		var out bytes.Buffer
		if err := runUpgradeCheck(&out); err != nil {
			t.Fatal(err)
		}
		var got upgradeCheckReport
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("%s: not JSON: %q", c.current, out.String())
		}
		want := upgradeCheckReport{Current: c.current, Latest: "0.3.3", UpToDate: c.upToDate}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", c.current, got, want)
		}
	}
}

func TestUpgradeCheckUnreachableFailsWithoutOutput(t *testing.T) {
	resetUpgradeFlags(t)
	withReleaseAPI(t, nil) // closed server
	setVersion(t, "0.3.2")
	upgradeJSON = true
	var out bytes.Buffer
	err := runUpgradeCheck(&out)
	if err == nil || !strings.Contains(err.Error(), "couldn't check the latest release") {
		t.Fatalf("err = %v", err)
	}
	if exitcode.Code(err) != exitcode.Failure {
		t.Fatalf("exit code %d, want 1", exitcode.Code(err))
	}
	if out.Len() != 0 {
		t.Fatalf("stdout got %q on a failed check", out.String())
	}
}

func TestUpgradeFlagMisuseIsAUsageError(t *testing.T) {
	resetUpgradeFlags(t)
	setVersion(t, "0.3.2")
	for name, set := range map[string]map[string]string{
		"--json alone":        {"json": "true"},
		"--check --version":   {"check": "true", "version": "0.3.3"},
		"version not a tag":   {"version": "../../evil"},
		"version with a path": {"version": "0.3.3/x"},
		"version is latest":   {"version": "latest"},
		"two v":               {"version": "vv0.3.3"},
		"empty":               {"version": ""},
	} {
		resetUpgradeFlags(t)
		for k, v := range set {
			if err := upgradeCmd.Flags().Set(k, v); err != nil {
				t.Fatal(err)
			}
		}
		err := runUpgrade(upgradeCmd, nil)
		if err == nil || exitcode.Code(err) != exitcode.Usage {
			t.Errorf("%s: err = %v (code %d), want a usage error", name, err, exitcode.Code(err))
		}
	}
}

func TestNormalizeReleaseTag(t *testing.T) {
	for in, want := range map[string]string{
		"0.3.3": "0.3.3", "v0.3.3": "0.3.3", " 0.3.3 ": "0.3.3", "0.4.0-rc1": "0.4.0-rc1", "v0.1.0-beta.2": "0.1.0-beta.2",
	} {
		got, err := normalizeReleaseTag(in)
		if err != nil || got != want {
			t.Errorf("normalizeReleaseTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestPinnedVersionWarnings(t *testing.T) {
	var out bytes.Buffer
	warnAboutPinnedVersion(&out, "0.4.0-rc1", "0.3.2", 0)
	if got := out.String(); got != "Warning: 0.4.0-rc1 is a pre-release.\n" {
		t.Errorf("pre-release: %q", got)
	}
	out.Reset()
	warnAboutPinnedVersion(&out, "0.3.1", "0.3.2", 0)
	if got := out.String(); !strings.Contains(got, "0.3.1 is older than this binary (0.3.2)") || !strings.Contains(got, "cannot read") {
		t.Errorf("downgrade: %q", got)
	}
	out.Reset()
	warnAboutPinnedVersion(&out, "0.3.3", "0.3.2", 0)
	if out.Len() != 0 {
		t.Errorf("an ordinary newer release warned: %q", out.String())
	}
}

// With the deployment recorded in layout 5, going back to an older release is
// not "may": the warning says the older CLI will stop on the state, that
// nothing is lost, and the commands that bring the current release back. In
// layout 4 (or no record) the generic warning stays, word for word.
func TestPinnedVersionWarningForLayout5(t *testing.T) {
	var out bytes.Buffer
	warnAboutPinnedVersion(&out, "0.3.1", "0.8.0", 5)
	got := out.String()
	for _, want := range []string{
		"state layout 5", "cannot read", "this one reads up to 4 -- upgrade versola",
		"install.sh | sh -s 0.8.0", "VERSOLA_VERSION", "install.ps1 -useb | iex", "Nothing is lost", "older releases only have `versola upgrade`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("layout 5: lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "may have written") {
		t.Errorf("layout 5 still hedges: %s", got)
	}
	for _, layout := range []int{0, 3, 4} {
		out.Reset()
		warnAboutPinnedVersion(&out, "0.3.1", "0.8.0", layout)
		if g := out.String(); !strings.Contains(g, "may have written") || strings.Contains(g, "install.sh") {
			t.Errorf("layout %d: %q", layout, g)
		}
	}
	// A release that is not older, or a pre-release newer one, is not a way back.
	out.Reset()
	warnAboutPinnedVersion(&out, "0.9.0", "0.8.0", 5)
	if out.Len() != 0 {
		t.Errorf("a newer release warned: %q", out.String())
	}
}

// Once a release reads layout 5, going back to it (or between two such
// releases) is not a lockout and must not be said to be one.
func TestGoesBackBeyondStateStopsAtFirstLayout5Release(t *testing.T) {
	old := layout5FirstRelease
	layout5FirstRelease = "0.5.0"
	t.Cleanup(func() { layout5FirstRelease = old })
	for _, c := range []struct {
		tag  string
		want bool
	}{
		{"0.4.9", true},
		{"0.5.0-rc1", true},
		{"0.5.0", false},
		{"0.6.0", false},
	} {
		if got := goesBackBeyondState(c.tag, "0.8.0", 5); got != c.want {
			t.Errorf("%s: got %v", c.tag, got)
		}
	}
}

func TestPinnedQuestion(t *testing.T) {
	if got := pinnedQuestion("0.3.1", "0.8.0", 5); got != "Install it anyway?" {
		t.Errorf("layout 5: %q", got)
	}
	for _, c := range []struct {
		tag    string
		layout int
	}{{"0.3.1", 4}, {"0.3.1", 0}, {"0.9.0", 5}} {
		if got := pinnedQuestion(c.tag, "0.8.0", c.layout); got != "Install now?" {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestGoesBackBeyondState(t *testing.T) {
	for _, c := range []struct {
		tag, cur string
		layout   int
		want     bool
	}{
		{"0.3.1", "0.8.0", 5, true},
		{"0.8.0-rc1", "0.8.0", 5, true},
		{"0.3.1", "0.8.0", 4, false},
		{"0.3.1", "0.8.0", 0, false},
		{"0.9.0", "0.8.0", 5, false},
		{"0.8.0", "0.8.0", 5, false},
	} {
		if got := goesBackBeyondState(c.tag, c.cur, c.layout); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

// deployedLayout only reads the record: its number comes back, the file is
// not touched, and no record or an unreadable one is 0.
func TestDeployedLayoutReadsOnly(t *testing.T) {
	writeVpsState(t, `{"schemaVersion":5,"target":"vps","version":"0.8.0","bundleDir":"bundles/b1","postgresMode":"bundled"}`)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".versola", "active", "state.json")
	before, _ := os.ReadFile(path)
	if got := deployedLayout(); got != 5 {
		t.Errorf("layout %d, want 5", got)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the record changed")
	}
	writeVpsState(t, `{"schemaVersion":4,"target":"vps","version":"0.8.0","bundleDir":"bundles/b1"}`)
	if got := deployedLayout(); got != 4 {
		t.Errorf("layout %d, want 4", got)
	}
	writeVpsState(t, `{"schemaVersion":99}`)
	if got := deployedLayout(); got != 0 {
		t.Errorf("a newer record: layout %d, want 0", got)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	if got := deployedLayout(); got != 0 {
		t.Errorf("no record: layout %d, want 0", got)
	}
}

// releaseDownloads serves the binary of tag at the path installRelease asks
// for, with a checksums.txt, and nothing for any other tag.
func releaseDownloads(t *testing.T, tag string, body []byte, goodSum bool) {
	t.Helper()
	asset, err := releaseAssetName()
	if err != nil {
		t.Skip(err)
	}
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])
	if !goodSum {
		hexSum = strings.Repeat("0", 64)
	}
	base := fmt.Sprintf("/%s/releases/download/%s/", upgradeRepo, tag)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case base + asset:
			_, _ = w.Write(body)
		case base + "checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", hexSum, asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := upgradeDownloadHost
	upgradeDownloadHost = srv.URL
	t.Cleanup(func() { upgradeDownloadHost = old })
}

func TestInstallPinnedRelease(t *testing.T) {
	asset, err := releaseAssetName()
	if err != nil {
		t.Skip(err)
	}
	exe := filepath.Join(t.TempDir(), "versola")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	releaseDownloads(t, "0.4.0-rc1", []byte("new binary"), true)
	if err := installRelease(http.DefaultClient, http.DefaultClient, exe, "0.4.0-rc1", asset, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("binary not replaced: %q", b)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("the download was left behind")
	}
}

func TestInstallRefusesAChecksumMismatchAndKeepsTheBinary(t *testing.T) {
	asset, err := releaseAssetName()
	if err != nil {
		t.Skip(err)
	}
	exe := filepath.Join(t.TempDir(), "versola")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	releaseDownloads(t, "0.4.0", []byte("tampered"), false)
	err = installRelease(http.DefaultClient, http.DefaultClient, exe, "0.4.0", asset, true)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("the binary was replaced despite the mismatch: %q", b)
	}
}

func TestInstallPinnedMissingReleaseSaysSo(t *testing.T) {
	asset, err := releaseAssetName()
	if err != nil {
		t.Skip(err)
	}
	exe := filepath.Join(t.TempDir(), "versola")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	releaseDownloads(t, "0.4.0", []byte("x"), true) // 9.9.9 is not served
	err = installRelease(http.DefaultClient, http.DefaultClient, exe, "9.9.9", asset, true)
	if err == nil || !strings.Contains(err.Error(), "there is no versola-cli release 9.9.9") || !strings.Contains(err.Error(), "versola upgrade --check") {
		t.Fatalf("err = %v", err)
	}
	// Not pinned: the 404 stays what it was, a download failure.
	err = installRelease(http.DefaultClient, http.DefaultClient, exe, "9.9.9", asset, false)
	if err == nil || !strings.Contains(err.Error(), "couldn't download") {
		t.Fatalf("unpinned err = %v", err)
	}
}

// The whole pinned path on a layout-5 deployment: the warning and the
// question come before anything is downloaded, an unanswered question (end of
// input, no --yes) is "no" -- exit code Aborted, the binary untouched -- and
// the state record is not changed.
func TestUpgradeOlderOverLayout5AsksAndChangesNothing(t *testing.T) {
	resetUpgradeFlags(t)
	setVersion(t, "0.8.0")
	writeVpsState(t, `{"schemaVersion":5,"target":"vps","version":"0.8.0","bundleDir":"bundles/b1","postgresMode":"bundled"}`)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".versola", "active", "state.json")
	before, _ := os.ReadFile(path)
	// A download would have nowhere to go: this host does not exist.
	old := upgradeDownloadHost
	upgradeDownloadHost = "http://127.0.0.1:1"
	t.Cleanup(func() { upgradeDownloadHost = old })
	if err := upgradeCmd.Flags().Set("version", "0.3.2"); err != nil {
		t.Fatal(err)
	}
	// End of input: a closed pipe, never the terminal or a hanging stdin.
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	w.Close()
	oldIn := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldIn; r.Close() })
	err := runUpgrade(upgradeCmd, nil)
	if err == nil || exitcode.Code(err) != exitcode.Aborted {
		t.Fatalf("err = %v (code %d), want an aborted question", err, exitcode.Code(err))
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the record changed")
	}
}

func TestUpgradeAlreadyCurrentOrAheadOffersNothing(t *testing.T) {
	resetUpgradeFlags(t)
	latestReleaseAPI(t, "0.3.3")
	for _, current := range []string{"0.3.3", "0.4.0-rc1"} {
		setVersion(t, current)
		// No --yes: any question would block on stdin, and a download would
		// fail (no host), so returning nil proves neither was reached.
		if err := runUpgrade(upgradeCmd, nil); err != nil {
			t.Errorf("%s: %v", current, err)
		}
	}
}
