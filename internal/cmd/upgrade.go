package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// upgradeRepo is versola-cli's own repository -- not to be confused with
// the Versola product repo that "bootstrap" pulls versola-tools from.
// This command only ever touches the CLI binary itself.
const upgradeRepo = "versolauth/versola-cli"

var upgradeAssumeYes bool

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Update versola-cli to the latest release",
	Long: `upgrade checks the latest versola-cli release on GitHub, and if it's
different from this binary's version, downloads it (verified against
that release's published checksums.txt, same as install.sh/install.ps1
do) and replaces the running executable in place.

Deliberately dependency-free: it reuses the exact same GitHub API
endpoint and checksums.txt convention install.sh/install.ps1 already
use, rather than pulling in a self-update library for one command.

Builds made straight from source ("versola version" reports "dev") aren't
tied to any published release, so upgrade refuses to touch those --
rebuild from source instead.`,
	RunE: runUpgrade,
}

func init() {
	upgradeCmd.Flags().BoolVarP(&upgradeAssumeYes, "yes", "y", false, "don't prompt for confirmation")
}

func runUpgrade(cmd *cobra.Command, args []string) error {
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

	latestTag, err := latestReleaseTag(client, upgradeRepo)
	if err != nil {
		return fmt.Errorf("couldn't check the latest release: %w", err)
	}

	// GitHub's "latest release" endpoint already excludes pre-releases
	// and always returns the newest non-pre-release tag, so a plain
	// inequality is enough here -- no need to pull in a semver library
	// just to answer "is there something to install".
	if latestTag == version {
		fmt.Printf("Already on the latest version (%s).\n", version)
		return nil
	}

	assetName, err := releaseAssetName()
	if err != nil {
		return err
	}

	fmt.Printf("versola-cli %s is available (you have %s).\n", latestTag, version)
	if !upgradeAssumeYes && !confirm("Update now?") {
		fmt.Println("Aborted.")
		return nil
	}

	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/%s", upgradeRepo, latestTag)

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

	fmt.Printf("Updated to %s.\n", latestTag)
	return nil
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

type githubRelease struct {
	TagName string `json:"tag_name"`
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
	req.Header.Set("User-Agent", "versola-cli/"+version)
	return client.Do(req)
}

func latestReleaseTag(client *http.Client, repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	resp, err := httpGet(client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("couldn't parse GitHub's response: %w", err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("GitHub's response didn't include a release tag")
	}
	return release.TagName, nil
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
		return nil, fmt.Errorf("server returned %s", resp.Status)
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
