package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

const centralOverride = "services:\n  central:\n    environment:\n      PORT: \"28090\"\n      DPORT: \"28091\"\n"

// guardBundle makes a bundle under a temp home with a compose file and,
// when withUpstreams, the proxy's upstreams.conf, and returns a state that
// points at it and runs from it.
func guardBundle(t *testing.T, withUpstreams bool) (*state.State, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle-1")
	if err := os.MkdirAll(filepath.Join(bundle, "proxy", "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withUpstreams {
		if err := os.WriteFile(filepath.Join(bundle, proxy.UpstreamsFile), []byte("# upstreams\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// What configure writes on vps since config generations.
		yml := "services:\n  proxy:\n    volumes:\n      - type: bind\n        source: \"/home/v/.versola/active/proxy-vps\"\n        target: " + proxy.GenerationMount + "\n        read_only: true\n"
		if err := os.WriteFile(filepath.Join(bundle, proxy.ComposeFile), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withUpstreams {
		// What up and configure write on vps since central is fronted.
		if err := os.WriteFile(filepath.Join(bundle, state.OverridesFile), []byte(centralOverride), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st := &state.State{Target: "vps", Version: "0.7.0", BundleDir: "bundle-1", ProxyMode: proxy.ModeExternal,
		AuthURL: "https://id.example.com", MountedBundleDirs: []string{"bundle-1"}}
	return st, bundle
}

func TestCheckRunnable(t *testing.T) {
	st, bundle := guardBundle(t, true)
	if err := checkRunnable(st, bundle); err != nil {
		t.Fatalf("a current deployment: %v", err)
	}

	for name, mutate := range map[string]func(s *state.State){
		"no proxy mode (before the CLI's proxy)": func(s *state.State) { s.ProxyMode = "" },
		"no bundle directory":                    func(s *state.State) { s.BundleDir = "" },
	} {
		s := *st
		mutate(&s)
		if err := checkRunnable(&s, bundle); !errors.Is(err, state.ErrOldInstall) {
			t.Errorf("%s: got %v, want ErrOldInstall", name, err)
		}
	}

	// A bundle written before upstreams.conf existed (versola-cli <= 0.2.7,
	// the production VPS's 0.2.6 among them).
	old, oldBundle := guardBundle(t, false)
	err := checkRunnable(old, oldBundle)
	if !errors.Is(err, state.ErrOldInstall) || !strings.Contains(err.Error(), "versola configure") {
		t.Errorf("bundle without upstreams.conf: got %v", err)
	}

	mixed := *st
	mixed.Slots = map[string][]state.Slot{"auth": {{N: 1, Version: "0.6.3"}, {N: 2, Version: "0.7.0"}}}
	err = checkRunnable(&mixed, bundle)
	if err == nil || !strings.Contains(err.Error(), "versola rollout") || !strings.Contains(err.Error(), "slot 1 at 0.6.3") {
		t.Errorf("mixed versions: got %v", err)
	}

	surge := *st
	surge.Surge = map[string]int{"edge": 2}
	if err := checkRunnable(&surge, bundle); err == nil || !strings.Contains(err.Error(), "versola rollout") {
		t.Errorf("a stand-in slot: got %v", err)
	}
}

// A vps bundle from a versola-cli with replicas but before config
// generations (0.2.8 and the releases up to this one): refused before
// anything moves auth's ports, by up and by replica alike.
func TestRequireGenerationsOnVps(t *testing.T) {
	st, bundle := guardBundle(t, true)
	old := "services:\n  proxy:\n    volumes:\n      - ./proxy/nginx.conf:/etc/nginx/nginx.conf:ro\n"
	if err := os.WriteFile(filepath.Join(bundle, proxy.ComposeFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkRunnable(st, bundle); !errors.Is(err, state.ErrOldInstall) || !strings.Contains(err.Error(), "generations") {
		t.Errorf("up: %v", err)
	}
	if err := CheckReplicaRequest(st, AuthService, 1, true); !errors.Is(err, state.ErrOldInstall) {
		t.Errorf("replica: %v", err)
	}
	// local has no generations and needs none.
	local := *st
	local.Target, local.ProxyMode = "local", proxy.ModeLocal
	if err := checkRunnable(&local, bundle); err != nil {
		t.Errorf("local: %v", err)
	}
}

// Up refuses before it touches Docker: these all fail on the record alone.
func TestUpRefusesBeforeStartingAnything(t *testing.T) {
	st, _ := guardBundle(t, true)
	mixed := *st
	mixed.Slots = map[string][]state.Slot{"edge": {{N: 1, Version: "0.6.3"}}}
	if err := Up(UpOptions{NoBrowser: true}, &mixed); err == nil || !strings.Contains(err.Error(), "versola rollout") {
		t.Errorf("mixed versions: %v", err)
	}
	old := *st
	old.ProxyMode = ""
	if err := Up(UpOptions{NoBrowser: true}, &old); !errors.Is(err, state.ErrOldInstall) {
		t.Errorf("before the CLI's proxy: %v", err)
	}
	// The production VPS's bundle (versola-cli 0.2.6) before a configure:
	// refused, and CheckUp -- asked before the confirmation -- says the same.
	oldBundle, _ := guardBundle(t, false)
	if err := Up(UpOptions{NoBrowser: true}, oldBundle); !errors.Is(err, state.ErrOldInstall) {
		t.Errorf("bundle without upstreams.conf: %v", err)
	}
	if err := CheckUp(oldBundle); !errors.Is(err, state.ErrOldInstall) {
		t.Errorf("CheckUp, bundle without upstreams.conf: %v", err)
	}
	if err := CheckUp(nil); err != nil {
		t.Errorf("CheckUp leaves nothing configured to Up: %v", err)
	}
	noBundle := *st
	noBundle.BundleDir = ""
	if err := Up(UpOptions{NoBrowser: true}, &noBundle); !errors.Is(err, state.ErrOldInstall) {
		t.Errorf("no bundle directory: %v", err)
	}
}

func TestCheckReplicaRequestGuards(t *testing.T) {
	st, _ := guardBundle(t, true)
	if err := CheckReplicaRequest(st, AuthService, 1, true); err != nil {
		t.Fatalf("a current deployment: %v", err)
	}
	for _, adding := range []bool{true, false} {
		mixed := *st
		mixed.Slots = map[string][]state.Slot{"auth": {{N: 1, Version: "0.7.0"}, {N: 2, Version: "0.6.3"}}}
		if err := CheckReplicaRequest(&mixed, AuthService, 1, adding); err == nil || !strings.Contains(err.Error(), "versola rollout") {
			t.Errorf("adding=%v, mixed versions: %v", adding, err)
		}
		old := *st
		old.ProxyMode = ""
		if err := CheckReplicaRequest(&old, AuthService, 1, adding); !errors.Is(err, state.ErrOldInstall) {
			t.Errorf("adding=%v, before the CLI's proxy: %v", adding, err)
		}
	}
	surge := *st
	surge.Surge = map[string]int{"auth": 2}
	if err := CheckReplicaRequest(&surge, EdgeService, 1, true); err == nil || !strings.Contains(err.Error(), "versola rollout") {
		t.Errorf("a stand-in slot: %v", err)
	}
	// add used to move such a bundle to upstreams.conf itself; now both
	// directions ask for a configure.
	old, _ := guardBundle(t, false)
	for _, adding := range []bool{true, false} {
		if err := CheckReplicaRequest(old, EdgeService, 1, adding); !errors.Is(err, state.ErrOldInstall) {
			t.Errorf("adding=%v, bundle without upstreams.conf: %v", adding, err)
		}
	}
}

// A deployment from before central was fronted: its central still holds
// 8090, so replicas can't be routed until `up` moves it.
func TestCheckReplicaRequestNeedsCentralMoved(t *testing.T) {
	st, bundle := guardBundle(t, true)
	st.Slots = map[string][]state.Slot{"auth": {{N: 1, Version: "0.7.0"}, {N: 2, Version: "0.7.0"}}}
	for _, adding := range []bool{true, false} {
		if err := os.Remove(filepath.Join(bundle, state.OverridesFile)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		err := CheckReplicaRequest(st, AuthService, 1, adding)
		if err == nil || !strings.Contains(err.Error(), "versola up") {
			t.Errorf("adding=%v, no overrides.yml: %v", adding, err)
		}
		// Auth's override alone does not move central.
		only := "services:\n  auth:\n    environment:\n      APORT: \"9082\"\n"
		if err := os.WriteFile(filepath.Join(bundle, state.OverridesFile), []byte(only), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CheckReplicaRequest(st, AuthService, 1, adding); err == nil || !strings.Contains(err.Error(), "versola up") {
			t.Errorf("adding=%v, auth-only overrides.yml: %v", adding, err)
		}
		if err := os.WriteFile(filepath.Join(bundle, state.OverridesFile), []byte(centralOverride), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CheckReplicaRequest(st, AuthService, 1, adding); err != nil {
			t.Errorf("adding=%v, central moved: %v", adding, err)
		}
	}
	// local has no overrides and needs none.
	local, _ := guardBundle(t, true)
	local.Target, local.ProxyMode = "local", proxy.ModeLocal
	if err := requireCentralMoved(local, t.TempDir()); err != nil {
		t.Errorf("local: %v", err)
	}
}
