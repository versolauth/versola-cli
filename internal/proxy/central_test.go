package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeriveCentralURL(t *testing.T) {
	for auth, want := range map[string]string{
		"https://id.example.com":      "https://central.example.com",
		"https://example.com":         "https://central.example.com",
		"https://auth.versola.kz":     "https://central.versola.kz",
		"https://id.eu.example.com":   "https://central.eu.example.com",
		"https://id.example.com:8443": "https://central.example.com",
		"http://id.example.com":       "", // central's listener is mutual TLS, https only
		"https://1.2.3.4":             "",
		"https://intranet":            "",
		"http://localhost:2821":       "",
	} {
		a, err := ParseAuthURL(auth, ModeExternal)
		if err != nil {
			t.Fatal(err)
		}
		if got := DeriveCentralURL(a); got != want {
			t.Errorf("DeriveCentralURL(%s) = %q, want %q", auth, got, want)
		}
	}
}

func TestParseCentralURL(t *testing.T) {
	auth := mustURL(t, "https://id.example.com", ModeNginx)
	for _, tc := range []struct {
		name, raw string
		wantURL   string
		wantOK    bool
		wantHost  string // RoutedHost in nginx mode
		wantErr   string
	}{
		{name: "derived", raw: "", wantURL: "https://central.example.com", wantOK: true, wantHost: "central.example.com"},
		{name: "override", raw: "https://Edge-Sync.Example.com/", wantURL: "https://edge-sync.example.com", wantOK: true, wantHost: "edge-sync.example.com"},
		{name: "own port: the proxy is not in the way", raw: "https://central.example.com:8093", wantURL: "https://central.example.com:8093", wantOK: true},
		{name: "the host and the port 8093", raw: "https://203.0.113.7:8093", wantURL: "https://203.0.113.7:8093", wantOK: true},
		{name: "http", raw: "http://central.example.com", wantErr: "must be https"},
		{name: "path", raw: "https://central.example.com/edge", wantErr: "no path"},
		{name: "default port", raw: "https://central.example.com:443", wantErr: "remove the default port"},
		{name: "auth's own name", raw: "https://id.example.com", wantErr: "same host as --auth-url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok, err := ParseCentralURL(tc.raw, auth)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || ok != tc.wantOK || c.URL != tc.wantURL {
				t.Fatalf("= %+v, %v, %v; want %q", c, ok, err, tc.wantURL)
			}
			if got := c.RoutedHost(auth, ModeNginx); got != tc.wantHost {
				t.Errorf("RoutedHost = %q, want %q", got, tc.wantHost)
			}
			// Behind --proxy external the operator's own server routes it.
			if got := c.RoutedHost(mustURL(t, "https://id.example.com", ModeExternal), ModeExternal); got != "" {
				t.Errorf("RoutedHost in external mode = %q, want none", got)
			}
		})
	}
	if _, ok, err := ParseCentralURL("", mustURL(t, "http://1.2.3.4", ModeNginx)); ok || err != nil {
		t.Errorf("no https domain: = %v, %v; want nothing derived", ok, err)
	}
}

func routedConfigTLS(t *testing.T) Config {
	t.Helper()
	c := frontedConfig(t)
	c.Mode, c.AuthURL = ModeNginx, mustURL(t, "https://id.example.com", ModeNginx)
	c.ACMEDirectory, c.Resolvers, c.IPv6 = ACMEProduction, "127.0.0.53", true
	c.CentralHost = "central.example.com"
	return c
}

func TestFilesRoutesCentral(t *testing.T) {
	files, err := routedConfigTLS(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	stream := directives(files[CentralStreamFile])
	for _, w := range []string{
		"map $ssl_preread_server_name $versola_public_upstream {",
		"hostnames;",
		"central.example.com 127.0.0.1:8445;",
		"default 127.0.0.1:8444;",
		"listen 443;", "listen [::]:443;",
		"ssl_preread on;",
		"proxy_pass $versola_public_upstream;",
		"proxy_protocol on;",
		"listen 127.0.0.1:8445 proxy_protocol;",
		"proxy_pass 127.0.0.1:8093;",
	} {
		if !has(stream, w) {
			t.Errorf("central-mtls.conf lacks %q", w)
		}
	}
	// Never terminates TLS: the handshake and the client certificate reach
	// central as they are.
	if hasSub(stream, "ssl_certificate") || hasSub(stream, "listen 443 ssl") {
		t.Error("the stream server must not terminate TLS")
	}
	web := directives(files["proxy/conf.d/versola.conf"])
	for _, w := range []string{"listen 127.0.0.1:8444 ssl proxy_protocol;", "server_name id.example.com;", "set_real_ip_from 127.0.0.1;", "real_ip_header proxy_protocol;", "acme_certificate letsencrypt;"} {
		if !has(web, w) {
			t.Errorf("versola.conf lacks %q", w)
		}
	}
	// Port 80 -- the ACME HTTP-01 challenge -- is still the http server's.
	if !has(web, "listen 80;") || has(web, "listen 443 ssl;") {
		t.Errorf("versola.conf's listeners are not 80 and the loopback one: %v", web)
	}
	if !has(directives(files["proxy/nginx.conf"]), "stream {") {
		t.Error("nginx.conf has no stream block")
	}
}

// Without a routed name nothing changes: the https server owns 443, and
// there is no stream file to leave behind.
func TestFilesWithoutCentralUnchanged(t *testing.T) {
	c := routedConfigTLS(t)
	c.CentralHost = ""
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[CentralStreamFile]; ok {
		t.Error("a central-mtls.conf without a routed name")
	}
	web := directives(files["proxy/conf.d/versola.conf"])
	if !has(web, "listen 443 ssl;") || hasSub(web, "proxy_protocol") {
		t.Errorf("versola.conf = %v", web)
	}
}

func TestFilesRejectsCentralHost(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"auth's own name": func(c *Config) { c.CentralHost = "id.example.com" },
		"no TLS of its own": func(c *Config) {
			c.Mode, c.AuthURL = ModeExternal, mustURL(t, "https://id.example.com", ModeExternal)
		},
		"no stream block (local)": func(c *Config) { c.Fronts, c.GenerationsDir = Fronts{}, "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := routedConfigTLS(t)
			mutate(&c)
			if _, err := c.Files(); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// Reconfiguring without a routed name takes the stream file out of the
// bundle, or the next generation would carry a server on 443 beside
// versola.conf's own.
func TestWriteRemovesStaleCentralStream(t *testing.T) {
	dir := t.TempDir()
	c := routedConfigTLS(t)
	if err := Write(dir, c); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CentralStreamFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	c.CentralHost = ""
	if err := Write(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale stream file: %v", err)
	}
}

// The generation `up` and `replica` build from the bundle carries the stream
// file, though their own config knows nothing of it.
func TestGenerationCarriesCentralStream(t *testing.T) {
	bundle, genDir := t.TempDir(), t.TempDir()
	c := routedConfigTLS(t)
	if err := Write(bundle, c); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "central-ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	gen, err := buildGeneration(genDir, bundle, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(genDir, gen, "stream.d", "central-mtls.conf")); err != nil {
		t.Error(err)
	}
}

// What nginx itself says: the stream module with ssl_preread, a PROXY
// protocol listener in both stream and http, and the variable proxy_pass.
func TestNginxAcceptsCentralRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	if runtime.GOOS == "windows" {
		t.Skip("bind-mounting temp dirs is unreliable on Docker Desktop for Windows")
	}
	dir := t.TempDir()
	if err := Write(dir, routedConfigTLS(t)); err != nil {
		t.Fatal(err)
	}
	stateDir := "/var/cache/nginx/acme-letsencrypt"
	prepare := "mkdir -p " + stateDir + "/production && exec nginx -t"
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "sh", "--tmpfs", stateDir,
		"-v", filepath.Join(dir, "proxy/nginx.conf")+":/etc/nginx/nginx.conf:ro",
		"-v", filepath.Join(dir, "proxy/conf.d")+":/etc/nginx/conf.d:ro",
		"-v", filepath.Join(dir, "proxy/stream.d")+":/etc/nginx/stream.d:ro",
		"-v", filepath.Join(dir, "proxy/proxy_params.conf")+":/etc/nginx/proxy_params.conf:ro",
		Image, "-c", prepare).CombinedOutput()
	if err != nil {
		t.Fatalf("nginx -t failed: %v\n%s", err, out)
	}
}
