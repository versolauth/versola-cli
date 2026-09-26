package deploy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/versolauth/versola-cli/internal/openbao"
)

func TestDecideOpenbao(t *testing.T) {
	cases := []struct {
		name                                      string
		exists, running, configMissing, canUnseal bool
		want                                      openbaoAction
	}{
		{"no container", false, false, false, false, openbaoStart},
		{"no container, key saved", false, false, false, true, openbaoStart},
		{"running, config present", true, true, false, false, openbaoKeep},
		{"running, config present, key saved", true, true, false, true, openbaoKeep},
		{"stopped, config present", true, false, false, false, openbaoRecreate},
		{"stopped, config missing, no key", true, false, true, false, openbaoRecreate},
		{"stopped, config missing, key saved", true, false, true, true, openbaoRecreate},
		{"running, config missing, key saved", true, true, true, true, openbaoRecreate},
		{"running, config missing, no key", true, true, true, false, openbaoKeepStale},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideOpenbao(c.exists, c.running, c.configMissing, c.canUnseal); got != c.want {
				t.Errorf("decideOpenbao(%v, %v, %v, %v) = %v, want %v", c.exists, c.running, c.configMissing, c.canUnseal, got, c.want)
			}
		})
	}
}

func TestCanUnsealOpenbao(t *testing.T) {
	const goodToken = "s.good"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token/lookup-self" && r.Header.Get("X-Vault-Token") == goodToken {
			w.Write([]byte(`{"data":{}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	writeAdmin := func(t *testing.T, creds *openbao.AdminCreds) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if creds != nil {
			if err := openbao.SaveAdminCreds("vps", creds); err != nil {
				t.Fatal(err)
			}
		}
	}

	cases := []struct {
		name        string
		creds       *openbao.AdminCreds
		setupByHand bool
		want        bool
	}{
		{"no creds file", nil, false, false},
		{"--setup-openbao", &openbao.AdminCreds{RootToken: goodToken, UnsealKey: "k"}, true, false},
		{"empty unseal key", &openbao.AdminCreds{RootToken: goodToken}, false, false},
		{"token from another instance", &openbao.AdminCreds{RootToken: "s.other", UnsealKey: "k"}, false, false},
		{"valid", &openbao.AdminCreds{RootToken: goodToken, UnsealKey: "k"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeAdmin(t, c.creds)
			ok, reason, err := canUnsealOpenbao("vps", srv.URL, c.setupByHand)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.want {
				t.Errorf("canUnsealOpenbao = %v (%s), want %v", ok, reason, c.want)
			}
			if !ok && reason == "" {
				t.Error("no reason given for refusing")
			}
		})
	}
}

func TestConfigGone(t *testing.T) {
	bundles := t.TempDir()
	present := filepath.Join(bundles, "bundle-1", "openbao.hcl")
	if err := os.MkdirAll(filepath.Dir(present), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "openbao.hcl") // missing, but not ours

	cases := []struct {
		name, source string
		want         bool
	}{
		{"pruned bundle", filepath.Join(bundles, "bundle-0", "openbao.hcl"), true},
		{"current bundle", present, false},
		{"missing outside ~/.versola/active (Docker Desktop VM path)", outside, false},
		{"relative path", "bundle-0/openbao.hcl", false},
		{"no mount", "", false},
		{"the bundles dir itself", bundles, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := configGone(c.source, bundles); got != c.want {
				t.Errorf("configGone(%q) = %v, want %v", c.source, got, c.want)
			}
		})
	}
}
