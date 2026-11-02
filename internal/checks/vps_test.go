package checks

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthName(t *testing.T) {
	stubNetwork(t, map[string][]string{
		"id.example.com":    {"203.0.113.7"},
		"nat.example.com":   {"198.51.100.4"},
		"stale.example.com": {"203.0.113.7", "2001:db8::9"},
	}, "10.0.0.5", "203.0.113.7")

	// Resolves here.
	if r := AuthName("id.example.com", true); !r.OK || r.Warn || !strings.Contains(r.Detail, "(this host)") {
		t.Errorf("own address: %+v", r)
	}
	// Doesn't resolve: the one failure.
	if r := AuthName("missing.example.com", true); r.OK || !strings.Contains(r.Detail, "add a DNS record") {
		t.Errorf("unresolved: %+v", r)
	}
	// Elsewhere: a warning, and with Let's Encrypt what it costs.
	r := AuthName("nat.example.com", true)
	if !r.OK || !r.Warn || !strings.Contains(r.Detail, "198.51.100.4") || !strings.Contains(r.Detail, "Let's Encrypt will not issue") {
		t.Errorf("foreign address, acme: %+v", r)
	}
	if r := AuthName("nat.example.com", false); !r.Warn || strings.Contains(r.Detail, "Let's Encrypt") {
		t.Errorf("foreign address, no acme: %+v", r)
	}
	// Here and elsewhere (a stale AAAA): a warning naming the stale address.
	if r := AuthName("stale.example.com", true); !r.OK || !r.Warn || !strings.Contains(r.Detail, "2001:db8::9") || !strings.Contains(r.Detail, "stale") {
		t.Errorf("mixed addresses: %+v", r)
	}
}

func TestRegistryAnswers(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/v2/" {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized) // what a registry says without a token
	}))
	defer srv.Close()
	old := registryClient
	t.Cleanup(func() { registryClient = old })
	registryClient = srv.Client()

	if r := registryAnswersAt("ghcr.io", srv.URL+"/v2/"); !r.OK || !strings.Contains(r.Detail, "401") {
		t.Errorf("a 401 is an answer: %+v", r)
	}

	srv.Close()
	r := registryAnswersAt("ghcr.io", srv.URL+"/v2/")
	if r.OK || !strings.Contains(r.Detail, "outbound HTTPS") || !strings.Contains(r.Detail, "--registry") {
		t.Errorf("no answer: %+v", r)
	}
	if r.Name != "Registry ghcr.io" {
		t.Errorf("name: %q", r.Name)
	}
}
