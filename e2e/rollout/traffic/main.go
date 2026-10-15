// Command traffic is the rollout e2e's client: it sends GET and POST requests
// at the proxy's public port, at auth's additional listener (8082) and at
// auth's mutual-TLS pass-through (8083) for as long as a rollout runs, and
// counts every answer that is not the one the stub gives.
//
// The success criterion is zero failures: a failure is a transport error or a
// status other than the expected one. The mutual-TLS target is probed over one
// long-lived connection (the way edge keeps its pool) and over a new
// connection each time.
//
// On the long-lived connections to 8083 two things are told apart (see
// classify):
//
//   - a reconnect: the pooled connection was closed while idle and the next
//     request quietly opened a new one. edge's client (zio-http) does the
//     same -- it drops a pooled connection it finds closed and takes a new
//     one -- so no request of edge's fails. Counted, not a failure.
//   - a cut: the connection died under a request (in flight, or between
//     taking it from the pool and writing). net/http retries a replayable
//     request on a reused connection by itself, which would hide it; zio-http
//     does not, so edge's request fails. Counted from the transport error,
//     or from a second connection being taken for the same request.
//
// Cuts on 8083 are the accepted limitation of a pass-through proxy: nginx
// closes the connections of a retired worker at worker_shutdown_timeout (30 s)
// after every reload. They are reported, and fail the run only with -strict.
//
// The run ends on SIGINT/SIGTERM or when the -stop-file exists, then prints a
// summary, as JSON with -json, and exits 1 if anything failed.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type target struct {
	name   string
	method string
	url    string
	host   string // Host header, "" to leave
	want   int
	body   string
	client *http.Client
	// closeAfter sends "Connection: close": a request written on a keep-alive
	// connection that an nginx reload closes at that instant fails with EOF
	// whatever the CLI did, so the plain POST targets do not reuse connections.
	closeAfter bool
	// dials counts connections this target's client opened (pooled targets
	// only, nil otherwise). A pooled target's cuts are the known limitation
	// (see the package comment) and are counted apart from its failures.
	dials *int64
}

func (t target) pooled() bool { return t.dials != nil }

type failure struct {
	At     string `json:"at"`
	Target string `json:"target"`
	Kind   string `json:"kind"` // "fail" or "cut"
	Reused bool   `json:"reusedConnection"`
	Error  string `json:"error"`
}

type stats struct {
	mu         sync.Mutex
	sent       map[string]int
	failed     map[string]int
	cut        map[string]int
	reconnects map[string]int
	versions   map[string]map[string]int // target -> version -> count
	fails      []failure
	keptFails  int // failures kept in fails; cuts have a cap of their own
	keptCuts   int
}

func newStats() *stats {
	return &stats{sent: map[string]int{}, failed: map[string]int{}, cut: map[string]int{}, reconnects: map[string]int{}, versions: map[string]map[string]int{}}
}

func (s *stats) ok(t, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[t]++
	if s.versions[t] == nil {
		s.versions[t] = map[string]int{}
	}
	s.versions[t][version]++
}

func (s *stats) record(t string, kind outcome, reused bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[t]++
	name := "fail"
	if kind == outcomeCut {
		s.cut[t]++
		name = "cut"
	} else {
		s.failed[t]++
	}
	// A broken target must not use up the room the cuts' detail lines need.
	kept := &s.keptFails
	if kind == outcomeCut {
		kept = &s.keptCuts
	}
	if *kept < 100 {
		*kept++
		s.fails = append(s.fails, failure{time.Now().Format("15:04:05.000"), t, name, reused, err})
	}
}

func (s *stats) reconnected(t string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconnects[t] += n
}

// outcome is what one request amounted to.
type outcome int

const (
	outcomeOK outcome = iota
	// outcomeCut: the request's pooled connection died under it. Only ever
	// for a pooled target.
	outcomeCut
	outcomeFail
)

// attempt is what httptrace saw of one client.Do: how many connections it
// asked for and got (net/http asks again when it retries) and whether the
// first of them was reused from the pool.
type attempt struct {
	asked       int // GetConn: connections asked for
	conns       int // GotConn: connections obtained
	firstReused bool
}

// classify decides what a request amounted to. problem is "" when the
// request got the answer it wanted, else what went wrong; transport is true
// when that was a transport error (no answer at all), timeout when it was the
// client's own deadline. Only a pooled target has cuts, and only for the one
// shape the reload explains: the reused connection died under the request
// and nothing else went wrong. A timeout (a hung backend), a retry net/http
// could not get a new connection for, or a failure on the retry's new
// connection are failures: the proxy or a backend is broken, not a retired
// worker. Anywhere else every problem, and every retry, is a failure as
// before.
func classify(pooled bool, a attempt, problem string, transport, timeout bool) outcome {
	if !pooled || !a.firstReused {
		if problem == "" {
			return outcomeOK
		}
		return outcomeFail
	}
	if problem == "" {
		if a.conns > 1 {
			// The answer came over a second connection: net/http retried
			// after the reused one died under the request. zio-http does not.
			return outcomeCut
		}
		return outcomeOK
	}
	if transport && !timeout && a.asked == 1 && a.conns == 1 {
		return outcomeCut
	}
	return outcomeFail
}

func main() {
	var (
		base       = flag.String("base", "http://127.0.0.1:2821", "the proxy's public address")
		host       = flag.String("host", "e2e.test", "Host header for -base (the deployment's auth-url host)")
		additional = flag.String("additional", "127.0.0.1:8082", "auth's additional listener behind the proxy")
		mtls       = flag.String("mtls", "127.0.0.1:8083", "auth's mutual-TLS pass-through behind the proxy")
		interval   = flag.Duration("interval", 50*time.Millisecond, "pause between requests of one worker")
		stopFile   = flag.String("stop-file", "", "stop when this file exists")
		asJSON     = flag.Bool("json", false, "print the summary as JSON")
		expect     = flag.String("expect-versions", "", "comma-separated versions every target must have been served by at least once (e.g. 1.0.0-e2e,1.0.1-e2e): proves traffic ran on both sides of the rollout")
		strict     = flag.Bool("strict", false, "count cuts of the pooled 8083 connections (the known limitation) as failures")
	)
	flag.Parse()

	plain, fresh, pooled := plainClient, freshClient, pooledClient
	insecure := &tls.Config{InsecureSkipVerify: true} // the stub's certificate is self-signed
	post := `{"e2e":"post"}`

	var dialsGet, dialsPost, dialsSlow int64
	targets := []target{
		{"auth GET /.well-known/openid-configuration", "GET", *base + "/.well-known/openid-configuration", *host, 200, "", plain(), false, nil},
		{"auth POST /token", "POST", *base + "/token", *host, 200, post, plain(), true, nil},
		{"edge GET /permissions/me", "GET", *base + "/permissions/me", *host, 401, "", plain(), false, nil},
		{"edge POST /login", "POST", *base + "/login", *host, 200, post, plain(), true, nil},
		{"auth-additional GET :8082", "GET", "http://" + *additional + "/", "", 200, "", plain(), false, nil},
		{"auth-additional POST :8082", "POST", "http://" + *additional + "/", "", 200, post, plain(), true, nil},
		{"auth-mtls GET :8083 (new connection)", "GET", "https://" + *mtls + "/", "", 200, "", fresh(insecure), false, nil},
		{"auth-mtls POST :8083 (new connection)", "POST", "https://" + *mtls + "/", "", 200, post, fresh(insecure), false, nil},
		{"auth-mtls GET :8083 (pooled connection)", "GET", "https://" + *mtls + "/", "", 200, "", pooled(insecure, &dialsGet), false, &dialsGet},
		{"auth-mtls POST :8083 (pooled connection)", "POST", "https://" + *mtls + "/", "", 200, post, pooled(insecure, &dialsPost), false, &dialsPost},
		// Always in flight: a cut of this connection cannot go unnoticed.
		{"auth-mtls GET :8083/slow (pooled, in flight)", "GET", "https://" + *mtls + "/slow?ms=1500", "", 200, "", pooled(insecure, &dialsSlow), false, &dialsSlow},
	}

	st := newStats()
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; halt() }()
	if *stopFile != "" {
		go func() {
			for {
				if _, err := os.Stat(*stopFile); err == nil {
					halt()
					return
				}
				select {
				case <-stop:
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			owed := 0 // see reconnects
			for {
				select {
				case <-stop:
					return
				default:
				}
				res := one(st, t)
				if t.pooled() {
					if n := reconnects(t.dials, res, &owed); n > 0 {
						st.reconnected(t.name, n)
					}
				}
				select {
				case <-stop:
					return
				case <-time.After(*interval):
				}
			}
		}(t)
	}
	started := time.Now()
	wg.Wait()
	report(st, time.Since(started), *asJSON)
	code := exitCode(st, *strict)
	if *expect != "" {
		for _, t := range targets {
			for _, v := range strings.Split(*expect, ",") {
				if st.versions[t.name][v] == 0 {
					fmt.Printf("MISSING: %s was never served by %s\n", t.name, v)
					code = 1
				}
			}
		}
	}
	os.Exit(code)
}

func plainClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}}
}

// freshClient opens a new connection for every request (no keep-alive).
func freshClient(cfg *tls.Config) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: cfg}}
}

// pooledClient keeps one connection for as long as it lives, the way edge
// keeps its pool. Dials are counted: every one after the first is a
// reconnect (or the second connection of a cut request, see classify).
func pooledClient(cfg *tls.Config, dials *int64) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		Proxy: nil, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 0, TLSClientConfig: cfg,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				atomic.AddInt64(dials, 1)
			}
			return c, err
		},
	}}
}

// reconnects returns the dials of a pooled target since its last request
// beyond its one connection, and resets the count. A cut owns one new
// connection -- the one net/http retried over, or, when it did not retry,
// the one the next request has to open -- so that one is not a reconnect:
// owed carries it to the next request when the cut's own request did not
// dial.
func reconnects(dials *int64, res outcome, owed *int) int {
	if res == outcomeCut {
		*owed++
	}
	// Read and reset in one step: a dial that finishes in the background
	// meanwhile must not be lost.
	var extra int
	for {
		n := atomic.LoadInt64(dials)
		if n <= 1 {
			return 0
		}
		if atomic.CompareAndSwapInt64(dials, n, 1) {
			extra = int(n - 1)
			break
		}
	}
	paid := min(extra, *owed)
	*owed -= paid
	return extra - paid
}

// exitCode is 1 when anything failed, or, with strict, anything was cut.
func exitCode(st *stats, strict bool) int {
	for _, n := range st.failed {
		if n > 0 {
			return 1
		}
	}
	if strict {
		for _, n := range st.cut {
			if n > 0 {
				return 1
			}
		}
	}
	return 0
}

func one(st *stats, t target) outcome {
	var body io.Reader
	if t.body != "" {
		body = strings.NewReader(t.body)
	}
	req, err := http.NewRequest(t.method, t.url, body)
	if err != nil {
		st.record(t.name, outcomeFail, false, err.Error())
		return outcomeFail
	}
	if t.host != "" {
		req.Host = t.host
	}
	if t.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Close = t.closeAfter
	var a attempt
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn: func(string) { a.asked++ },
		GotConn: func(i httptrace.GotConnInfo) {
			a.conns++
			if a.conns == 1 {
				a.firstReused = i.Reused
			}
		},
	}))
	res := func(problem string, transport, timeout bool) outcome {
		kind := classify(t.pooled(), a, problem, transport, timeout)
		if kind != outcomeOK {
			if problem == "" {
				problem = fmt.Sprintf("answered only after a retry on a new connection (%d connections taken): the reused one died under the request", a.conns)
			}
			st.record(t.name, kind, a.firstReused, problem)
		}
		return kind
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return res(err.Error(), true, isTimeout(err))
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		return res("reading the answer: "+err.Error(), true, isTimeout(err))
	}
	if resp.StatusCode != t.want {
		return res(fmt.Sprintf("status %d, want %d", resp.StatusCode, t.want), false, false)
	}
	if t.body != "" && t.want == 200 && !strings.Contains(string(got), fmt.Sprintf("body=%d", len(t.body))) {
		return res("the POST body did not arrive whole: "+strings.TrimSpace(string(got)), false, false)
	}
	kind := res("", false, false)
	if kind == outcomeOK {
		st.ok(t.name, resp.Header.Get("X-Stub-Version"))
	} else {
		// Served all the same: the version still counts as seen.
		st.seen(t.name, resp.Header.Get("X-Stub-Version"))
	}
	return kind
}

// isTimeout says whether err is the client's own deadline (Client.Timeout).
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// seen records the version that answered a request already counted (a cut
// whose retry got through).
func (s *stats) seen(t, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[t] == nil {
		s.versions[t] = map[string]int{}
	}
	s.versions[t][version]++
}

func report(st *stats, took time.Duration, asJSON bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"seconds": took.Seconds(), "sent": st.sent, "failed": st.failed, "cut": st.cut,
			"reconnects": st.reconnects, "versions": st.versions, "failures": st.fails,
		})
		return
	}
	names := make([]string, 0, len(st.sent))
	for n := range st.sent {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("traffic for %s\n", took.Round(time.Second))
	total, bad, cut, re := 0, 0, 0, 0
	for _, n := range names {
		total += st.sent[n]
		bad += st.failed[n]
		cut += st.cut[n]
		re += st.reconnects[n]
		fmt.Printf("  %-44s sent %6d  failed %4d  cut %4d  reconnects %4d  served by %v\n", n, st.sent[n], st.failed[n], st.cut[n], st.reconnects[n], st.versions[n])
	}
	fmt.Printf("TOTAL sent %d, failed %d, cut %d (pooled 8083, known limitation), reconnects %d (harmless)\n", total, bad, cut, re)
	for _, f := range st.fails {
		label := "FAIL"
		if f.Kind == "cut" {
			label = "CUT "
		}
		fmt.Printf("  %s %s  %s  reused=%v  %s\n", label, f.At, f.Target, f.Reused, f.Error)
	}
}
