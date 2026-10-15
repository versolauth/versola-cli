package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
