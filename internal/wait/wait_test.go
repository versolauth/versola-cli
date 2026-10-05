package wait

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lanIP is an address of this machine that is not loopback, or "".
func lanIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			return n.IP.String()
		}
	}
	return ""
}

// The probes are always to this host: a proxy from the environment must
// not get them. Go skips it for loopback only, so the probe is to another
// address of this machine, with HTTP_PROXY pointing at a counting server.
func TestProbesBypassTheEnvironmentProxy(t *testing.T) {
	ip := lanIP()
	if ip == "" {
		t.Skip("no non-loopback address on this machine")
	}
	var viaProxy, direct atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { viaProxy.Add(1) }))
	defer proxySrv.Close()
	t.Setenv("HTTP_PROXY", proxySrv.URL)
	t.Setenv("http_proxy", proxySrv.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	l, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skipf("can't listen on %s: %v", ip, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	defer srv.Close()

	if err := ForReady(srv.URL, 5*time.Second); err != nil {
		t.Fatalf("ForReady: %v", err)
	}
	if err := ForReachable(srv.URL, 5*time.Second); err != nil {
		t.Fatalf("ForReachable: %v", err)
	}
	if viaProxy.Load() != 0 || direct.Load() != 2 {
		t.Errorf("the probes went through the proxy: %d via proxy, %d direct", viaProxy.Load(), direct.Load())
	}
}

func TestForReady(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if err := ForReady(ok.URL, 5*time.Second); err != nil {
		t.Errorf("200: %v", err)
	}

	// Not ready: the error says what the last answer was.
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer busy.Close()
	err := ForReady(busy.URL, 0)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: want a timeout naming the status, got %v", err)
	}

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := gone.URL
	gone.Close()
	err = ForReady(url, 0)
	if err == nil || !strings.Contains(err.Error(), "last error") {
		t.Errorf("nothing listening: want a timeout naming the error, got %v", err)
	}
}

func TestForReachable(t *testing.T) {
	// Any answer is reachable, a 500 included.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if err := ForReachable(srv.URL, 5*time.Second); err != nil {
		t.Errorf("500 is still an answer: %v", err)
	}
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := gone.URL
	gone.Close()
	if err := ForReachable(url, 0); err == nil {
		t.Error("nothing listening: want an error")
	}
}
