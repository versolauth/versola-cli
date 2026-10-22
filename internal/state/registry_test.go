package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func finalizeRegistry(t *testing.T, registry string) {
	t.Helper()
	b, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize(FinalizeOptions{Target: "vps", Version: "1.0.0", BundleDir: b, AuthURL: "https://id.example.com", ProxyMode: "nginx", ImageRegistry: registry}); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeRecordsTheImageRegistry(t *testing.T) {
	isolate(t)
	finalizeRegistry(t, "cr.yandex/crp1a2b3c")
	st, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ImageRegistry != "cr.yandex/crp1a2b3c" {
		t.Errorf("recorded %q", st.ImageRegistry)
	}
	if st.SchemaVersion != SchemaVersion {
		t.Errorf("layout %d: the registry is an optional field, the layout stays", st.SchemaVersion)
	}

	// The next configure without --registry is a deployment from the default
	// registry again: nothing carries over.
	finalizeRegistry(t, "")
	if st, err = Load(); err != nil || st.ImageRegistry != "" {
		t.Errorf("after a configure without it: %q, %v", st.ImageRegistry, err)
	}
}

func TestTheDefaultRegistryLeavesNoTraceInTheRecord(t *testing.T) {
	isolate(t)
	finalizeRegistry(t, "")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	// An older versola-cli reads this file too: it must look as it always did.
	if strings.Contains(string(raw), "imageRegistry") {
		t.Errorf("state.json names the registry:\n%s", raw)
	}
}
