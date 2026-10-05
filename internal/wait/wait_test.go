package wait

import (
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
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
	if runtime.GOOS == "windows" {
		// Listening on a non-loopback address makes Windows ask about the
		// firewall for every freshly built test binary; CI runs on Linux.
		t.Skip("would trigger a Windows firewall prompt")
	}
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

// A URL that cannot be requested never will be: fail at once, not after
// the timeout (up waits a minute for each service).
func TestInvalidURLFailsAtOnce(t *testing.T) {
	for _, url := range []string{"localhost:8081/readiness", "http://[::1", "http://a b/", "ftp://localhost/x", "http://localhost:99999/x", "http://localhost:0/x", "http:///x"} {
		start := time.Now()
		if err := ForReady(url, time.Minute); err == nil {
			t.Errorf("ForReady(%q): want an error", url)
		}
		if err := ForReachable(url, time.Minute); err == nil {
			t.Errorf("ForReachable(%q): want an error", url)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%q: took %s", url, d)
		}
	}
}

// An answer from the address asked is the answer: a redirect to some other
// server that is ready does not make this one ready.
func TestRedirectIsNotFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer redirecting.Close()
	err := ForReady(redirecting.URL, 0)
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("a redirect is not ready: got %v", err)
	}
	if err := ForReachable(redirecting.URL, 0); err != nil {
		t.Errorf("a redirect is an answer: %v", err)
	}
}

// The timeout is the timeout: a server that never answers is not waited on
// for a whole attempt past it.
func TestTimeoutIsNotOvershot(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer hang.Close()
	defer close(release)
	start := time.Now()
	err := ForReady(hang.URL, time.Second)
	if err == nil {
		t.Fatal("want a timeout")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a one second timeout took %s", d)
	}
}

// A password in a URL does not reach an error message.
func TestErrorsHideUserinfo(t *testing.T) {
	for _, url := range []string{"http://user:hunter2@127.0.0.1:1/readiness", "ftp://user:hunter2@localhost/x", "http://user:hunter2@localhost:99999/x", "http://user:hunter2@[::1/x"} {
		for name, err := range map[string]error{"ForReady": ForReady(url, 0), "ForReachable": ForReachable(url, 0)} {
			if err == nil || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("%s(%q): %v", name, url, err)
			}
		}
	}
}

// The pause between attempts is cut to the time left: a 1.5 second wait
// for a service that answers 503 ends at 1.5 seconds, not at 2.
func TestPauseIsCutToTheDeadline(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer busy.Close()
	start := time.Now()
	if err := ForReady(busy.URL, 1500*time.Millisecond); err == nil {
		t.Fatal("want a timeout")
	}
	if d := time.Since(start); d < 1400*time.Millisecond || d > 1900*time.Millisecond {
		t.Errorf("a 1.5s wait took %s", d)
	}
}
