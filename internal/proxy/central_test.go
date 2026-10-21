package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseCentralURL(t *testing.T) {
	auth := mustURL(t, "https://id.example.com", ModeNginx)
	for _, tc := range []struct {
		name, raw string
		wantURL   string
		wantOK    bool
		wantErr   string
	}{
		// Nothing is derived any more: a local edge reaches central by
		// address, and publishing a name is --central-mtls-domain's decision.
		{name: "none", raw: "", wantOK: false},
		{name: "override", raw: "https://Edge-Sync.Example.com/", wantURL: "https://edge-sync.example.com", wantOK: true},
		{name: "own port", raw: "https://central.example.com:8093", wantURL: "https://central.example.com:8093", wantOK: true},
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
		})
	}
}

func TestParsePublicNames(t *testing.T) {
	auth := mustURL(t, "https://id.example.com", ModeNginx)
	p, err := ParsePublicNames("ID-MTLS.example.com", "central-mtls.example.com.", "central.example.com", auth)
	if err != nil {
		t.Fatal(err)
	}
	if want := (PublicNames{MTLS: "id-mtls.example.com", CentralMTLS: "central-mtls.example.com", Enroll: "central.example.com"}); p != want {
		t.Errorf("= %+v, want %+v", p, want)
	}
	if !p.Any() || !p.SharesHTTPS() {
		t.Error("names published, but Any/SharesHTTPS say otherwise")
	}
	// No flags: nothing is published, and nothing needs the stream block.
	none, err := ParsePublicNames("", "", "", auth)
	if err != nil || none.Any() || none.SharesHTTPS() || len(none.LoopbackPorts()) != 0 || len(none.Relays()) != 0 {
		t.Errorf("no names: %+v, %v", none, err)
	}
	// central.<domain> alone is plain https on the existing listener: no stream block.
	enroll, _ := ParsePublicNames("", "", "central.example.com", auth)
	if enroll.SharesHTTPS() || len(enroll.LoopbackPorts()) != 0 {
		t.Errorf("enroll alone shares 443: %+v", enroll)
	}
	if got := p.LoopbackPorts(); len(got) != 3 || got[0] != FrontPort || got[1] != AuthRelayPort || got[2] != CentralRelayPort {
		t.Errorf("LoopbackPorts = %v", got)
	}

	for _, tc := range []struct{ name, mtls, centralMTLS, enroll, auth, want string }{
		{name: "url", mtls: "https://id-mtls.example.com", want: "bare host name"},
		{name: "port", centralMTLS: "central-mtls.example.com:8443", want: "bare host name"},
		{name: "path", enroll: "central.example.com/edge", want: "bare host name"},
		{name: "ip", mtls: "203.0.113.7", want: "IP address"},
		{name: "one label", mtls: "central", want: "public domain"},
		{name: "localhost", enroll: "x.localhost", want: "public domain"},
		{name: "bad characters", mtls: "id_mtls.example.com", want: "letters, digits"},
		{name: "auth's name", mtls: "id.example.com", want: "already --auth-url's name"},
		{name: "same twice", mtls: "x.example.com", centralMTLS: "x.example.com", want: "already --mtls-domain's name"},
		{name: "http auth url", mtls: "id-mtls.example.com", auth: "http://id.example.com", want: "needs an https --auth-url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := auth
			if tc.auth != "" {
				a = mustURL(t, tc.auth, ModeNginx)
			}
			_, err := ParsePublicNames(tc.mtls, tc.centralMTLS, tc.enroll, a)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func routedConfigTLS(t *testing.T) Config {
	t.Helper()
	c := frontedConfig(t)
	c.Mode, c.AuthURL = ModeNginx, mustURL(t, "https://id.example.com", ModeNginx)
	c.ACMEDirectory, c.Resolvers, c.IPv6 = ACMEProduction, "127.0.0.53", true
	c.Public = PublicNames{MTLS: "id-mtls.example.com", CentralMTLS: "central-mtls.example.com", Enroll: "central.example.com"}
	return c
}

func TestFilesPublishesMutualTLSNames(t *testing.T) {
	files, err := routedConfigTLS(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	stream := directives(files[PublicStreamFile])
	for _, w := range []string{
		"map $ssl_preread_server_name $versola_public_upstream {",
		"hostnames;",
		"id-mtls.example.com 127.0.0.1:8446;",
		"central-mtls.example.com 127.0.0.1:8445;",
		"default 127.0.0.1:8444;",
		"listen 443;", "listen [::]:443;",
		"ssl_preread on;",
		"proxy_pass $versola_public_upstream;",
		"proxy_protocol on;",
		// Its own log, only for the mutual-TLS names.
		"access_log /dev/stdout versola_public if=$versola_public_logged;",
		// One public server per name: its own limit and log, forwarding to
		// the internal listener (auth's balanced front, central's own).
		"listen 127.0.0.1:8446 proxy_protocol;",
		"listen 127.0.0.1:8445 proxy_protocol;",
		"proxy_pass 127.0.0.1:8083;",
		"proxy_pass 127.0.0.1:8093;",
		"limit_conn_zone $proxy_protocol_addr zone=versola_public_conn:10m;",
		"limit_conn versola_public_conn 100;",
		"access_log /dev/stdout versola_public_auth;",
		"access_log /dev/stdout versola_public_central;",
	} {
		if !has(stream, w) {
			t.Errorf("public-mtls.conf lacks %q", w)
		}
	}
	if n := strings.Count(string(files[PublicStreamFile]), "limit_conn versola_public_conn"); n != 2 {
		t.Errorf("limit_conn appears %d times, want once per public server", n)
	}
	// Never terminates TLS: the handshake and the client certificate reach
	// the service as they are.
	if hasSub(stream, "ssl_certificate") || hasSub(stream, "listen 443 ssl") {
		t.Error("the stream server must not terminate TLS")
	}
	// The internal servers (auth's front on 127.0.0.1:8083) stay unlimited and unnamed: no SNI.
	internal := directives(files[StreamFile])
	if hasSub(internal, "limit_conn") || hasSub(internal, "ssl_preread") || !has(internal, "listen 127.0.0.1:8083;") {
		t.Errorf("auth-mtls.conf (the internal server) = %v", internal)
	}
	web := directives(files["proxy/conf.d/versola.conf"])
	for _, w := range []string{"listen 127.0.0.1:8444 ssl proxy_protocol;", "server_name id.example.com;", "set_real_ip_from 127.0.0.1;", "real_ip_header proxy_protocol;", "acme_certificate letsencrypt;"} {
		if !has(web, w) {
			t.Errorf("versola.conf lacks %q", w)
		}
	}
	// Port 80 -- the ACME HTTP-01 challenge -- is still the http server's, and
	// answers for the enrollment name too.
	if !has(web, "listen 80;") || has(web, "listen 443 ssl;") {
		t.Errorf("versola.conf's listeners are not 80 and the loopback one: %v", web)
	}
	if !has(web, "server_name id.example.com central.example.com;") {
		t.Errorf("port 80 does not answer for the enrollment name: %v", web)
	}
	if !has(directives(files["proxy/nginx.conf"]), "stream {") {
		t.Error("nginx.conf has no stream block")
	}
}

// central.<domain>: one route, one method, rate limited, to central's
// enrollment listener -- and nothing else answers there.
func TestFilesEnrollmentServer(t *testing.T) {
	files, err := routedConfigTLS(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	conf := string(files["proxy/conf.d/versola.conf"])
	i := strings.Index(conf, "server_name central.example.com;")
	if i < 0 {
		t.Fatal("no server for central.example.com")
	}
	web := directives([]byte(conf[i:]))
	for _, w := range []string{
		"acme_certificate letsencrypt;",
		"location = /edge/enroll {",
		"limit_except POST { deny all; }",
		"limit_req zone=versola_enroll burst=5 nodelay;",
		"client_max_body_size 64k;",
		"proxy_pass http://127.0.0.1:8094;",
		"proxy_set_header X-Forwarded-For $remote_addr;",
		"location / {", "return 404;",
	} {
		if !has(web, w) {
			t.Errorf("the enrollment server lacks %q", w)
		}
	}
	if !hasSub(directives([]byte(conf)), "limit_req_zone $binary_remote_addr zone=versola_enroll:1m rate=6r/m;") {
		t.Error("no limit_req_zone for the enrollment route")
	}
	// Only the one route reaches central: the auth routes are not on this name.
	if hasSub(web, "auth_backend") || hasSub(web, "edge_backend") {
		t.Error("the enrollment name serves Versola's routes")
	}
}

// Enrollment alone needs no stream block: the https server stays on 443.
func TestFilesEnrollmentOnly(t *testing.T) {
	c := routedConfigTLS(t)
	c.Public = PublicNames{Enroll: "central.example.com"}
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[PublicStreamFile]; ok {
		t.Error("a stream file for enrollment alone")
	}
	web := directives(files["proxy/conf.d/versola.conf"])
	if !has(web, "listen 443 ssl;") || hasSub(web, "proxy_protocol") || !has(web, "location = /edge/enroll {") {
		t.Errorf("versola.conf = %v", web)
	}
}

// Without a published name nothing changes: the https server owns 443, and
// there is no stream file or enrollment server to leave behind.
func TestFilesWithoutPublicNamesUnchanged(t *testing.T) {
	c := routedConfigTLS(t)
	c.Public = PublicNames{}
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[PublicStreamFile]; ok {
		t.Error("a public-mtls.conf without a published name")
	}
	web := directives(files["proxy/conf.d/versola.conf"])
	if !has(web, "listen 443 ssl;") || hasSub(web, "proxy_protocol") || hasSub(web, "/edge/enroll") || hasSub(web, "central.example.com") {
		t.Errorf("versola.conf = %v", web)
	}
}

func TestFilesRejectsPublicNames(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"auth's own name": func(c *Config) { c.Public.MTLS = "id.example.com" },
		"a name twice":    func(c *Config) { c.Public.Enroll = c.Public.CentralMTLS },
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

// Reconfiguring without a published name takes the stream file out of the
// bundle, or the next generation would carry a server on 443 beside
// versola.conf's own.
func TestWriteRemovesStalePublicStream(t *testing.T) {
	dir := t.TempDir()
	c := routedConfigTLS(t)
	if err := Write(dir, c); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PublicStreamFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	c.Public = PublicNames{}
	if err := Write(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale stream file: %v", err)
	}
}

// The generation `up` and `replica` build from the bundle carries the stream
// file, though their own config knows nothing of it.
func TestGenerationCarriesPublicStream(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(genDir, gen, "stream.d", "public-mtls.conf")); err != nil {
		t.Error(err)
	}
}

// What nginx itself says: the stream module with ssl_preread, PROXY
// protocol listeners in both stream and http, per-address limits, the
// variable proxy_pass and the enrollment server.
func TestNginxAcceptsPublicNames(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	if runtime.GOOS == "windows" {
		t.Skip("bind-mounting temp dirs is unreliable on Docker Desktop for Windows")
	}
	for name, public := range map[string]PublicNames{
		"all":         routedConfigTLS(t).Public,
		"auth only":   {MTLS: "id-mtls.example.com"},
		"enroll only": {Enroll: "central.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := routedConfigTLS(t)
			c.Public = public
			if err := Write(dir, c); err != nil {
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
		})
	}
}

// frontedConfig plus central behind a front of its own (vps).
func centralFrontedConfig(t *testing.T) Config {
	t.Helper()
	c := frontedConfig(t)
	c.Central = []Backend{{Service: "central", Port: 28090}, {Service: "central-2", Port: 28190}}
	c.Fronts.Central = 8090
	return c
}

func TestFilesWithCentralFront(t *testing.T) {
	files, err := centralFrontedConfig(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	up := directives(files[UpstreamsFile])
	for _, w := range []string{"upstream central_backend {", "server 127.0.0.1:28090 max_fails=3 fail_timeout=10s;", "server 127.0.0.1:28190 max_fails=3 fail_timeout=10s;"} {
		if !has(up, w) {
			t.Errorf("upstreams.conf lacks %q", w)
		}
	}
	central := directives(files[CentralFile])
	for _, w := range []string{"listen 127.0.0.1:8090;", "access_log off;", "client_max_body_size 0;", "proxy_pass http://central_backend;", "proxy_http_version 1.1;", "proxy_set_header Host $http_host;", `proxy_set_header Connection "";`} {
		if !has(central, w) {
			t.Errorf("central.conf lacks %q", w)
		}
	}
	// Loopback only: central's admin API must not be reachable from outside.
	for _, l := range central {
		if strings.HasPrefix(l, "listen ") && !strings.HasPrefix(l, "listen 127.0.0.1:") {
			t.Errorf("central front listens beyond loopback: %q", l)
		}
	}
}

// Without a central front the files are what they were: no central upstream,
// no central.conf.
func TestFilesWithoutCentralFront(t *testing.T) {
	files, err := frontedConfig(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[CentralFile]; ok {
		t.Error("central.conf rendered without a central front")
	}
	if hasSub(directives(files[UpstreamsFile]), "central_backend") {
		t.Error("central upstream rendered without a central front")
	}
}

func TestCentralFrontRejected(t *testing.T) {
	for name, mutate := range map[string]func(c *Config){
		"front without replicas": func(c *Config) { c.Central = nil },
		"replicas without front": func(c *Config) { c.Fronts.Central = 0 },
		"front out of range":     func(c *Config) { c.Fronts.Central = 70000 },
		"front on an auth front": func(c *Config) { c.Fronts.Central = c.Fronts.Additional },
		"replica on the front":   func(c *Config) { c.Central[0].Port = 8090 },
		"replica without a port": func(c *Config) { c.Central[0].Port = 0 },
		"same port twice":        func(c *Config) { c.Central[1].Port = c.Central[0].Port },
	} {
		c := centralFrontedConfig(t)
		c.Central = append([]Backend(nil), c.Central...)
		mutate(&c)
		if _, err := c.Files(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	local := centralFrontedConfig(t)
	local.Mode = ModeLocal
	local.GenerationsDir = ""
	local.Fronts = Fronts{Central: 8090}
	local.AuthURL = mustURL(t, "http://localhost:2821", ModeLocal)
	if _, err := local.Files(); err == nil {
		t.Error("a central front on local: want an error")
	}
}

func TestWriteUpstreamsCentralFront(t *testing.T) {
	dir := t.TempDir()
	c := centralFrontedConfig(t)
	if err := WriteUpstreams(dir, c); err != nil {
		t.Fatal(err)
	}
	if !fileExists(dir, CentralFile) {
		t.Fatal("central.conf not written")
	}
	c.Central, c.Fronts.Central = nil, 0
	if err := WriteUpstreams(dir, c); err != nil {
		t.Fatal(err)
	}
	if fileExists(dir, CentralFile) {
		t.Error("stale central.conf left")
	}
	if _, err := os.Stat(filepath.Join(dir, CentralFile)); !os.IsNotExist(err) {
		t.Errorf("stat: %v", err)
	}
}

func TestWaitCentralFront(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(p)
	client := &http.Client{Timeout: time.Second}

	// Whatever central answers is central answering.
	if err := checkFronts(client, Fronts{Central: port}); err != nil {
		t.Errorf("404 from central: %v", err)
	}
	// The proxy's own 502/504: no replica behind the front yet.
	for _, s := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		status = s
		err := checkFronts(client, Fronts{Central: port})
		if err == nil || !strings.Contains(err.Error(), "central") || !strings.Contains(err.Error(), strconv.Itoa(s)) {
			t.Errorf("%d: %v", s, err)
		}
	}
	// WaitCentralFront polls until it is served, and gives up at the timeout.
	status = http.StatusOK
	if err := WaitCentralFront(port, 5*time.Second); err != nil {
		t.Errorf("served: %v", err)
	}
	status = http.StatusBadGateway
	if err := WaitCentralFront(port, 300*time.Millisecond); err == nil {
		t.Error("502 for the whole timeout: want an error")
	}
	srv.Close()
	if err := checkFronts(client, Fronts{Central: port}); err == nil {
		t.Error("nothing listening: want an error")
	}
}
