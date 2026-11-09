package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	reused := attempt{asked: 1, conns: 1, firstReused: true}
	retried := attempt{asked: 2, conns: 2, firstReused: true}
	retryNoConn := attempt{asked: 2, conns: 1, firstReused: true}
	fresh := attempt{asked: 1, conns: 1}
	cases := []struct {
		name               string
		pooled             bool
		a                  attempt
		problem            string
		transport, timeout bool
		want               outcome
	}{
		{"answered on the pooled connection", true, reused, "", false, false, outcomeOK},
		{"answered on a new connection after an idle close", true, fresh, "", false, false, outcomeOK},
		{"answered only after net/http retried a dead reused connection", true, retried, "", false, false, outcomeCut},
		{"transport error on the reused connection", true, reused, "EOF", true, false, outcomeCut},
		{"timeout on the reused connection: a hung backend", true, reused, "Client.Timeout exceeded", true, true, outcomeFail},
		{"the retry could not get a new connection", true, retryNoConn, "connection refused", true, false, outcomeFail},
		{"the retry's new connection failed too", true, retried, "EOF", true, false, outcomeFail},
		{"transport error on a new connection", true, fresh, "connection refused", true, false, outcomeFail},
		{"no connection at all", true, attempt{asked: 1}, "dial tcp: refused", true, false, outcomeFail},
		{"wrong status on the reused connection", true, reused, "status 502, want 200", false, false, outcomeFail},
		{"not pooled: a retry is not looked at", false, retried, "", false, false, outcomeOK},
		{"not pooled: a transport error on a reused connection fails", false, reused, "EOF", true, false, outcomeFail},
	}
	for _, c := range cases {
		if got := classify(c.pooled, c.a, c.problem, c.transport, c.timeout); got != c.want {
			t.Errorf("%s: classify = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	st := newStats()
	if exitCode(st, true) != 0 {
		t.Fatal("nothing recorded must pass")
	}
	st.record("mtls pooled", outcomeCut, true, "EOF")
	if exitCode(st, false) != 0 {
		t.Error("a cut must not fail the run without -strict")
	}
	if exitCode(st, true) != 1 {
		t.Error("a cut must fail the run with -strict")
	}
	st.record("public", outcomeFail, false, "status 502, want 200")
	if exitCode(st, false) != 1 {
		t.Error("a failure must fail the run")
	}
}

func TestReconnects(t *testing.T) {
	var d int64
	owed := 0
	if n := reconnects(&d, outcomeOK, &owed); n != 0 || d != 0 {
		t.Errorf("no dial yet: got %d (dials %d)", n, d)
	}
	d = 1
	if n := reconnects(&d, outcomeOK, &owed); n != 0 || d != 1 {
		t.Errorf("the one connection: got %d (dials %d)", n, d)
	}
	d = 3
	if n := reconnects(&d, outcomeOK, &owed); n != 2 || d != 1 {
		t.Errorf("two new connections: got %d (dials %d)", n, d)
	}
	// A GET cut net/http retried: its second connection is the cut's.
	d = 2
	if n := reconnects(&d, outcomeCut, &owed); n != 0 || d != 1 || owed != 0 {
		t.Errorf("retried cut: got %d (dials %d, owed %d)", n, d, owed)
	}
	// A POST cut, not retried: the next request's new connection is the cut's.
	d = 1
	if n := reconnects(&d, outcomeCut, &owed); n != 0 || owed != 1 {
		t.Errorf("cut without a retry: got %d (owed %d)", n, owed)
	}
	d = 2
	if n := reconnects(&d, outcomeOK, &owed); n != 0 || owed != 0 {
		t.Errorf("the request after it: got %d (owed %d), want 0 and 0", n, owed)
	}
	d = 2
	if n := reconnects(&d, outcomeOK, &owed); n != 1 {
		t.Errorf("a later idle close is a reconnect again: got %d", n)
	}
}

// connKey keys the per-connection request counter in a request's context.
type connKey struct{}

// cutServer answers like the stub (200, X-Stub-Version, body=N for a POST)
// but drops the connection without an answer at the request number cutAt on
// a connection (0: never), and closes idle connections after idle.
func cutServer(t *testing.T, cutAt int64, idle time.Duration) *httptest.Server {
	return cutServerWith(t, cutAt, idle, nil)
}

// cutServerWith also calls onCut (with the server) right before the cut, and
// hangs a request to /hang for a second.
func cutServerWith(t *testing.T, cutAt int64, idle time.Duration, onCut func(*httptest.Server)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hang":
			time.Sleep(time.Second)
		case "/stall": // the headers at once, the body never in time
			w.Header().Set("X-Stub-Version", "v1")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(time.Second)
			return
		case "/short": // a POST answer that says it got one byte
			fmt.Fprintln(w, "auth v1 POST body=1")
			return
		}
		n := atomic.AddInt64(r.Context().Value(connKey{}).(*int64), 1)
		if cutAt > 0 && n == cutAt {
			if onCut != nil {
				onCut(srv)
			}
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			c.Close() // like a retired nginx worker at worker_shutdown_timeout
			return
		}
		w.Header().Set("X-Stub-Version", "v1")
		if r.Method == http.MethodPost {
			buf := make([]byte, 1024)
			m, _ := r.Body.Read(buf)
			fmt.Fprintf(w, "auth v1 POST body=%d\n", m)
			return
		}
		fmt.Fprintln(w, "auth v1")
	}))
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, new(int64))
	}
	if idle > 0 {
		srv.Config.IdleTimeout = idle
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func pooledTarget(srv *httptest.Server, method, body string, dials *int64) target {
	cfg := &tls.Config{InsecureSkipVerify: true}
	return target{name: "mtls pooled", method: method, url: srv.URL + "/", want: 200, body: body, client: pooledClient(cfg, dials), dials: dials}
}

// A GET whose reused connection dies under it is retried by net/http on a new
// connection and answered: edge's client would not retry, so it is a cut.
func TestOneHiddenRetryIsACut(t *testing.T) {
	srv := cutServer(t, 2, 0)
	var dials int64
	tg := pooledTarget(srv, "GET", "", &dials)
	st := newStats()
	if got := one(st, tg); got != outcomeOK {
		t.Fatalf("first request: %d, want ok", got)
	}
	if got := one(st, tg); got != outcomeCut {
		t.Fatalf("second request: %d, want cut", got)
	}
	if st.cut[tg.name] != 1 || st.failed[tg.name] != 0 {
		t.Errorf("cut %d failed %d, want 1 and 0", st.cut[tg.name], st.failed[tg.name])
	}
	if st.versions[tg.name]["v1"] != 2 {
		t.Errorf("versions %v: the retried answer still counts as served", st.versions[tg.name])
	}
	owed := 0
	if n := reconnects(&dials, outcomeCut, &owed); n != 0 {
		t.Errorf("reconnects %d, want 0: the retry's connection is the cut's", n)
	}
}

// A POST is not retried by net/http once written: the cut is a transport
// error on the reused connection.
func TestOnePostCutIsACut(t *testing.T) {
	srv := cutServer(t, 2, 0)
	var dials int64
	tg := pooledTarget(srv, "POST", `{"e2e":"post"}`, &dials)
	st := newStats()
	if got := one(st, tg); got != outcomeOK {
		t.Fatalf("first request: %d, want ok", got)
	}
	if got := one(st, tg); got != outcomeCut {
		t.Fatalf("second request: %d, want cut (fails %+v)", got, st.fails)
	}
	if st.failed[tg.name] != 0 {
		t.Errorf("failed %d, want 0", st.failed[tg.name])
	}
}

// A pooled connection the server closes while idle is replaced quietly, the
// way zio-http replaces a closed channel it takes from its pool: a reconnect,
// not a cut.
func TestOneIdleCloseIsAReconnect(t *testing.T) {
	srv := cutServer(t, 0, 50*time.Millisecond)
	var dials int64
	tg := pooledTarget(srv, "GET", "", &dials)
	st := newStats()
	if got := one(st, tg); got != outcomeOK {
		t.Fatalf("first request: %d, want ok", got)
	}
	owed := 0
	reconnects(&dials, outcomeOK, &owed)
	time.Sleep(600 * time.Millisecond)
	got := one(st, tg)
	if got != outcomeOK {
		t.Fatalf("after the idle close: %d, want ok (fails %+v)", got, st.fails)
	}
	if n := reconnects(&dials, got, &owed); n != 1 {
		t.Errorf("reconnects %d, want 1", n)
	}
	if st.cut[tg.name] != 0 || st.failed[tg.name] != 0 {
		t.Errorf("cut %d failed %d, want 0 and 0", st.cut[tg.name], st.failed[tg.name])
	}
}

// A target that is not pooled is judged as before: a hidden retry is not
// looked at, a wrong status fails.
func TestOneNotPooledUnchanged(t *testing.T) {
	srv := cutServer(t, 0, 0)
	tg := target{name: "public", method: "GET", url: srv.URL + "/", want: 401, client: freshClient(&tls.Config{InsecureSkipVerify: true})}
	st := newStats()
	if got := one(st, tg); got != outcomeFail {
		t.Fatalf("wrong status: %d, want fail", got)
	}
	if st.failed["public"] != 1 || st.cut["public"] != 0 {
		t.Errorf("failed %d cut %d, want 1 and 0", st.failed["public"], st.cut["public"])
	}
}

// A pooled request that runs into the client's own deadline is a hung
// backend, not a reload: a failure.
func TestOneTimeoutOnReusedConnectionFails(t *testing.T) {
	srv := cutServer(t, 0, 0)
	var dials int64
	tg := pooledTarget(srv, "GET", "", &dials)
	tg.client.Timeout = 300 * time.Millisecond
	st := newStats()
	if got := one(st, tg); got != outcomeOK {
		t.Fatalf("first request: %d, want ok", got)
	}
	tg.url = srv.URL + "/hang"
	if got := one(st, tg); got != outcomeFail {
		t.Fatalf("timeout: %d, want fail (fails %+v)", got, st.fails)
	}
	if st.cut[tg.name] != 0 {
		t.Errorf("cut %d, want 0", st.cut[tg.name])
	}
}

// The reused connection dies and net/http's retry finds nothing to connect
// to: no upstream answers, a failure, not a cut.
func TestOneRetryWithoutConnectionFails(t *testing.T) {
	srv := cutServerWith(t, 2, 0, func(s *httptest.Server) { s.Listener.Close() })
	var dials int64
	tg := pooledTarget(srv, "GET", "", &dials)
	st := newStats()
	if got := one(st, tg); got != outcomeOK {
		t.Fatalf("first request: %d, want ok", got)
	}
	if got := one(st, tg); got != outcomeFail {
		t.Fatalf("retry without a connection: %d, want fail (fails %+v)", got, st.fails)
	}
}

// After a POST cut (not retried), the next request's new connection belongs
// to the cut and is not counted as a reconnect.
func TestOnePostCutThenNoReconnect(t *testing.T) {
	srv := cutServer(t, 2, 0)
	var dials int64
	tg := pooledTarget(srv, "POST", `{"e2e":"post"}`, &dials)
	st := newStats()
	owed := 0
	for i, want := range []outcome{outcomeOK, outcomeCut, outcomeOK} {
		got := one(st, tg)
		if got != want {
			t.Fatalf("request %d: %d, want %d (fails %+v)", i+1, got, want, st.fails)
		}
		if n := reconnects(&dials, got, &owed); n != 0 {
			t.Errorf("request %d: reconnects %d, want 0", i+1, n)
		}
	}
}

// A transport error on a target that is not pooled is a failure.
func TestOneNotPooledTransportErrorFails(t *testing.T) {
	srv := cutServer(t, 1, 0)
	tg := target{name: "public", method: "POST", url: srv.URL + "/", want: 200, body: "x", client: freshClient(&tls.Config{InsecureSkipVerify: true})}
	st := newStats()
	if got := one(st, tg); got != outcomeFail {
		t.Fatalf("got %d, want fail", got)
	}
}

// A pooled request whose answer starts but stalls past the client's deadline
// is a hung backend, not a reload: a failure, also when it is the body read
// that times out.
func TestOneBodyTimeoutOnReusedConnectionFails(t *testing.T) {
	srv := cutServer(t, 0, 0)
	var dials int64
	tg := pooledTarget(srv, "GET", "", &dials)
	tg.client.Timeout = 300 * time.Millisecond
	st := newStats()
	if got := one(st, tg); got != outcomeOK {
		t.Fatalf("first request: %d, want ok", got)
	}
	tg.url = srv.URL + "/stall"
	if got := one(st, tg); got != outcomeFail {
		t.Fatalf("stalled body: %d, want fail (fails %+v)", got, st.fails)
	}
	if st.cut[tg.name] != 0 {
		t.Errorf("cut %d, want 0", st.cut[tg.name])
	}
}

// A POST whose body did not arrive whole is a failure, not a cut.
func TestOnePostBodyMismatchFails(t *testing.T) {
	srv := cutServer(t, 0, 0)
	tg := target{name: "public", method: "POST", url: srv.URL + "/short", want: 200, body: `{"e2e":"post"}`, client: freshClient(&tls.Config{InsecureSkipVerify: true})}
	st := newStats()
	if got := one(st, tg); got != outcomeFail {
		t.Fatalf("got %d, want fail", got)
	}
}

// The detail lines of cuts are kept even when another target fails all the
// time.
func TestRecordKeepsCutsApartFromFailures(t *testing.T) {
	st := newStats()
	for i := 0; i < 500; i++ {
		st.record("broken", outcomeFail, false, "status 502, want 200")
	}
	st.record("mtls pooled", outcomeCut, true, "EOF")
	var cuts int
	for _, f := range st.fails {
		if f.Kind == "cut" {
			cuts++
		}
	}
	if cuts != 1 {
		t.Errorf("kept %d cut lines, want 1", cuts)
	}
	if st.failed["broken"] != 500 {
		t.Errorf("counter %d, want 500", st.failed["broken"])
	}
}

// Against a real service the exact status of a request without credentials
// is not asserted: 4xx is an answer, 5xx (a proxy with no backend, a dying
// replica) and a transport error are failures.
func TestOneWantAnsweredAcceptsBelow500(t *testing.T) {
	for _, code := range []int{200, 400, 401, 404} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		tg := target{name: "real", method: "GET", url: srv.URL, want: wantAnswered, client: plainClient()}
		if got := one(newStats(), tg); got != outcomeOK {
			t.Errorf("status %d: got %d, want ok", code, got)
		}
		srv.Close()
	}
}

func TestOneWantAnsweredFailsOn5xxAndTransportError(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		tg := target{name: "real", method: "GET", url: srv.URL, want: wantAnswered, client: plainClient()}
		st := newStats()
		if got := one(st, tg); got != outcomeFail || st.failed["real"] != 1 {
			t.Errorf("status %d: got %d (failed %d), want fail", code, got, st.failed["real"])
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens any more
	tg := target{name: "real", method: "GET", url: url, want: wantAnswered, client: plainClient()}
	if got := one(newStats(), tg); got != outcomeFail {
		t.Errorf("connection refused: got %d, want fail", got)
	}
}

// An exact want stays exact: a 404 where 200 is wanted fails, also in a run
// that has real targets elsewhere.
func TestOneExactWantStaysExact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	tg := target{name: "docs", method: "GET", url: srv.URL, want: 200, client: plainClient()}
	if got := one(newStats(), tg); got != outcomeFail {
		t.Fatalf("got %d, want fail", got)
	}
}

// The real targets leave out what needs a client certificate, a client or a
// user, and central when asked to; none of them is a pooled 8083 target.
func TestRealTargets(t *testing.T) {
	with := realTargets("http://p", "e2e.test", "127.0.0.1:8082", "127.0.0.1:8090", nil, plainClient)
	without := realTargets("http://p", "e2e.test", "127.0.0.1:8082", "", nil, plainClient)
	if len(with) != len(without)+1 {
		t.Fatalf("central: %d targets with it, %d without", len(with), len(without))
	}
	for _, tg := range with {
		if tg.pooled() || strings.Contains(tg.url, ":8083") {
			t.Errorf("%s: a real run has no 8083 target yet", tg.name)
		}
		if tg.want != 200 && tg.want != 401 && tg.want != wantAnswered {
			t.Errorf("%s: want %d is a stub's status", tg.name, tg.want)
		}
	}
}

// The real targets' URLs and wants are pinned: a wrong path would fail every
// request of the first real run from the start. The two public documents are
// exact (the CLI itself pins edge as 401 and auth as 200), the rest
// is "answered".
func TestRealTargetsPinned(t *testing.T) {
	got := map[string]string{}
	for _, tg := range realTargets("http://p", "e2e.test", "127.0.0.1:8082", "127.0.0.1:8090", nil, plainClient) {
		got[fmt.Sprintf("%s %s", tg.method, tg.url)] = fmt.Sprint(tg.want)
	}
	want := map[string]string{
		"GET http://p/.well-known/openid-configuration": "200",
		"GET http://p/.well-known/jwks.json":            "200",
		"POST http://p/token":                           fmt.Sprint(wantAnswered),
		"GET http://p/permissions/me":                   "401",
		"GET http://127.0.0.1:8082/":                    fmt.Sprint(wantAnswered),
		"GET http://127.0.0.1:8090/":                    fmt.Sprint(wantAnswered),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %s, got %q (all: %v)", k, v, got[k], got)
		}
	}
}

// The open loop sends at its rate, not at the server's pace: 10 ms period for
// 400 ms against a server that answers in 30 ms still sends about 40 requests
// (a closed loop with the same server would send about 13).
func TestOpenLoopKeepsItsRateWhenTheServerIsSlow(t *testing.T) {
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		time.Sleep(30 * time.Millisecond)
	}))
	defer srv.Close()
	tg := target{name: "slow", method: "GET", url: srv.URL, want: 200, client: plainClient()}
	stop := make(chan struct{})
	done := make(chan struct{})
	st := newStats()
	go func() { openLoop(st, tg, 10*time.Millisecond, 100, stop); close(done) }()
	time.Sleep(400 * time.Millisecond)
	close(stop)
	<-done
	if got := atomic.LoadInt64(&n); got < 20 || got > 50 {
		t.Fatalf("sent %d requests in 400 ms at a 10 ms period, want about 40", got)
	}
	if st.failed["slow"] != 0 {
		t.Fatalf("failures: %v", st.fails)
	}
}

// A stalled server fills the loop's room; every request past it is a failure
// of the target, and the loop still stops on stop.
func TestOpenLoopCountsWhatItCannotSendAsFailures(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	tg := target{name: "stalled", method: "GET", url: srv.URL, want: 200, client: plainClient()}
	stop := make(chan struct{})
	done := make(chan struct{})
	st := newStats()
	go func() { openLoop(st, tg, 5*time.Millisecond, 2, stop); close(done) }()
	time.Sleep(400 * time.Millisecond)
	close(stop)
	close(release) // let the two requests under way finish
	<-done
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.failed["stalled"] < 5 {
		t.Fatalf("failed %d, want many once 2 requests are stuck", st.failed["stalled"])
	}
	if len(st.fails) == 0 || !strings.Contains(st.fails[0].Error, "not sent") {
		t.Fatalf("failures: %v", st.fails)
	}
}

// Failures and cuts are counted per window, apart from each other and past the
// cap of the detail lines.
func TestRecordCountsWindows(t *testing.T) {
	st := newStats()
	for i := 0; i < 150; i++ {
		st.record("a", outcomeFail, false, "x")
	}
	st.record("b", outcomeCut, true, "y")
	sum := func(m map[string]int) (n int) {
		for _, v := range m {
			n += v
		}
		return
	}
	if sum(st.failWin) != 150 || sum(st.cutWin) != 1 {
		t.Fatalf("failWin %v, cutWin %v", st.failWin, st.cutWin)
	}
	if len(st.fails) != 101 {
		t.Fatalf("detail lines %d, want 100 failures + 1 cut", len(st.fails))
	}
}

// writeClientCert makes a self-signed certificate and its key as PEM files, the
// way mtls-client.sh leaves them (the key as PKCS#8).
func writeClientCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e-mtls-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestLoadMTLS(t *testing.T) {
	cert, key := writeClientCert(t)
	if _, err := loadMTLS("127.0.0.1:8083", cert, key, "", "openid"); err == nil || !strings.Contains(err.Error(), "-mtls-client") {
		t.Fatalf("no client id: %v", err)
	}
	if _, err := loadMTLS("127.0.0.1:8083", cert, key+".missing", "c", "openid"); err == nil || !strings.Contains(err.Error(), "client certificate") {
		t.Fatalf("missing key: %v", err)
	}
	m, err := loadMTLS("127.0.0.1:8083", cert, key, "c", "openid")
	if err != nil || len(m.cfg.Certificates) != 1 {
		t.Fatalf("got %v, %v", m, err)
	}
}

// With a client the real targets gain a token request on 8083 over a new
// connection and mtlsPooled over long-lived ones, as a form the real token
// endpoint takes.
func TestRealTargetsWithMTLS(t *testing.T) {
	m := &mtlsClient{addr: "127.0.0.1:8083", cfg: &tls.Config{InsecureSkipVerify: true}, clientID: "e2e mtls/1", scope: "openid"}
	without := realTargets("http://p", "e2e.test", "127.0.0.1:8082", "", nil, plainClient)
	with := realTargets("http://p", "e2e.test", "127.0.0.1:8082", "", m, plainClient)
	extra := with[len(without):]
	if len(extra) != 1+mtlsPooled {
		t.Fatalf("%d extra targets, want %d", len(extra), 1+mtlsPooled)
	}
	for i, tg := range extra {
		if tg.method != "POST" || tg.url != "https://127.0.0.1:8083/token" || tg.want != 200 {
			t.Errorf("%s: %s %s want %d", tg.name, tg.method, tg.url, tg.want)
		}
		if tg.body != "grant_type=client_credentials&client_id=e2e+mtls%2F1&scope=openid" || tg.contentType() != "application/x-www-form-urlencoded" {
			t.Errorf("%s: body %q (%s)", tg.name, tg.body, tg.contentType())
		}
		if (i > 0) != tg.pooled() {
			t.Errorf("%s: pooled=%v", tg.name, tg.pooled())
		}
	}
}

// The token requests really present the certificate: a server that requires
// one answers them, and the stub's body check does not apply to a form
// body.
func TestMTLSTargetsPresentTheCertificate(t *testing.T) {
	var asked, withCert int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&asked, 1)
		if len(r.TLS.PeerCertificates) == 1 && r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" && r.ParseForm() == nil && r.Form.Get("grant_type") == "client_credentials" {
			atomic.AddInt64(&withCert, 1)
			fmt.Fprint(w, `{"access_token":"x"}`)
			return
		}
		w.WriteHeader(400)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()
	cert, key := writeClientCert(t)
	m, err := loadMTLS(srv.Listener.Addr().String(), cert, key, "c1", "openid")
	if err != nil {
		t.Fatal(err)
	}
	st := newStats()
	for _, tg := range realTargets("http://p", "h", "a", "", m, plainClient)[5:] {
		if got := one(st, tg); got != outcomeOK {
			t.Errorf("%s: got %d (%v)", tg.name, got, st.fails)
		}
	}
	if asked != 1+mtlsPooled || withCert != asked {
		t.Fatalf("asked %d, with certificate and form %d", asked, withCert)
	}
	// Without the certificate the handshake itself fails: a failure, not a cut.
	bare := target{name: "bare", method: "POST", url: "https://" + srv.Listener.Addr().String() + "/token", want: 200, body: "grant_type=client_credentials", client: freshClient(&tls.Config{InsecureSkipVerify: true})}
	if got := one(newStats(), bare); got != outcomeFail {
		t.Fatalf("without a certificate: got %d, want fail", got)
	}
}
