// Command traffic is the rollout e2e's client: it sends GET and POST requests
// at the proxy's public port, at auth's additional listener (8082) and at
// auth's mutual-TLS pass-through (8083) for as long as a rollout runs, and
// counts every answer that is not the one the stub gives.
//
// The success criterion is zero failures: a failure is a transport error or a
// status other than the expected one. The mutual-TLS target is probed over one
// long-lived connection (the way edge keeps its pool, so a connection cut by a
// retired nginx worker shows up as an error on a reused connection) and over
// a new connection each time.
//
// The run ends on SIGINT/SIGTERM or when the -stop-file exists, then prints a
// summary, as JSON with -json, and exits 1 if anything failed.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
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
	// dials counts connections this target's client opened (pooled targets).
	dials *int64
}

type failure struct {
	At     string `json:"at"`
	Target string `json:"target"`
	Reused bool   `json:"reusedConnection"`
	Error  string `json:"error"`
}

type stats struct {
	mu       sync.Mutex
	sent     map[string]int
	failed   map[string]int
	versions map[string]map[string]int // target -> version -> count
	fails    []failure
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

func (s *stats) fail(t string, reused bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[t]++
	s.failed[t]++
	if len(s.fails) < 200 {
		s.fails = append(s.fails, failure{time.Now().Format("15:04:05.000"), t, reused, err})
	}
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
	)
	flag.Parse()

	plain := func() *http.Client {
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}}
	}
	// No keep-alive: a new connection every request.
	fresh := func(cfg *tls.Config) *http.Client {
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: cfg}}
	}
	// One pooled connection, kept for as long as it lives. net/http redials
	// by itself when it finds the connection closed while idle (and retries a
	// GET that hit one), which would hide a cut, so every dial after the
	// first is counted and reported as a failure: edge keeps its connections
	// open and a re-established one means they were cut.
	pooled := func(cfg *tls.Config, dials *int64) *http.Client {
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

	st := &stats{sent: map[string]int{}, failed: map[string]int{}, versions: map[string]map[string]int{}}
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
			for {
				select {
				case <-stop:
					return
				default:
				}
				one(st, t)
				if t.dials != nil && atomic.LoadInt64(t.dials) > 1 {
					// Every dial after the first is a connection that was cut
					// (or closed) and quietly replaced.
					st.fail(t.name, true, fmt.Sprintf("pooled connection was re-established (%d dials)", atomic.LoadInt64(t.dials)))
					atomic.StoreInt64(t.dials, 1)
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
	code := 0
	for _, n := range st.failed {
		if n > 0 {
			code = 1
		}
	}
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

func one(st *stats, t target) {
	var body io.Reader
	if t.body != "" {
		body = strings.NewReader(t.body)
	}
	req, err := http.NewRequest(t.method, t.url, body)
	if err != nil {
		st.fail(t.name, false, err.Error())
		return
	}
	if t.host != "" {
		req.Host = t.host
	}
	if t.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Close = t.closeAfter
	reused := false
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused },
	}))
	resp, err := t.client.Do(req)
	if err != nil {
		st.fail(t.name, reused, err.Error())
		return
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != t.want {
		st.fail(t.name, reused, fmt.Sprintf("status %d, want %d", resp.StatusCode, t.want))
		return
	}
	if t.body != "" && t.want == 200 && !strings.Contains(string(got), fmt.Sprintf("body=%d", len(t.body))) {
		st.fail(t.name, reused, "the POST body did not arrive whole: "+strings.TrimSpace(string(got)))
		return
	}
	st.ok(t.name, resp.Header.Get("X-Stub-Version"))
}

func report(st *stats, took time.Duration, asJSON bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"seconds": took.Seconds(), "sent": st.sent, "failed": st.failed, "versions": st.versions, "failures": st.fails,
		})
		return
	}
	names := make([]string, 0, len(st.sent))
	for n := range st.sent {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("traffic for %s\n", took.Round(time.Second))
	total, bad := 0, 0
	for _, n := range names {
		total += st.sent[n]
		bad += st.failed[n]
		fmt.Printf("  %-44s sent %6d  failed %4d  served by %v\n", n, st.sent[n], st.failed[n], st.versions[n])
	}
	fmt.Printf("TOTAL sent %d, failed %d\n", total, bad)
	for _, f := range st.fails {
		fmt.Printf("  FAIL %s  %s  reused=%v  %s\n", f.At, f.Target, f.Reused, f.Error)
	}
}
