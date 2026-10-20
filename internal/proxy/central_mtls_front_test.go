package proxy

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// frontedConfig plus the proxy's fronts of central: its PORT, and its
// mutual-TLS listener on two replicas.
func centralMTLSConfig(t *testing.T) Config {
	t.Helper()
	return withCentralMTLS(t, frontedConfig(t))
}

func withCentralMTLS(t *testing.T, c Config) Config {
	t.Helper()
	// An absolute path on every OS the tests run on.
	c.GenerationsDir = t.TempDir()
	c.Fronts.Central = 8090
	c.Fronts.CentralMutualTLS = 8093
	c.Central = []Backend{
		{Service: "central", Port: 28090, MutualTLSPort: 28093},
		{Service: "central-2", Port: 28190, MutualTLSPort: 28193},
	}
	return c
}

func TestFilesWithCentralMutualTLSFront(t *testing.T) {
	files, err := centralMTLSConfig(t).Files()
	if err != nil {
		t.Fatal(err)
	}
	front := directives(files[CentralMutualTLSFile])
	for _, w := range []string{
		"upstream central_mutual_tls {", "least_conn;",
		"server 127.0.0.1:28093 max_fails=3 fail_timeout=10s;", "server 127.0.0.1:28193 max_fails=3 fail_timeout=10s;",
		"listen 8093;", "proxy_pass central_mutual_tls;",
	} {
		if !has(front, w) {
			t.Errorf("%s lacks %q:\n%s", CentralMutualTLSFile, w, files[CentralMutualTLSFile])
		}
	}
	// Bytes pass through: central terminates its own TLS.
	if hasSub(front, "ssl") {
		t.Error("the front of central's MPORT must not terminate TLS")
	}
	// Nothing of it in the PORT upstream, and the stream block that includes
	// the file exists.
	if up := string(files[UpstreamsFile]); strings.Contains(up, "28093") {
		t.Errorf("MPORT in the HTTP upstreams:\n%s", up)
	}
	if !has(directives(files["proxy/nginx.conf"]), "include stream.d/*.conf;") {
		t.Error("nginx.conf does not include stream.d")
	}
}

// A release whose central has no mutual-TLS listener gets no front for it:
// nothing is bound on 8093.
func TestNoCentralMutualTLSFrontWithoutTheListener(t *testing.T) {
	c := centralMTLSConfig(t)
	c.Fronts.CentralMutualTLS = 0
	c.Central = []Backend{{Service: "central", Port: 28090}}
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[CentralMutualTLSFile]; ok {
		t.Error("a front of central's MPORT without a listener")
	}
	for rel, b := range files {
		if strings.Contains(string(b), "8093") && rel != CentralStreamFile {
			t.Errorf("%s names 8093", rel)
		}
	}
}

func TestCentralMutualTLSFrontRejected(t *testing.T) {
	for name, mutate := range map[string]func(c *Config){
		"on the PORT front's port":       func(c *Config) { c.Fronts.CentralMutualTLS = 8090 },
		"on auth's MPORT front":          func(c *Config) { c.Fronts.CentralMutualTLS = 8083 },
		"out of range":                   func(c *Config) { c.Fronts.CentralMutualTLS = 70000 },
		"a replica without a listener":   func(c *Config) { c.Central[1].MutualTLSPort = 0 },
		"a replica on the proxy's port":  func(c *Config) { c.Central[0].MutualTLSPort = 8093 },
		"the same listener twice":        func(c *Config) { c.Central[1].MutualTLSPort = 28093 },
		"a listener without a front":     func(c *Config) { c.Fronts.CentralMutualTLS = 0 },
		"a front without the PORT front": func(c *Config) { c.Fronts.Central = 0; c.Central = nil },
		"a replica's MPORT out of range": func(c *Config) { c.Central[1].MutualTLSPort = 70000 },
	} {
		c := centralMTLSConfig(t)
		c.Central = append([]Backend(nil), c.Central...)
		mutate(&c)
		if _, err := c.Files(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// WriteUpstreams writes the front with the other dynamic files and removes
// it once the proxy has none.
func TestWriteUpstreamsCentralMutualTLSFront(t *testing.T) {
	dir := t.TempDir()
	c := centralMTLSConfig(t)
	if err := WriteUpstreams(dir, c); err != nil {
		t.Fatal(err)
	}
	if !fileExists(dir, CentralMutualTLSFile) {
		t.Fatalf("%s not written", CentralMutualTLSFile)
	}
	c.Fronts.CentralMutualTLS = 0
	c.Central = []Backend{{Service: "central", Port: 28090}}
	if err := WriteUpstreams(dir, c); err != nil {
		t.Fatal(err)
	}
	if fileExists(dir, CentralMutualTLSFile) {
		t.Error("a stale front of central's MPORT left")
	}
}

// The 443 name still reaches central through the relay, which now ends at
// the proxy's own front of 8093.
func TestCentralHostRelayEndsAtTheFront(t *testing.T) {
	c := withCentralMTLS(t, routedConfigTLS(t))
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if !has(directives(files[CentralStreamFile]), "proxy_pass 127.0.0.1:8093;") {
		t.Errorf("relay:\n%s", files[CentralStreamFile])
	}
	if !has(directives(files[CentralMutualTLSFile]), "listen 8093;") {
		t.Errorf("front:\n%s", files[CentralMutualTLSFile])
	}
}

func TestWaitFrontsCentralMutualTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	client := &http.Client{Timeout: time.Second}
	f := Fronts{CentralMutualTLS: port}
	if err := checkFronts(client, f); err != nil {
		t.Errorf("listening: %v", err)
	}
	ln.Close()
	if err := checkFronts(client, f); err == nil || !strings.Contains(err.Error(), "central's MPORT") {
		t.Errorf("nothing on the port: %v", err)
	}
}
