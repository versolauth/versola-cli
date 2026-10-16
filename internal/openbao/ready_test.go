package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A throwaway value, only ever compared against; tests check that it never
// shows up in output or errors.
const testUnsealKey = "unit-test-unseal-key-not-a-real-secret"

// fakeBao is just enough of OpenBao for EnsureUnsealed: /v1/sys/health
// (503 + sealed:true while sealed, like the real one) and /v1/sys/unseal.
type fakeBao struct {
	mu          sync.Mutex
	initialized bool
	sealed      bool
	goodKey     string
	// ignoreUnseal: accept the call but stay sealed.
	ignoreUnseal bool
	// brokenHealth: the first N health calls answer with a non-JSON 502, as
	// a container that is still starting would.
	brokenHealth int
	// standbyHealth: after an unseal, the first N health calls say standby.
	standbyHealth int
	unsealCalls   int
	keysSeen      []string
}

func (f *fakeBao) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/sys/health":
			if f.brokenHealth > 0 {
				f.brokenHealth--
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("starting"))
				return
			}
			status := http.StatusOK
			if !f.initialized {
				status = http.StatusNotImplemented
			} else if f.sealed {
				status = http.StatusServiceUnavailable
			}
			w.WriteHeader(status)
			standby := false
			if f.initialized && !f.sealed && f.standbyHealth > 0 {
				f.standbyHealth--
				standby = true
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"initialized": f.initialized, "sealed": f.sealed, "standby": standby})
		case "/v1/sys/unseal":
			var req struct {
				Key string `json:"key"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.unsealCalls++
			f.keysSeen = append(f.keysSeen, req.Key)
			if req.Key != f.goodKey {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":["invalid key"]}`))
				return
			}
			if !f.ignoreUnseal {
				f.sealed = false
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"sealed": f.sealed})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func startFake(t *testing.T, f *fakeBao) string {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv.URL // http://127.0.0.1:port -- a local address
}

// withHome points ~/.versola at a temp dir, and optionally saves admin
// credentials for target there.
func withHome(t *testing.T, target string, admin *AdminCreds) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if admin != nil {
		if err := SaveAdminCreds(target, admin); err != nil {
			t.Fatal(err)
		}
	}
}

func fastWaits(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldPoll, oldActive := readyWait, readyPoll, activeWait
	readyWait, readyPoll, activeWait = wait, 5*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { readyWait, readyPoll, activeWait = oldWait, oldPoll, oldActive })
}

func notContainsKey(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, testUnsealKey) {
		t.Errorf("%s contains the unseal key: %q", what, text)
	}
}

func TestEnsureUnsealed_unsealsASealedOpenBao(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "root-token-test", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: true, goodKey: testUnsealKey}
	addr := startFake(t, f)

	var out bytes.Buffer
	if err := EnsureUnsealed(context.Background(), "vps", addr, &out); err != nil {
		t.Fatalf("EnsureUnsealed: %v", err)
	}
	if f.sealed {
		t.Error("OpenBao is still sealed")
	}
	if f.unsealCalls != 1 || f.keysSeen[0] != testUnsealKey {
		t.Errorf("unseal calls = %d, keys = %v; want exactly one with the saved key", f.unsealCalls, f.keysSeen)
	}
	if !strings.Contains(out.String(), "unsealing") {
		t.Errorf("output should say it unsealed, got %q", out.String())
	}
	notContainsKey(t, "output", out.String())
	if strings.Contains(out.String(), "root-token-test") {
		t.Errorf("output contains the root token: %q", out.String())
	}
}

func TestEnsureUnsealed_alreadyUnsealedDoesNothing(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: false, goodKey: testUnsealKey}
	addr := startFake(t, f)

	var out bytes.Buffer
	if err := EnsureUnsealed(context.Background(), "vps", addr, &out); err != nil {
		t.Fatal(err)
	}
	if f.unsealCalls != 0 {
		t.Errorf("unseal was called %d times on an unsealed OpenBao", f.unsealCalls)
	}
	if out.Len() != 0 {
		t.Errorf("should be silent when nothing is needed, got %q", out.String())
	}
}

func TestEnsureUnsealed_noSavedKeyGivesAHint(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", nil) // --setup-openbao: no <target>-admin.json
	f := &fakeBao{initialized: true, sealed: true, goodKey: testUnsealKey}
	addr := startFake(t, f)

	err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard)
	if err == nil {
		t.Fatal("want an error: sealed and nothing to unseal it with")
	}
	for _, want := range []string{"sealed", "vps-admin.json", "bao operator unseal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if f.unsealCalls != 0 {
		t.Errorf("nothing should be submitted without a saved key, got %d calls", f.unsealCalls)
	}
}

func TestEnsureUnsealed_adminFileWithoutKeyIsTreatedAsNoKey(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: ""})
	f := &fakeBao{initialized: true, sealed: true}
	addr := startFake(t, f)

	err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bao operator unseal") {
		t.Fatalf("want the by-hand hint, got %v", err)
	}
	if f.unsealCalls != 0 {
		t.Errorf("an empty key must not be submitted, got %d calls", f.unsealCalls)
	}
}

func TestEnsureUnsealed_refusedKey(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: true, goodKey: "some-other-key"}
	addr := startFake(t, f)

	err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard)
	if err == nil {
		t.Fatal("want an error when the saved key is refused")
	}
	if !strings.Contains(err.Error(), "saved key") {
		t.Errorf("error should say the saved key failed: %v", err)
	}
	notContainsKey(t, "error", err.Error())
}

func TestEnsureUnsealed_stillSealedAfterUnseal(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: true, goodKey: testUnsealKey, ignoreUnseal: true}
	addr := startFake(t, f)

	err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "still sealed") {
		t.Fatalf("want a 'still sealed' error, got %v", err)
	}
}

func TestEnsureUnsealed_notInitialized(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: false, goodKey: testUnsealKey}
	addr := startFake(t, f)

	err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "versola configure vps") {
		t.Fatalf("want a hint to run configure, got %v", err)
	}
	if f.unsealCalls != 0 {
		t.Errorf("an uninitialized OpenBao must not be sent an unseal key")
	}
}

func TestEnsureUnsealed_waitsForOpenBaoToStart(t *testing.T) {
	fastWaits(t, 2*time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: true, goodKey: testUnsealKey, brokenHealth: 3}
	addr := startFake(t, f)

	if err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard); err != nil {
		t.Fatalf("should have waited out the startup: %v", err)
	}
	if f.sealed {
		t.Error("OpenBao is still sealed")
	}
}

func TestEnsureUnsealed_neverAnswering(t *testing.T) {
	fastWaits(t, 60*time.Millisecond)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // nothing listens there any more

	err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard)
	if err == nil {
		t.Fatal("want an error when OpenBao never answers")
	}
	for _, want := range []string{"versola-openbao-vps", "versola configure vps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestEnsureUnsealed_contextCancelledWhileWaiting(t *testing.T) {
	fastWaits(t, time.Minute)
	withHome(t, "vps", nil)
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := EnsureUnsealed(ctx, "vps", addr, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("did not stop waiting when the context ended")
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"http://localhost:8200":       true,
		"http://LOCALHOST:8200":       true,
		"http://127.0.0.1:8200":       true,
		"http://127.0.1.1:8200":       true,
		"http://[::1]:8200":           true,
		"http://openbao.example:8200": false,
		"https://10.0.0.5:8200":       false,
		"http://127.0.0.1.evil.io":    false,
		"http://localhost@evil.com":   false,
		"http://evil.com@localhost":   true,
		"http://localhost.:8200":      false,
		"http://[::1%25lo]:8200":      false,
		"http://[::ffff:127.0.0.1]:1": true,
		"http://localhost.evil.io":    false,
		"::not a url::":               false,
		"":                            false,
	} {
		if got := isLoopbackAddress(addr); got != want {
			t.Errorf("isLoopbackAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

// The saved key must never be sent to an address that is not this machine's:
// `secrets login <target> <address>` takes the address from the command line.
func TestEnsureUnsealed_neverSendsTheKeyToANonLocalAddress(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: true, goodKey: testUnsealKey}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	// Same server, but reached under a name that is not loopback. A transport
	// that dials the test server whatever the host says stands in for DNS.
	oldClient := adminHTTP
	adminHTTP = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
			return http.DefaultTransport.RoundTrip(r2)
		}),
	}
	defer func() { adminHTTP = oldClient }()

	err := EnsureUnsealed(context.Background(), "vps", "http://openbao.example:8200", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bao operator unseal") {
		t.Fatalf("want a refusal with the by-hand hint, got %v", err)
	}
	if f.unsealCalls != 0 {
		t.Fatalf("the unseal key was sent to a non-local address (%d calls)", f.unsealCalls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLogin_sealedIsRecognisable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantSealed bool
	}{
		{"sealed", 503, `{"errors":["Vault is sealed"]}`, true},
		{"other 503", 503, `{"errors":["upstream unavailable"]}`, false},
		{"sealed text, other status", 500, `Vault is sealed`, false},
		{"bad credentials", 400, `{"errors":["invalid secret id"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(&Credentials{Address: srv.URL, RoleID: "role", SecretID: "secret-id-test"})
			err := c.Login(context.Background())
			if err == nil {
				t.Fatal("want a login error")
			}
			if got := errors.Is(err, ErrSealed); got != tc.wantSealed {
				t.Errorf("errors.Is(err, ErrSealed) = %v, want %v (%v)", got, tc.wantSealed, err)
			}
			if strings.Contains(err.Error(), "secret-id-test") {
				t.Errorf("login error contains the secret ID: %v", err)
			}
			if tc.wantSealed && !strings.Contains(err.Error(), "Vault is sealed") {
				t.Errorf("OpenBao's own text should stay in the message: %v", err)
			}
		})
	}
}

func TestEnsureUnsealed_waitsUntilTheNodeIsActive(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	f := &fakeBao{initialized: true, sealed: true, goodKey: testUnsealKey, standbyHealth: 3}
	addr := startFake(t, f)

	if err := EnsureUnsealed(context.Background(), "vps", addr, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.standbyHealth != 0 {
		t.Errorf("returned while the node was still a standby (%d standby answers left)", f.standbyHealth)
	}
}

// A remote OpenBao (given to `secrets login`) behind a proxy that doesn't expose
// /sys/health used to work: it must still reach the login.
func TestEnsureUnsealed_remoteWithoutHealthIsLetThrough(t *testing.T) {
	fastWaits(t, time.Second)
	withHome(t, "vps", &AdminCreds{RootToken: "r", UnsealKey: testUnsealKey})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>forbidden</html>"))
	}))
	defer srv.Close()
	oldClient := adminHTTP
	adminHTTP = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
			return http.DefaultTransport.RoundTrip(r2)
		}),
	}
	defer func() { adminHTTP = oldClient }()

	if err := EnsureUnsealed(context.Background(), "vps", "http://openbao.example:8200", io.Discard); err != nil {
		t.Fatalf("a remote OpenBao without /sys/health must not fail here: %v", err)
	}
}

// The key is sent in a PUT body, which Go would re-send to wherever a 307
// points: a listener on the local OpenBao port must not be able to forward it.
func TestUnseal_doesNotFollowRedirects(t *testing.T) {
	var thief []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		thief = append(thief, string(b))
	}))
	defer evil.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/v1/sys/unseal", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	err := Unseal(context.Background(), redirector.URL, testUnsealKey)
	if err == nil {
		t.Fatal("a redirect is not an unseal; want an error")
	}
	if len(thief) != 0 {
		t.Fatalf("the unseal key was forwarded to another host: %v", thief)
	}
	notContainsKey(t, "error", err.Error())
}
