package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProxy answers each path with the status in codes (404 for any path
// not listed) and records the Host header of every request.
func fakeProxy(t *testing.T, codes map[string]int) (*httptest.Server, *[]string) {
	t.Helper()
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		if code, ok := codes[r.URL.Path]; ok {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hosts
}

// ready: what a proxy in front of a serving Versola answers.
func ready() map[string]int {
	return map[string]int{
		"/.well-known/openid-configuration": http.StatusOK,
		"/permissions/me":                   http.StatusUnauthorized,
		"/central/admin/":                   http.StatusOK,
	}
}

func TestCheckProbesReady(t *testing.T) {
	srv, hosts := fakeProxy(t, ready())
	answered, err := checkProbes(srv.Client(), srv.URL, "id.example.com")
	if err != nil || !answered {
		t.Fatalf("answered=%v err=%v, want true, nil", answered, err)
	}
	if len(*hosts) != len(probes) {
		t.Fatalf("want %d requests, got %d", len(probes), len(*hosts))
	}
	for _, h := range *hosts {
		if h != "id.example.com" {
			t.Errorf("Host = %q, want id.example.com -- the proxy routes by it", h)
		}
	}
}

func TestCheckProbesNotReady(t *testing.T) {
	for _, c := range []struct {
		name, path string
		code       int
		what       string
	}{
		{"auth not reachable", "/.well-known/openid-configuration", http.StatusBadGateway, "auth"},
		{"edge not reachable", "/permissions/me", http.StatusBadGateway, "edge"},
		{"no route to edge", "/permissions/me", http.StatusNotFound, "edge"},
		// 200 here is not edge refusing an anonymous caller -- something
		// else answered.
		{"something else answering for edge", "/permissions/me", http.StatusOK, "edge"},
		{"admin console missing", "/central/admin/", http.StatusNotFound, "the admin console"},
	} {
		t.Run(c.name, func(t *testing.T) {
			codes := ready()
			codes[c.path] = c.code
			srv, _ := fakeProxy(t, codes)
			answered, err := checkProbes(srv.Client(), srv.URL, "id.example.com")
			if !answered {
				t.Error("an HTTP answer, whatever its status, counts as answered")
			}
			var status *statusError
			if !errors.As(err, &status) {
				t.Fatalf("want a statusError, got %v", err)
			}
			if status.what != c.what || status.code != c.code {
				t.Errorf("got %s answering %d, want %s answering %d", status.what, status.code, c.what, c.code)
			}
			if !strings.Contains(err.Error(), c.what) {
				t.Errorf("error %q doesn't name %s", err, c.what)
			}
		})
	}
}

// Nothing listening: not answered -- what WaitReady's TLS path reads as the
// certificate still pending, rather than a failure.
func TestCheckProbesNoAnswer(t *testing.T) {
	srv, _ := fakeProxy(t, ready())
	base := srv.URL
	srv.Close()
	answered, err := checkProbes(srv.Client(), base, "id.example.com")
	if answered || err == nil {
		t.Fatalf("answered=%v err=%v, want false and an error", answered, err)
	}
	var status *statusError
	if errors.As(err, &status) {
		t.Errorf("a connection failure isn't a statusError: %v", err)
	}
}
