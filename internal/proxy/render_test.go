package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw, mode string) AuthURL {
	t.Helper()
	a, err := ParseAuthURL(raw, mode)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// directives returns the non-comment lines of a config, trimmed -- so
// assertions are about what nginx reads, not about explanatory comments.
func directives(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func hasSub(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

type renderCase struct {
	name      string
	cfg       func(t *testing.T) Config
	want      []string // exact directive lines in conf.d/versola.conf
	wantNot   []string // substrings that must not appear in any directive
	compose   []string // substrings proxy.yml must contain
	noCompose []string
}

func renderCases() []renderCase {
	return []renderCase{
		{
			name: "nginx mode, TLS, ipv4 only",
			cfg: func(t *testing.T) Config {
				return Config{Mode: ModeNginx, AuthURL: mustURL(t, "https://id.example.com", ModeNginx),
					ACMEDirectory: ACMEProduction, Resolvers: "127.0.0.53"}
			},
			want: []string{"listen 443 ssl;", "listen 80;", "server_name id.example.com;", "acme_certificate letsencrypt;",
				"state_path /var/cache/nginx/acme-letsencrypt/production;",
				"ssl_certificate $acme_certificate;", "resolver 127.0.0.53 ipv6=off;",
				"uri " + ACMEProduction + ";", "return 301 https://$host$request_uri;", "server 127.0.0.1:8080;"},
			wantNot: []string{"[::]", "set_real_ip_from", "2821"},
			compose: []string{"image: " + Image, "container_name: " + ContainerName, "name: versola-vps", ACMEVolume + ":/var/cache/nginx/acme-letsencrypt", "external: true", "./central-ui:/usr/share/nginx/html/central/admin:ro"},
		},
		{
			name: "nginx mode, TLS, ipv6, staging",
			cfg: func(t *testing.T) Config {
				return Config{Mode: ModeNginx, AuthURL: mustURL(t, "https://id.example.com", ModeNginx),
					ACMEDirectory: ACMEStaging, Resolvers: "127.0.0.53", IPv6: true}
			},
			want:    []string{"listen [::]:443 ssl;", "listen [::]:80;", "resolver 127.0.0.53;", "uri " + ACMEStaging + ";", "state_path /var/cache/nginx/acme-letsencrypt/staging;"},
			wantNot: []string{"ipv6=off"},
		},
		{
			name: "nginx mode, plain http",
			cfg: func(t *testing.T) Config {
				return Config{Mode: ModeNginx, AuthURL: mustURL(t, "http://1.2.3.4", ModeNginx), IPv6: true}
			},
			want:      []string{"listen 80;", "listen [::]:80;", "server_name 1.2.3.4;"},
			wantNot:   []string{"ssl", "acme", "resolver", "set_real_ip_from"},
			noCompose: []string{ACMEVolume},
		},
		{
			name: "external mode",
			cfg: func(t *testing.T) Config {
				// IPv6 must be ignored here: the proxy only listens on loopback.
				return Config{Mode: ModeExternal, AuthURL: mustURL(t, "https://id.example.com", ModeExternal), IPv6: true}
			},
			want: []string{"listen 127.0.0.1:2821;", "set_real_ip_from 127.0.0.1;", "real_ip_header X-Forwarded-For;",
				"map $http_x_forwarded_proto $versola_forwarded_proto {"},
			wantNot:   []string{"ssl", "acme", "resolver", "[::]", "listen 80", "listen 443"},
			noCompose: []string{ACMEVolume},
		},
	}
}

func TestFiles(t *testing.T) {
	for _, c := range renderCases() {
		t.Run(c.name, func(t *testing.T) {
			files, err := c.cfg(t).Files()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{ComposeFile, "proxy/nginx.conf", "proxy/proxy_params.conf", "proxy/conf.d/versola.conf"} {
				if len(files[name]) == 0 {
					t.Fatalf("missing %s", name)
				}
			}
			conf := directives(files["proxy/conf.d/versola.conf"])
			for _, w := range c.want {
				if !has(conf, w) {
					t.Errorf("versola.conf lacks %q", w)
				}
			}
			for _, w := range c.wantNot {
				if hasSub(conf, w) {
					t.Errorf("versola.conf unexpectedly contains %q", w)
				}
			}
			compose := string(files[ComposeFile])
			for _, w := range c.compose {
				if !strings.Contains(compose, w) {
					t.Errorf("proxy.yml lacks %q", w)
				}
			}
			for _, w := range c.noCompose {
				if strings.Contains(compose, w) {
					t.Errorf("proxy.yml unexpectedly contains %q", w)
				}
			}
			// Every mode: nginx's own redirects (/admin -> /central/admin/)
			// must stay relative, or behind an external proxy they'd point
			// at http://<host>:2821.
			if !has(conf, "absolute_redirect off;") {
				t.Error("versola.conf lacks absolute_redirect off;")
			}
			// proxy_params.conf uses these; they must always be defined, in
			// every mode, or nginx refuses the config.
			if !has(conf, "map $http_host $versola_host {") || !hasSub(conf, "$versola_forwarded_proto {") {
				t.Error("versola.conf lacks the $versola_host / $versola_forwarded_proto maps")
			}
			if c.name != "external mode" && hasSub(conf, "$http_x_forwarded_proto") {
				t.Error("a client-sent X-Forwarded-Proto must only be trusted in external mode")
			}
			params := directives(files["proxy/proxy_params.conf"])
			if !has(params, "proxy_set_header Host $versola_host;") || !has(params, "proxy_set_header X-Forwarded-Proto $versola_forwarded_proto;") {
				t.Error("proxy_params.conf doesn't use $versola_host / $versola_forwarded_proto")
			}
			if !has(directives(files["proxy/nginx.conf"]), "load_module modules/ngx_http_acme_module.so;") {
				t.Error("nginx.conf doesn't load the ACME module")
			}
			// Directives nginx refuses to see twice in one server block
			// ("directive is duplicate") -- easy to reintroduce when the
			// wrapper template and the copied routes both set one.
			for _, once := range []string{"client_max_body_size", "server_name", "http2", "acme_certificate", "ssl_certificate ", "set_real_ip_from", "real_ip_header"} {
				n := 0
				for _, l := range conf {
					if strings.HasPrefix(l, once) {
						n++
					}
				}
				limit := 1
				if once == "server_name" && hasSub(conf, "acme_issuer") {
					limit = 2 // the port-80 redirect server has its own
				}
				if n > limit {
					t.Errorf("%q appears %d times in versola.conf", once, n)
				}
			}
			if !hasSub(conf, "location /central/admin/") || !hasSub(conf, "proxy_pass http://edge_backend") {
				t.Error("versola.conf is missing the routes")
			}
		})
	}
}

func TestFilesRejectsBadConfig(t *testing.T) {
	tlsURL := mustURL(t, "https://id.example.com", ModeNginx)
	for name, c := range map[string]Config{
		"bad mode":         {Mode: "caddy", AuthURL: tlsURL},
		"tls, no resolver": {Mode: ModeNginx, AuthURL: tlsURL, ACMEDirectory: ACMEProduction},
		"tls, bad acme":    {Mode: ModeNginx, AuthURL: tlsURL, ACMEDirectory: "https://evil.example/dir", Resolvers: "1.1.1.1"},
	} {
		if _, err := c.Files(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// TestNginxAcceptsConfig runs `nginx -t` inside the real proxy image on
// every rendered variant, laid out exactly as proxy.yml mounts it. Needs
// Docker; skipped without it (and with -short).
func TestNginxAcceptsConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	if runtime.GOOS == "windows" {
		t.Skip("bind-mounting temp dirs is unreliable on Docker Desktop for Windows")
	}
	for _, c := range renderCases() {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := Write(dir, c.cfg(t)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "central-ui"), 0o755); err != nil {
				t.Fatal(err)
			}
			// The ACME state dir is a tmpfs inside the container, not a
			// host dir: the ACME module already writes its account key
			// there during `nginx -t`, as root, which a host-side test
			// cleanup (running as the CI user) couldn't delete. Laid out
			// the way configure prepares the real volume (setUpProxy): the
			// module only creates the last element of state_path itself,
			// so the per-directory subdirs must already exist.
			stateDir := "/var/cache/nginx/acme-letsencrypt"
			prepare := "mkdir -p " + stateDir + "/" + acmeStateDir(ACMEProduction) + " " + stateDir + "/" + acmeStateDir(ACMEStaging)
			args := []string{"run", "--rm", "--entrypoint", "sh",
				"--tmpfs", stateDir,
				"-v", filepath.Join(dir, "proxy/nginx.conf") + ":/etc/nginx/nginx.conf:ro",
				"-v", filepath.Join(dir, "proxy/conf.d") + ":/etc/nginx/conf.d:ro",
				"-v", filepath.Join(dir, "proxy/proxy_params.conf") + ":/etc/nginx/proxy_params.conf:ro",
				Image, "-c", prepare + " && exec nginx -t"}
			out, err := exec.Command("docker", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("nginx -t failed: %v\n%s", err, out)
			}
		})
	}
}
