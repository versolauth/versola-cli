package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// On vps the proxy does not mount its configuration from the deployment
// bundle but from a directory of its own (Config.GenerationsDir), the same
// for every bundle:
//
//	proxy-vps/
//	  gen-000007/       a whole configuration: nginx.conf, conf.d/,
//	  gen-000008/       stream.d/, proxy_params.conf, html/ (admin console)
//	  current -> gen-000008
//
// nginx runs with `-c /etc/nginx/versola/current/nginx.conf` and every
// include is relative to that file, so a reload reads one generation
// entirely -- never half of the old one and half of the new. A change is:
// build a new generation, have nginx check it in a throwaway container
// (before anything the running proxy uses is touched), switch `current` to
// it (a rename: atomic), reload. If the reload is refused, `current` goes
// back. Two things this fixes compared with mounting bundle files:
//
//   - A file bind-mounted on its own (nginx.conf was) keeps showing the
//     container the copy it was started with: an atomic rewrite makes a new
//     file, the mount still points at the old one. A mounted directory sees
//     what is renamed inside it.
//   - The mount source no longer changes with every configure, so Compose
//     does not recreate the container for a new bundle -- a reload is enough.

const (
	// GenerationMount is where GenerationsDir is mounted in the container.
	GenerationMount = "/etc/nginx/versola"
	// CurrentGeneration is the link to the generation in use.
	CurrentGeneration = "current"

	generationPrefix = "gen-"
	generationHTML   = "html"
	// buildSuffix marks a generation still being built, or one a build left
	// behind when it was cut off: never `current`, removed by the next build.
	buildSuffix = ".tmp"
)

// confPath is nginx.conf of a generation, as the container sees it.
func confPath(gen string) string {
	return GenerationMount + "/" + gen + "/nginx.conf"
}

// staticFiles are the files of a generation taken from the bundle as
// configure rendered them; the replicas do not change them.
var staticFiles = []string{"proxy/nginx.conf", "proxy/proxy_params.conf", "proxy/conf.d/versola.conf"}

// generationNumber reads gen-000007 as 7; ok is false for any other name.
func generationNumber(name string) (int, bool) {
	digits, found := strings.CutPrefix(name, generationPrefix)
	if !found || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

// buildGeneration writes a new generation into dir from the bundle's
// static files, its admin console and the given dynamic files (paths as in
// DynamicFiles), and returns its name. It is built under a temporary name
// and renamed when complete, so a gen-N directory is always whole.
func buildGeneration(dir, bundleDir string, dynamic map[string][]byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("couldn't create %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	next := 1
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), buildSuffix) {
			// Left by a build that was cut off.
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			continue
		}
		if n, ok := generationNumber(e.Name()); ok && n >= next {
			next = n + 1
		}
	}
	name := fmt.Sprintf("%s%06d", generationPrefix, next)
	tmp := filepath.Join(dir, name+buildSuffix)
	if err := fillGeneration(tmp, bundleDir, dynamic); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	// Readable by nginx's workers (uid 101), whatever this process's umask
	// cut from the modes it asked for: the master runs as root, so `nginx
	// -t` would not notice, and the admin console would answer 403.
	if err := makeReadable(dir, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("couldn't finish the proxy config %s: %w", name, err)
	}
	return name, nil
}

func fillGeneration(gen, bundleDir string, dynamic map[string][]byte) error {
	write := func(rel string, b []byte) error {
		path := filepath.Join(gen, strings.TrimPrefix(rel, "proxy/"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, b, 0o644)
	}
	for _, rel := range staticFiles {
		b, err := os.ReadFile(filepath.Join(bundleDir, rel))
		if err != nil {
			return fmt.Errorf("couldn't read the bundle's %s: %w", rel, err)
		}
		if err := write(rel, b); err != nil {
			return err
		}
	}
	// Only a routed central name has one (see Config.CentralHost).
	if b, err := os.ReadFile(filepath.Join(bundleDir, CentralStreamFile)); err == nil {
		if err := write(CentralStreamFile, b); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("couldn't read the bundle's %s: %w", CentralStreamFile, err)
	}
	for rel, b := range dynamic {
		if err := write(rel, b); err != nil {
			return err
		}
	}
	// stream.d exists even without a stream file: nginx.conf includes it.
	if err := os.MkdirAll(filepath.Join(gen, "stream.d"), 0o755); err != nil {
		return err
	}
	// The admin console, where routes.conf's root has it on vps.
	ui := filepath.Join(bundleDir, "central-ui")
	if err := copyTree(ui, filepath.Join(gen, generationHTML, "central", "admin")); err != nil {
		return fmt.Errorf("couldn't copy the admin console from %s: %w", ui, err)
	}
	return nil
}

// makeReadable sets dir itself and everything under gen to 0755 (dirs) or
// 0644 (files).
func makeReadable(dir, gen string) error {
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(gen, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Chmod(path, 0o755)
		case d.Type().IsRegular():
			return os.Chmod(path, 0o644)
		}
		return nil
	})
}

// copyTree copies the regular files and directories under src to dst,
// readable by nginx's workers (another user than this process).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil // a link or a device in a built web app: not served
	})
}

// validateGeneration has nginx check gen in a throwaway container of the
// proxy's image, with the generations directory mounted as the proxy
// mounts it -- before the running proxy is pointed at it. The ACME state
// directory is a tmpfs there: the module writes its account key during
// `nginx -t`, which must not reach the real volume.
func validateGeneration(dir, gen string) error {
	stateDir := "/var/cache/nginx/acme-letsencrypt"
	prepare := "mkdir -p " + stateDir + "/" + acmeStateDir(ACMEProduction) + " " + stateDir + "/" + acmeStateDir(ACMEStaging)
	_, err := dockerOutput("run", "--rm", "--network", "none", "--entrypoint", "sh",
		"--tmpfs", stateDir,
		// --mount, not -v: -v splits on colons. Its value is a CSV record,
		// so a path with a comma or a quote is quoted as one field.
		"--mount", "type=bind,"+csvField("src="+dir)+",dst="+GenerationMount+",readonly",
		Image, "-c", prepare+" && exec nginx -t -c "+confPath(gen))
	if err != nil {
		// 125 is docker run's own failure (no image and the pull failed,
		// no daemon, a bad flag): the config was never looked at.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 125 {
			return fmt.Errorf("couldn't run %s to check the new proxy config (nothing was changed): %s", Image, nginxSays(err))
		}
		return fmt.Errorf("nginx rejected the new proxy config (nothing was changed): %s", nginxSays(err))
	}
	return nil
}

// csvField is s as one field of a CSV record (docker's --mount value).
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// nginxSays is nginx's own words from a failed docker call (stderr), or
// the error itself. The proxy's config holds no secrets.
func nginxSays(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return string(bytes.TrimSpace(ee.Stderr))
	}
	return err.Error()
}

// currentGeneration is the generation `current` points at, "" when none.
func currentGeneration(dir string) (string, error) {
	target, err := os.Readlink(filepath.Join(dir, CurrentGeneration))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("couldn't read %s: %w", filepath.Join(dir, CurrentGeneration), err)
	}
	return filepath.Base(target), nil
}

// switchGeneration points `current` at gen, atomically: a new link renamed
// over the old one. The link is relative, so it resolves inside the
// container's mount just as on the host.
func switchGeneration(dir, gen string) error {
	if _, ok := generationNumber(gen); !ok {
		return fmt.Errorf("%q is not a proxy config generation", gen)
	}
	tmp := filepath.Join(dir, CurrentGeneration+buildSuffix)
	_ = os.Remove(tmp)
	if err := os.Symlink(gen, tmp); err != nil {
		return fmt.Errorf("couldn't switch the proxy config to %s: %w", gen, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, CurrentGeneration)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("couldn't switch the proxy config to %s: %w", gen, err)
	}
	return nil
}

// pruneGenerations removes every generation but those in keep. Failures
// are left alone: an old directory is only disk space.
func pruneGenerations(dir string, keep ...string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, e := range entries {
		if _, ok := generationNumber(e.Name()); ok && !kept[e.Name()] {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// Apply makes c the proxy configuration of a vps deployment without
// touching the running nginx: the bundle's dynamic files are rewritten, a
// new generation is built from the bundle and checked by nginx, and
// `current` is switched to it. It returns the generation `current` pointed
// at before ("" if none), for RollBack. If the check fails nothing changes.
// The running proxy picks the new generation up on ReloadCurrent, or when
// it next starts.
func Apply(bundleDir string, c Config) (previous string, err error) {
	if c.GenerationsDir == "" {
		return "", fmt.Errorf("Apply is for a proxy with config generations (vps)")
	}
	files, err := c.DynamicFiles()
	if err != nil {
		return "", err
	}
	if err := writeDynamic(bundleDir, files); err != nil {
		return "", err
	}
	gen, err := buildGeneration(c.GenerationsDir, bundleDir, files)
	if err != nil {
		return "", err
	}
	if err := validateGeneration(c.GenerationsDir, gen); err != nil {
		_ = os.RemoveAll(filepath.Join(c.GenerationsDir, gen))
		return "", err
	}
	previous, err = currentGeneration(c.GenerationsDir)
	if err != nil {
		_ = os.RemoveAll(filepath.Join(c.GenerationsDir, gen))
		return "", err
	}
	if err := switchGeneration(c.GenerationsDir, gen); err != nil {
		_ = os.RemoveAll(filepath.Join(c.GenerationsDir, gen))
		return "", err
	}
	pruneGenerations(c.GenerationsDir, gen, previous)
	return previous, nil
}

// RollBack points `current` back at previous (Apply's result); with no
// previous generation there is nothing to go back to.
func RollBack(dir, previous string) error {
	if previous == "" {
		return nil
	}
	return switchGeneration(dir, previous)
}

// ReloadCurrent has the running proxy of mode check the generation
// `current` points at and reload into it. nginx keeps serving its old
// configuration when either step fails.
func ReloadCurrent(mode string) error {
	name := ContainerFor(mode)
	conf := confPath(CurrentGeneration)
	if _, err := dockerOutput("exec", name, "nginx", "-c", conf, "-t"); err != nil {
		return fmt.Errorf("nginx rejected the new config (the proxy keeps its old one): %s", nginxSays(err))
	}
	if err := dockerRun("exec", name, "nginx", "-c", conf, "-s", "reload"); err != nil {
		return fmt.Errorf("the proxy's reload failed (it keeps its old config): %w", err)
	}
	return nil
}

// UsesGenerations reports whether the bundle's proxy.yml runs the proxy
// from config generations. A vps bundle configured by an older versola-cli
// mounts its own files instead, and its running proxy has no generations
// to reload into: such a bundle is configured again, not started.
func UsesGenerations(bundleDir string) bool {
	b, err := os.ReadFile(filepath.Join(bundleDir, ComposeFile))
	// CRLF too: a CLI built from a Windows checkout embeds templates with
	// CRLF line endings and writes them as they are.
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return err == nil && bytes.Contains(b, []byte("target: "+GenerationMount+"\n"))
}

// CurrentFrontsCentral reports whether the configuration nginx runs now (the
// `current` generation under genDir) fronts central's PORT -- that is, whether
// central already listens on its moved ports behind it. A deployment from
// before the front still has central itself on PORT, where nothing else can
// listen until central has been restarted once.
func CurrentFrontsCentral(genDir string) bool {
	if genDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(genDir, CurrentGeneration, strings.TrimPrefix(CentralFile, "proxy/")))
	return err == nil
}
