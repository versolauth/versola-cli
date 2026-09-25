package state

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// isolate points Dir() at a fresh temp home for the test.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func bundles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func configure(t *testing.T) string {
	t.Helper()
	b, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if err := Finalize("vps", "1.0.0", b, "https://id.example.com", "nginx"); err != nil {
		t.Fatal(err)
	}
	return filepath.Base(b)
}

func expectBundles(t *testing.T, dir, step string, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := bundles(t, dir)
	if len(got) != len(want) {
		t.Fatalf("%s: bundles = %v, want %v", step, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: bundles = %v, want %v", step, got, want)
		}
	}
}

// up is a successful `up`: MarkStarting before any container starts,
// MarkRunning once everything is up.
func up(t *testing.T) {
	t.Helper()
	if err := MarkStarting(); err != nil {
		t.Fatal(err)
	}
	if err := MarkRunning(); err != nil {
		t.Fatal(err)
	}
}

// A bundle containers may mount must survive until a successful `up` has
// replaced them all; everything else is cleaned up.
func TestBundleLifecycle(t *testing.T) {
	dir := isolate(t)

	b1 := configure(t)
	expectBundles(t, dir, "first configure", b1)

	up(t)
	expectBundles(t, dir, "first up", b1)

	b2 := configure(t)
	expectBundles(t, dir, "configure while b1 runs", b1, b2)

	b3 := configure(t) // b2 was never started: it goes, b1 still runs
	expectBundles(t, dir, "configure again without up", b1, b3)

	up(t)
	expectBundles(t, dir, "up", b3)
	st, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.BundleDir != b3 || len(st.MountedBundleDirs) != 1 || st.MountedBundleDirs[0] != b3 {
		t.Errorf("after up: BundleDir=%s MountedBundleDirs=%v, want %s / [%s]", st.BundleDir, st.MountedBundleDirs, b3, b3)
	}
}

// An `up` that failed halfway may already have started containers from
// its bundle: a later configure must keep that bundle (and the one that
// was running before) until an `up` succeeds.
func TestBundleFailedUp(t *testing.T) {
	dir := isolate(t)

	b1 := configure(t)
	up(t)

	b2 := configure(t)
	if err := MarkStarting(); err != nil { // up starts some containers from b2, then fails
		t.Fatal(err)
	}

	b3 := configure(t) // the operator reconfigures to try a fix
	expectBundles(t, dir, "configure after a failed up", b1, b2, b3)

	b4 := configure(t) // and again, still without a successful up
	expectBundles(t, dir, "configure again", b1, b2, b4)

	up(t)
	expectBundles(t, dir, "successful up", b4)
}

// A record written before MountedBundleDirs existed can't say whether its
// bundle is in use, so it's kept; a directory left by a configure that
// failed halfway (never finalized) is removed.
func TestBundleLegacyRecordAndLeftovers(t *testing.T) {
	dir := isolate(t)

	old, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	legacy := &State{SchemaVersion: SchemaVersion, Target: "vps", Version: "0.5.3", BundleDir: filepath.Base(old)}
	if err := legacy.Save(); err != nil {
		t.Fatal(err)
	}
	failed, err := Prepare() // a configure that died before Finalize
	if err != nil {
		t.Fatal(err)
	}

	b := configure(t)
	expectBundles(t, dir, "configure over a legacy record", filepath.Base(old), b)
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Errorf("leftover bundle %s from a failed configure wasn't removed", failed)
	}
}
