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
