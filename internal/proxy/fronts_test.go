package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A vps proxy in front of two auth replicas' APORT/MPORT, with its config
// in generations.
func frontedConfig(t *testing.T) Config {
	t.Helper()
	return Config{Mode: ModeExternal, AuthURL: mustURL(t, "https://id.example.com", ModeExternal),
		Auth: []Backend{
			{Service: "auth", Port: 8080, AdditionalPort: 9082, MutualTLSPort: 9083},
			{Service: "auth-2", Port: 8180, AdditionalPort: 9182, MutualTLSPort: 9183},
		},
		Edge:           []Backend{{Service: "edge", Port: 8095}},
		Fronts:         Fronts{Additional: 8082, MutualTLS: 8083},
		GenerationsDir: "/home/v/.versola/active/proxy-vps",
	}
}

func TestFilesWithFronts(t *testing.T) {
	files, err := frontedConfig(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	up := directives(files[UpstreamsFile])
	for _, w := range []string{"upstream auth_additional {", "server 127.0.0.1:9082 max_fails=3 fail_timeout=10s;", "server 127.0.0.1:9182 max_fails=3 fail_timeout=10s;"} {
		if !has(up, w) {
			t.Errorf("upstreams.conf lacks %q", w)
		}
	}
	internal := directives(files[InternalFile])
	for _, w := range []string{"access_log off;", "client_max_body_size 0;", "listen 127.0.0.1:8082;", "proxy_pass http://auth_additional;", "proxy_set_header Host $http_host;", `proxy_set_header Connection "";`} {
		if !has(internal, w) {
			t.Errorf("internal.conf lacks %q", w)
		}
	}
	stream := directives(files[StreamFile])
	for _, w := range []string{"upstream auth_mutual_tls {", "least_conn;", "server 127.0.0.1:9083 max_fails=3 fail_timeout=10s;", "server 127.0.0.1:9183 max_fails=3 fail_timeout=10s;", "listen 127.0.0.1:8083;", "proxy_pass auth_mutual_tls;"} {
		if !has(stream, w) {
			t.Errorf("auth-mtls.conf lacks %q", w)
		}
	}
	// TLS passes through: the proxy terminates none of it.
	if hasSub(stream, "ssl") {
		t.Error("the stream front must not terminate TLS")
	}
	nginxConf := directives(files["proxy/nginx.conf"])
	for _, w := range []string{"include conf.d/*.conf;", "stream {", "include stream.d/*.conf;"} {
		if !has(nginxConf, w) {
			t.Errorf("nginx.conf lacks %q", w)
		}
	}
	// The admin console is served from the current generation.
	if !has(directives(files["proxy/conf.d/versola.conf"]), "root /etc/nginx/versola/current/html;") {
		t.Error("versola.conf's admin console root is not the current generation's")
	}
	compose := string(files[ComposeFile])
	for _, w := range []string{
		`command: ["nginx", "-c", "/etc/nginx/versola/current/nginx.conf", "-g", "daemon off;"]`,
		`- "/home/v/.versola/active/proxy-vps:/etc/nginx/versola:ro"`,
		"network_mode: host",
	} {
		if !strings.Contains(compose, w) {
			t.Errorf("proxy.yml lacks %q:\n%s", w, compose)
		}
	}
	if !UsesGenerations(writeTemp(t, ComposeFile, files[ComposeFile])) {
		t.Error("UsesGenerations does not recognise the proxy.yml it renders")
	}
	if strings.Contains(compose, "./proxy/") || strings.Contains(compose, "./central-ui") {
		t.Errorf("proxy.yml still mounts bundle files:\n%s", compose)
	}
}

// With TLS the ACME volume is mounted beside the generations.
func TestFilesWithFrontsTLS(t *testing.T) {
	c := frontedConfig(t)
	c.Mode, c.AuthURL = ModeNginx, mustURL(t, "https://id.example.com", ModeNginx)
	c.ACMEDirectory, c.Resolvers = ACMEProduction, "127.0.0.53"
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	compose := string(files[ComposeFile])
	for _, w := range []string{`- "/home/v/.versola/active/proxy-vps:/etc/nginx/versola:ro"`, "- " + ACMEVolume + ":/var/cache/nginx/acme-letsencrypt"} {
		if !strings.Contains(compose, w) {
			t.Errorf("proxy.yml lacks %q:\n%s", w, compose)
		}
	}
}

// local: no fronts, no stream, the bundle's files mounted as before.
func TestFilesLocalUnchanged(t *testing.T) {
	c := withBackends(Config{Mode: ModeLocal, AuthURL: mustURL(t, "http://localhost:2821", ModeLocal)})
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[InternalFile]; ok {
		t.Error("local has no APORT front")
	}
	if _, ok := files[StreamFile]; ok {
		t.Error("local has no MPORT front")
	}
	if has(directives(files["proxy/nginx.conf"]), "stream {") {
		t.Error("local nginx.conf has a stream block")
	}
	if !has(directives(files["proxy/conf.d/versola.conf"]), "root /usr/share/nginx/html;") {
		t.Error("local admin console root changed")
	}
	if !strings.Contains(string(files[ComposeFile]), "./proxy/nginx.conf:/etc/nginx/nginx.conf:ro") {
		t.Error("local proxy.yml no longer mounts the bundle's files")
	}
}

func TestFrontsRejected(t *testing.T) {
	for name, mutate := range map[string]func(c *Config){
		"local with fronts":           func(c *Config) { c.Mode = ModeLocal; c.GenerationsDir = "" },
		"local with generations":      func(c *Config) { c.Mode = ModeLocal; c.Fronts = Fronts{} },
		"relative generations dir":    func(c *Config) { c.GenerationsDir = "proxy-vps" },
		"fronts on one port":          func(c *Config) { c.Fronts.MutualTLS = 8082 },
		"front out of range":          func(c *Config) { c.Fronts.Additional = 70000 },
		"replica without APORT":       func(c *Config) { c.Auth[1].AdditionalPort = 0 },
		"replica on the proxy's port": func(c *Config) { c.Auth[0].MutualTLSPort = 8083 },
		"same port twice":             func(c *Config) { c.Auth[1].AdditionalPort = 9082 },
	} {
		c := frontedConfig(t)
		c.Auth = append([]Backend(nil), c.Auth...)
		if name == "local with fronts" || name == "local with generations" {
			c.AuthURL = mustURL(t, "http://localhost:2821", ModeLocal)
		}
		mutate(&c)
		if _, err := c.Files(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// WriteUpstreams writes the fronts' files with the upstreams, and removes
// them when there are no fronts any more.
func TestWriteUpstreamsFronts(t *testing.T) {
	dir := t.TempDir()
	c := frontedConfig(t)
	if err := WriteUpstreams(dir, c); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{UpstreamsFile, InternalFile, StreamFile} {
		if !fileExists(dir, rel) {
			t.Errorf("%s not written", rel)
		}
	}
	c.Fronts = Fronts{}
	if err := WriteUpstreams(dir, c); err != nil {
		t.Fatal(err)
	}
	if fileExists(dir, InternalFile) || fileExists(dir, StreamFile) {
		t.Error("stale front files left")
	}
}

func TestWaitFronts(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	_, aport, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, mport, _ := net.SplitHostPort(ln.Addr().String())
	a, _ := strconv.Atoi(aport)
	m, _ := strconv.Atoi(mport)
	client := &http.Client{Timeout: time.Second}

	// Whatever auth answers is auth answering.
	if err := checkFronts(client, Fronts{Additional: a, MutualTLS: m}); err != nil {
		t.Errorf("404 from auth: %v", err)
	}
	// The proxy's own 502: nothing behind it.
	status = http.StatusBadGateway
	if err := checkFronts(client, Fronts{Additional: a, MutualTLS: m}); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("502: %v", err)
	}
	status = http.StatusOK
	ln.Close()
	if err := checkFronts(client, Fronts{Additional: a, MutualTLS: m}); err == nil || !strings.Contains(err.Error(), "MPORT") {
		t.Errorf("nothing on MPORT: %v", err)
	}
}

func writeTemp(t *testing.T, rel string, b []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A path with `$` reaches Compose as a literal: doubled.
func TestGenerationsMountEscapesDollar(t *testing.T) {
	c := frontedConfig(t)
	c.GenerationsDir = "/home/a$b/.versola/active/proxy-vps"
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files[ComposeFile]), `"/home/a$$b/.versola/active/proxy-vps:/etc/nginx/versola:ro"`) {
		t.Errorf("proxy.yml:\n%s", files[ComposeFile])
	}
}
