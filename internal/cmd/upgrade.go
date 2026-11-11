package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/state"
)

// upgradeRepo is versola-cli's own repository -- not to be confused with
// the Versola product repo that "bootstrap" pulls versola-tools from.
// This command only ever touches the CLI binary itself.
const upgradeRepo = "versolauth/versola-cli"

var (
	upgradeAssumeYes bool
	upgradeCheck     bool
	upgradeJSON      bool
	upgradeVersion   string
)

// upgradeDownloadHost is where release binaries are downloaded from; tests point it at an httptest server.
var upgradeDownloadHost = "https://github.com"

var upgradeCmd = &cobra.Command{
	Use:     "upgrade",
	GroupID: groupUpgrade,
	Short:   "Update versola-cli to the latest (or a given) release",
	Long: `upgrade checks the latest versola-cli release on GitHub, and if this
binary is older, downloads it (verified against that release's published
checksums.txt, same as install.sh/install.ps1 do) and replaces the running
executable in place. It never runs by itself: the only thing that happens
without you asking is a one-line notice, at most once a day, that a newer
release exists.

  versola upgrade                  update to the latest release
  versola upgrade --check          only say whether a newer release exists
  versola upgrade --check --json   the same for scripts: {current, latest, upToDate}
  versola upgrade --version 0.3.3  install exactly this release (a pre-release too)

The notice is printed to stderr after a command that succeeded, only in a
terminal, never with --json, and is turned off by setting
VERSOLA_NO_UPDATE_NOTIFIER to any value.

Deliberately dependency-free: it reuses the exact same GitHub API
endpoint and checksums.txt convention install.sh/install.ps1 already
use, rather than pulling in a self-update library for one command.

Builds made straight from source ("versola version" reports "dev") aren't
tied to any published release, so upgrade refuses to touch those --
rebuild from source instead.`,
	Args: cobra.NoArgs,
	RunE: runUpgrade,
}

func init() {
	upgradeCmd.Flags().BoolVarP(&upgradeAssumeYes, "yes", "y", false, "don't prompt for confirmation")
	upgradeCmd.Flags().BoolVar(&upgradeCheck, "check", false, "only report whether a newer release exists (exit 0 either way, 1 if GitHub can't be reached)")
	upgradeCmd.Flags().BoolVar(&upgradeJSON, "json", false, "with --check: print {current, latest, upToDate} on stdout")
	upgradeCmd.Flags().StringVar(&upgradeVersion, "version", "", "install exactly this release (e.g. 0.3.3, or a pre-release such as 0.4.0-rc1)")
}

// httpStatusError is a download that GitHub answered with a status other than
// 200; the code is kept so "no such release" can be told from a broken network.
type httpStatusError struct {
	Code   int
	Status string
}

func (e *httpStatusError) Error() string { return "server returned " + e.Status }

func runUpgrade(cmd *cobra.Command, args []string) error {
	pinned := cmd.Flags().Changed("version")
	if upgradeJSON && !upgradeCheck {
		return exitcode.With(exitcode.Usage, fmt.Errorf("--json goes with --check"))
	}
	if upgradeCheck && pinned {
		return exitcode.With(exitcode.Usage, fmt.Errorf("--check and --version don't go together: --check only reports, --version installs"))
	}
	if upgradeCheck {
		return runUpgradeCheck(os.Stdout)
	}
	var pinnedTag string
	if pinned {
		// Before anything is touched: the tag ends up in a download URL.
		tag, err := normalizeReleaseTag(upgradeVersion)
		if err != nil {
			return exitcode.With(exitcode.Usage, err)
		}
		pinnedTag = tag
	}

	// A "dev" build didn't come from a release, so there's no meaningful
	// version to compare against and no guarantee it's even safe to
	// overwrite (e.g. it might not be at the path a normal install would
	// use). Bail out rather than guessing.
	if version == "dev" {
		fmt.Println(`This is a "dev" build (not installed from a release) -- nothing to upgrade. Rebuild from source instead.`)
		return nil
	}

	exe, err := executablePath()
	if err != nil {
		return fmt.Errorf("couldn't locate the running executable: %w", err)
	}

	// Best-effort: a previous upgrade on Windows may have left the old
	// binary behind under this name (see replaceBinary) because it
	// couldn't remove its own running file. It's harmless clutter, but
	// this is the one command that would ever create it, so it's also
	// the natural place to sweep it up on a later run.
	_ = os.Remove(exe + ".old")

	// Two clients, deliberately different: the release-tag lookup and the
	// checksums.txt fetch are both small, fast requests, so a 30s timeout
	// is generous. The binary download is a different story -- Timeout on
	// an http.Client covers the whole request including reading the body,
	// not just connecting, so applying the same 30s budget there would cap
	// how slow a connection can be before "upgrade" fails downloading a
	// several-MB binary in a way install.sh/install.ps1 never did (curl
	// there has no time limit at all). Only the download client goes
	// without one, matching that same no-limit behavior.
	client := &http.Client{Timeout: 30 * time.Second}
	downloadClient := &http.Client{}

	tag := pinnedTag
	question := "Install now?"
	if pinned {
		if tag == version {
			fmt.Printf("Already on %s.\n", version)
			return nil
		}
		layout := deployedLayout()
		warnAboutPinnedVersion(os.Stdout, tag, version, layout)
		question = pinnedQuestion(tag, version, layout)
		fmt.Printf("Installing versola-cli %s (you have %s).\n", tag, version)
	} else {
		latestTag, err := releaseClient(latestTimeout).Latest(upgradeRepo)
		if err != nil {
			return fmt.Errorf("couldn't check the latest release: %w", err)
		}
		// "Is there something to install" is "is it newer", not "is it
		// different": someone on a pre-release newer than the latest stable
		// release must not be offered a downgrade. A version that doesn't
		// parse (never a published tag) falls back to plain inequality.
		if cmp, ok := compareVersions(latestTag, version); latestTag == version || (ok && cmp <= 0) {
			if latestTag == version {
				fmt.Printf("Already on the latest version (%s).\n", version)
			} else {
				fmt.Printf("This build (%s) is newer than the latest release (%s) -- nothing to update. To install a specific release: versola upgrade --version <tag>.\n", version, latestTag)
			}
			return nil
		}
		tag = latestTag
		question = "Update now?"
		fmt.Printf("versola-cli %s is available (you have %s).\n", tag, version)
	}

	assetName, err := releaseAssetName()
	if err != nil {
		return err
	}
	if !upgradeAssumeYes && !confirm(question) {
		fmt.Println("Aborted.")
		return exitcode.Reported(exitcode.Aborted, fmt.Errorf("aborted"))
	}
	return installRelease(client, downloadClient, exe, tag, assetName, pinned)
}

// installRelease downloads tag's binary next to exe, verifies it and swaps it
// in. pinned only changes what a 404 means: for a tag the customer typed, "no
// such release", not a network problem.
func installRelease(client, downloadClient *http.Client, exe, tag, assetName string, pinned bool) error {
	baseURL := fmt.Sprintf("%s/%s/releases/download/%s", upgradeDownloadHost, upgradeRepo, tag)

	fmt.Printf("Downloading %s...\n", assetName)
	// Download into the same directory as the target executable, not a
	// system temp dir -- os.Rename (used by replaceBinary) needs both
	// paths on the same filesystem to work atomically, and a temp dir on
	// a different drive/mount is a real possibility (e.g. C: vs a temp
	// dir on another volume).
	newPath := exe + ".new"
	defer os.Remove(newPath) // no-op once the rename below has moved it away
	newBytes, err := downloadToFile(downloadClient, baseURL+"/"+assetName, newPath)
	if err != nil {
		var se *httpStatusError
		if pinned && errors.As(err, &se) && se.Code == http.StatusNotFound {
			return fmt.Errorf("there is no versola-cli release %s with a %s binary -- the releases are listed at https://github.com/%s/releases (or run: versola upgrade --check)", tag, assetName, upgradeRepo)
		}
		return fmt.Errorf("couldn't download %s: %w", assetName, err)
	}

	if err := verifyChecksum(client, baseURL, assetName, newBytes); err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(newPath, 0o755); err != nil {
			return fmt.Errorf("couldn't set the new binary executable: %w", err)
		}
	}

	fmt.Println("Installing...")
	if err := replaceBinary(newPath, exe); err != nil {
		return fmt.Errorf("update failed (the previous binary should still be intact): %w", err)
	}

	fmt.Printf("Updated to %s.\n", tag)
	return nil
}

// upgradeCheckReport is the --json form of "upgrade --check".
type upgradeCheckReport struct {
	Current  string `json:"current"`
	Latest   string `json:"latest"`
	UpToDate bool   `json:"upToDate"`
}

// runUpgradeCheck says whether a newer release exists, and changes nothing.
// Exit 0 whether or not there is one (a script reads the answer, not the
// code); 1 only when GitHub can't be asked.
func runUpgradeCheck(w io.Writer) error {
	latest, err := releaseClient(latestTimeout).Latest(upgradeRepo)
	if err != nil {
		return fmt.Errorf("couldn't check the latest release: %w", err)
	}
	cmp, ok := compareVersions(latest, version)
	// A "dev" build is never up to date: it isn't any release.
	upToDate := version != "dev" && (latest == version || (ok && cmp <= 0))
	if upgradeJSON {
		return json.NewEncoder(w).Encode(upgradeCheckReport{Current: version, Latest: latest, UpToDate: upToDate})
	}
	switch {
	case version == "dev":
		fmt.Fprintf(w, "versola-cli %s is the latest release; this is a \"dev\" build, which versola upgrade does not replace.\n", latest)
	case upToDate:
		fmt.Fprintf(w, "versola-cli %s is current\n", version)
	default:
		fmt.Fprintf(w, "versola-cli %s is available (you have %s): run versola upgrade\n", latest, version)
	}
	return nil
}

// normalizeReleaseTag turns what was typed after --version into a release
// tag: one leading "v" is dropped (tags have none, but it is the natural way
// to write a version), and the rest must be a version (0.3.3, 0.4.0-rc1).
// Nothing else reaches the download URL.
func normalizeReleaseTag(s string) (string, error) {
	tag := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if _, _, ok := parseVersion(tag); !ok || strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("%q is not a versola-cli version: give a release such as 0.3.3 or 0.4.0-rc1 (they are listed at https://github.com/%s/releases)", s, upgradeRepo)
	}
	return tag, nil
}

// deployedLayout is the layout of the state record on this machine: 0 when
// there is none or it cannot be read (then the generic warning stands). A
// read-only look: state.Load changes nothing, and only the number leaves it.
func deployedLayout() int {
	st, err := state.Load()
	if err != nil {
		return 0
	}
	return st.SchemaVersion
}

// layout5FirstRelease is the first versola-cli release that reads state layout
// 5, "" while no release has it. A release from it on reads layout 5, so going
// back to it (or between two such releases) locks nobody out. SET IT when the
// first release that writes layout 5 is tagged: until then every release the
// command can install is older than layout 5 and the definite warning is true.
var layout5FirstRelease = ""

// goesBackBeyondState is whether installing tag over current would leave
// this deployment's state (in layout `layout`) unreadable by the CLI it
// installs: tag is older than the running binary and than the first release
// that reads layout 5, and the record carries a field of a layout newer than
// BaseSchemaVersion, which every older CLI refuses ("state layout 5, this one
// reads up to 4"). Layout 4 and lower were read by every release this command
// can install, so it is false there.
func goesBackBeyondState(tag, current string, layout int) bool {
	cmp, ok := compareVersions(tag, current)
	if !ok || cmp >= 0 || layout <= state.BaseSchemaVersion {
		return false
	}
	if layout5FirstRelease != "" {
		if c, ok := compareVersions(tag, layout5FirstRelease); !ok || c >= 0 {
			return false
		}
	}
	return true
}

// pinnedQuestion is what `upgrade --version` asks before replacing the binary:
// sharper when the release would lock the deployment's state out.
func pinnedQuestion(tag, current string, layout int) string {
	if goesBackBeyondState(tag, current, layout) {
		return "Install it anyway?"
	}
	return "Install now?"
}

// warnAboutPinnedVersion says what is unusual about the release the customer
// asked for: a pre-release, or one older than the binary they run, since a
// newer CLI may have written state (~/.versola/active) that an older one
// cannot read. Neither is refused: both are things someone does on purpose.
// layout is deployedLayout(): above BaseSchemaVersion the second warning is
// not "may" but a fact, and names the commands that bring the newer CLI back
// (the install script with the version, because `versola upgrade` run by the
// older CLI asks for the latest stable release, and a layout-5 release may
// still be a pre-release).
func warnAboutPinnedVersion(w io.Writer, tag, current string, layout int) {
	if _, pre, ok := parseVersion(tag); ok && len(pre) > 0 {
		fmt.Fprintf(w, "Warning: %s is a pre-release.\n", tag)
	}
	cmp, ok := compareVersions(tag, current)
	if !ok || cmp >= 0 {
		return
	}
	if goesBackBeyondState(tag, current, layout) {
		fmt.Fprintf(w, "Warning: %s is older than this binary (%s), and the deployment on this machine is recorded in state layout %d, which %s cannot read: every command of it that reads the state will stop with \"state layout %d, this one reads up to %d -- upgrade versola\".\nNothing is lost and nothing changes in Docker or in ~/.versola, but to run Versola commands again you will have to install %s again, with the install script and its version:\n  macOS/Linux: curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sh -s %s\n  Windows:     $env:VERSOLA_VERSION = \"%s\"; iwr https://raw.githubusercontent.com/%s/main/install.ps1 -useb | iex\n(If %s has `versola upgrade --version`, that works too; older releases only have `versola upgrade`, which installs the latest stable release.)\n", tag, current, layout, tag, layout, state.BaseSchemaVersion, current, upgradeRepo, current, current, upgradeRepo, tag)
		return
	}
	fmt.Fprintf(w, "Warning: %s is older than this binary (%s). A newer versola-cli may have written deployment state (~/.versola/active) that an older one cannot read; if a command then reports an unreadable state layout, install the newer release again.\n", tag, current)
}

// executablePath returns the real, symlink-resolved path to the running
// binary. os.Executable() alone can return a symlink on some platforms --
// resolving it matters here specifically because upgrade is about to
// replace whatever file actually holds the binary's contents, not
// whatever path happens to point at it.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// releaseAssetName maps the running binary's OS/arch to the asset name
// release.yml publishes -- e.g. "versola-windows-amd64.exe". Go's own
// runtime.GOOS/GOARCH already give exactly the values used there, unlike
// install.sh's "uname" output, which needs translating first.
func releaseAssetName() (string, error) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		return "versola-windows-amd64.exe", nil
	}
	if runtime.GOOS == "darwin" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		return fmt.Sprintf("versola-darwin-%s", runtime.GOARCH), nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		return "versola-linux-amd64", nil
	}
	return "", fmt.Errorf("no published release binary for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// httpGet is a GET request with an explicit User-Agent -- Go's own default
// ("Go-http-client/1.1") already satisfies GitHub's bare requirement that
// some User-Agent be present, but a client-identifying one is more useful
// if GitHub ever needs to flag or rate-limit a specific caller, and there's
// no reason not to send one.
func httpGet(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	return client.Do(req)
}

// userAgent is what every GitHub request of this CLI identifies itself as
// (downloads here, and the release lookups of internal/release).
func userAgent() string {
	return "versola-cli/" + version
}

// downloadToFile streams url into path and also returns the full body, so
// callers can hash it for checksum verification without a second read of
// the (now-closed) file.
func downloadToFile(client *http.Client, url, path string) ([]byte, error) {
	resp, err := httpGet(client, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{Code: resp.StatusCode, Status: resp.Status}
	}

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(body); err != nil {
		return nil, err
	}
	return body, nil
}

// verifyChecksum mirrors install.sh/install.ps1's policy exactly: a
// missing checksums.txt (HTTP 404, e.g. an older release that predates
// checksums existing) is a soft skip, not an error, but any other
// failure to fetch it is a distinct warning -- that case means
// verification didn't happen, which isn't the same claim as "wasn't
// needed".
func verifyChecksum(client *http.Client, baseURL, assetName string, content []byte) error {
	resp, err := httpGet(client, baseURL+"/checksums.txt")
	if err != nil {
		fmt.Printf("Warning: could not fetch checksums.txt (%v), skipping verification\n", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		fmt.Println("Note: this release did not publish checksums.txt, skipping verification.")
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Warning: could not fetch checksums.txt (HTTP %d), skipping verification\n", resp.StatusCode)
		return nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Warning: could not read checksums.txt (%v), skipping verification\n", err)
		return nil
	}

	expected, ok := checksumForAsset(data, assetName)
	if !ok {
		fmt.Printf("Warning: checksums.txt has no entry for %s, skipping verification\n", assetName)
		return nil
	}

	fmt.Println("Verifying checksum...")
	sum := sha256.Sum256(content)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum mismatch for %s\n  expected: %s\n  actual:   %s", assetName, expected, actual)
	}
	fmt.Println("Checksum OK.")
	return nil
}

// checksumForAsset finds the entry for want in a plain "sha256sum" style
// checksums.txt ("<hash>  <filename>" per line). Matches by exact
// filename, not substring, and strips a leading "*" (sha256sum's
// binary-mode marker) or "./" (relative-path prefix) from the filename
// column first -- same conventions install.sh/install.ps1 already handle,
// so verification doesn't silently no-op just because a release used one
// of them.
func checksumForAsset(data []byte, want string) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = strings.TrimPrefix(name, "./")
		if name == want {
			return fields[0], true
		}
	}
	return "", false
}

// replaceBinary swaps newPath into place at targetPath.
//
// On Windows, a running .exe can't be overwritten or deleted in place --
// the OS locks it while it's mapped for execution. Renaming it aside is
// allowed, though (this is the same trick major desktop apps use to
// self-update while running): move the current exe to targetPath+".old",
// move the new one into targetPath, and try to remove the ".old" file --
// which will typically fail here too, since it's still this very process
// executing from it, so runUpgrade sweeps it up on a later run instead.
//
// On macOS/Linux, none of this is needed: replacing a running process's
// underlying file works directly (the OS keeps the old inode alive via
// the already-open file descriptor until the process exits).
func replaceBinary(newPath, targetPath string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(newPath, targetPath)
	}

	oldPath := targetPath + ".old"
	_ = os.Remove(oldPath) // clear out any leftover from a previous upgrade, if present
	if err := os.Rename(targetPath, oldPath); err != nil {
		return fmt.Errorf("couldn't move the current binary aside: %w", err)
	}
	if err := os.Rename(newPath, targetPath); err != nil {
		// Try to put the original back so a failed upgrade doesn't leave
		// the user with no working binary at all.
		if rollbackErr := os.Rename(oldPath, targetPath); rollbackErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v -- the previous binary is at %s)", err, rollbackErr, oldPath)
		}
		return err
	}
	_ = os.Remove(oldPath) // best-effort; see doc comment above
	return nil
}
