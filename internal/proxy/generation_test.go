package proxy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fileExists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, rel))
	return err == nil
}

// bundleFor writes what configure leaves in a bundle for c: the proxy's
// files and an admin console.
func bundleFor(t *testing.T, c Config) string {
	t.Helper()
	dir := t.TempDir()
	if err := Write(dir, c); err != nil {
		t.Fatal(err)
	}
	ui := filepath.Join(dir, "central-ui", "assets")
	if err := os.MkdirAll(ui, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"central-ui/index.html": "<html>", "central-ui/assets/app.js": "app"} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeDocker replaces the docker calls; validate decides `nginx -t` in the
// throwaway container, exec the ones in the running proxy.
type fakeDocker struct {
	calls    []string
	validate error
	test     error
	reload   error
}

func (f *fakeDocker) install(t *testing.T) {
	oldOut, oldRun := dockerOutput, dockerRun
	t.Cleanup(func() { dockerOutput, dockerRun = oldOut, oldRun })
	dockerOutput = func(args ...string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if args[0] == "run" {
			return nil, f.validate
		}
		return nil, f.test
	}
	dockerRun = func(args ...string) error {
		f.calls = append(f.calls, strings.Join(args, " "))
		return f.reload
	}
}

func generationsConfig(t *testing.T) (Config, string) {
	c := frontedConfig(t)
	c.GenerationsDir = filepath.Join(t.TempDir(), "proxy-vps")
	return c, bundleFor(t, c)
}

func TestApplyBuildsChecksAndSwitches(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)

	prev, err := Apply(bundle, c)
	if err != nil {
		t.Fatal(err)
	}
	if prev != "" {
		t.Errorf("first generation: previous %q", prev)
	}
	cur, err := currentGeneration(c.GenerationsDir)
	if err != nil || cur != "gen-000001" {
		t.Fatalf("current %q %v", cur, err)
	}
	gen := filepath.Join(c.GenerationsDir, cur)
	for _, rel := range []string{"nginx.conf", "proxy_params.conf", "conf.d/versola.conf", "conf.d/upstreams.conf", "conf.d/internal.conf", "stream.d/auth-mtls.conf", "html/central/admin/index.html", "html/central/admin/assets/app.js"} {
		if !fileExists(gen, rel) {
			t.Errorf("generation lacks %s", rel)
		}
	}
	// Readable by nginx's workers, another user.
	if st, err := os.Stat(filepath.Join(gen, "html/central/admin/index.html")); err == nil && runtime.GOOS != "windows" && st.Mode().Perm()&0o004 == 0 {
		t.Errorf("admin console not world-readable: %v", st.Mode())
	}
	// Checked in a throwaway container, against that generation.
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "run --rm") ||
		!strings.Contains(f.calls[0], "--network none") ||
		!strings.Contains(f.calls[0], "type=bind,src="+c.GenerationsDir+",dst=/etc/nginx/versola,readonly") ||
		!strings.Contains(f.calls[0], "nginx -t -c /etc/nginx/versola/gen-000001/nginx.conf") {
		t.Errorf("validation: %v", f.calls)
	}

	// The next one: previous is returned, and only those two are kept.
	c.Auth = c.Auth[:1]
	if prev, err = Apply(bundle, c); err != nil || prev != "gen-000001" {
		t.Fatalf("second: %q %v", prev, err)
	}
	if prev, err = Apply(bundle, c); err != nil || prev != "gen-000002" {
		t.Fatalf("third: %q %v", prev, err)
	}
	if fileExists(c.GenerationsDir, "gen-000001") || !fileExists(c.GenerationsDir, "gen-000002") || !fileExists(c.GenerationsDir, "gen-000003") {
		t.Error("want gen-000002 and gen-000003 kept, gen-000001 pruned")
	}
	b, _ := os.ReadFile(filepath.Join(c.GenerationsDir, "current", "stream.d", "auth-mtls.conf"))
	if strings.Contains(string(b), "9183") {
		t.Error("current still routes to the removed replica")
	}
}

// A restrictive umask does not leave the generation unreadable to nginx.
func TestGenerationReadableUnderUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	old := setUmask(0o077)
	defer setUmask(old)
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	if _, err := Apply(bundle, c); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"", "gen-000001", "gen-000001/html/central/admin", "gen-000001/html/central/admin/index.html", "gen-000001/conf.d/versola.conf"} {
		st, err := os.Stat(filepath.Join(c.GenerationsDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o005 == 0 && st.IsDir() || st.Mode().Perm()&0o004 == 0 {
			t.Errorf("%q: mode %v", rel, st.Mode().Perm())
		}
	}
}

// A config nginx refuses changes nothing: `current` stays, the rejected
// generation is gone, and nginx's words are in the error.
func TestApplyRefusedChangesNothing(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	if _, err := Apply(bundle, c); err != nil {
		t.Fatal(err)
	}
	f.validate = &exec.ExitError{Stderr: []byte(`nginx: [emerg] unknown directive "bad"`)}
	_, err := Apply(bundle, c)
	if err == nil || !strings.Contains(err.Error(), `unknown directive "bad"`) {
		t.Fatalf("err = %v", err)
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "gen-000001" {
		t.Errorf("current moved to %q", cur)
	}
	if fileExists(c.GenerationsDir, "gen-000002") {
		t.Error("the refused generation was kept")
	}
}

// A build cut off halfway leaves a .tmp directory: never current, and
// cleared by the next build.
func TestBuildClearsALeftoverBuild(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	left := filepath.Join(c.GenerationsDir, "gen-000004"+buildSuffix)
	if err := os.MkdirAll(left, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(bundle, c); err != nil {
		t.Fatal(err)
	}
	if fileExists(c.GenerationsDir, "gen-000004"+buildSuffix) {
		t.Error("leftover build kept")
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "gen-000001" {
		t.Errorf("current %q: a .tmp directory does not count", cur)
	}
}

// Without the admin console the generation can't be built, and nothing
// switches.
func TestBuildNeedsTheAdminConsole(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	if err := os.RemoveAll(filepath.Join(bundle, "central-ui")); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(bundle, c); err == nil {
		t.Fatal("want an error")
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "" {
		t.Errorf("current %q", cur)
	}
}

// Reload on vps: Apply, then the running proxy checks and reloads
// `current`; if it refuses, `current` goes back.
func TestReloadGenerations(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	if _, err := Apply(bundle, c); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := Reload(bundle, c); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec versola-proxy nginx -c /etc/nginx/versola/current/nginx.conf -t", "exec versola-proxy nginx -c /etc/nginx/versola/current/nginx.conf -s reload"}
	if len(f.calls) != 3 || f.calls[1] != want[0] || f.calls[2] != want[1] {
		t.Errorf("calls: %v", f.calls)
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "gen-000002" {
		t.Errorf("current %q", cur)
	}

	f.reload = errors.New("exec failed")
	if err := Reload(bundle, c); err == nil {
		t.Fatal("want an error")
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "gen-000002" {
		t.Errorf("a refused reload must put current back, got %q", cur)
	}
}

// Stage: the config is ready for the next start, nothing is reloaded.
func TestStage(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	if err := Stage(bundle, c); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "exec") {
			t.Errorf("staging touched the running proxy: %v", f.calls)
		}
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "gen-000001" {
		t.Errorf("current %q", cur)
	}
}

func TestSwitchGenerationRefusesOtherNames(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "../etc", "gen-", "gen-1.tmp", "current"} {
		if err := switchGeneration(dir, bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// TestNginxAcceptsGenerations runs `nginx -t` in the real proxy image on a
// generation built from a real bundle, mounted and started the way
// proxy.yml does on vps. Needs Docker.
func TestNginxAcceptsGenerations(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	if runtime.GOOS == "windows" {
		t.Skip("vps only")
	}
	for name, c := range map[string]Config{
		"external": frontedConfig(t),
		"nginx with TLS": func() Config {
			c := frontedConfig(t)
			c.Mode, c.AuthURL = ModeNginx, mustURL(t, "https://id.example.com", ModeNginx)
			c.ACMEDirectory, c.Resolvers = ACMEProduction, "127.0.0.53"
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			c.GenerationsDir = filepath.Join(t.TempDir(), "proxy-vps")
			bundle := bundleFor(t, c)
			// The real thing: validateGeneration runs nginx -t in the image.
			if _, err := Apply(bundle, c); err != nil {
				t.Fatalf("nginx refused the generation: %v", err)
			}
			// And through `current`, as the running proxy reads it.
			out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "sh",
				"--tmpfs", "/var/cache/nginx/acme-letsencrypt",
				"-v", c.GenerationsDir+":"+GenerationMount+":ro", Image,
				"-c", "mkdir -p /var/cache/nginx/acme-letsencrypt/production && exec nginx -t -c "+confPath(CurrentGeneration)).CombinedOutput()
			if err != nil {
				t.Fatalf("nginx -t through current: %v\n%s", err, out)
			}
		})
	}
}

// docker run failing on its own (exit 125: no image, no daemon) is not
// nginx rejecting the config, and says so; nothing changes either way.
func TestValidateRunFailure(t *testing.T) {
	f := &fakeDocker{}
	f.install(t)
	c, bundle := generationsConfig(t)
	cmd := exec.Command("sh", "-c", "echo 'Unable to find image' >&2; exit 125")
	_, runErr := cmd.Output()
	f.validate = runErr
	_, err := Apply(bundle, c)
	if err == nil || !strings.Contains(err.Error(), "couldn't run "+Image) || strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err = %v", err)
	}
	if cur, _ := currentGeneration(c.GenerationsDir); cur != "" {
		t.Errorf("current %q", cur)
	}
}

// A bundle written by a CLI built from a Windows checkout has CRLF line endings.
func TestUsesGenerationsAcceptsCRLF(t *testing.T) {
	for name, nl := range map[string]string{"LF": "\n", "CRLF": "\r\n"} {
		dir := t.TempDir()
		yml := "services:" + nl + "  proxy:" + nl + "    volumes:" + nl + "      - type: bind" + nl + "        target: " + GenerationMount + nl
		if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		if !UsesGenerations(dir) {
			t.Errorf("%s: generations not recognised", name)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte("services:\r\n  proxy:\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if UsesGenerations(dir) {
		t.Error("a bundle without the mount is not generations")
	}
}
