package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/manifest"
	"github.com/versolauth/versola-cli/internal/state"
)

// compatManifest says what 0.7.6 states: rolling from 0.7.5, no way in from 0.6.*.
const compatManifest = `{
  "schemaVersion": 1,
  "releases": [
    {"version": "0.7.6", "rollingFrom": ["0.7.5"], "freshDatabaseFrom": ["0.6.*"],
     "notes": "https://github.com/versolauth/versola/releases/tag/0.7.6"}
  ]
}`

func compatFor(t *testing.T, from string) manifest.Result {
	t.Helper()
	m, err := manifest.Parse([]byte(compatManifest))
	if err != nil {
		t.Fatal(err)
	}
	return m.Check(from, "0.7.6")
}

func TestRequireCompatibleFollowsTheManifest(t *testing.T) {
	st := rolloutGuardState(t)

	// Rolling: the release says so, no flag.
	st.Version = "0.7.5"
	if err := RequireCompatible(st, "0.7.6", RolloutOptions{Compat: compatFor(t, "0.7.5")}); err != nil {
		t.Errorf("a declared rolling upgrade asked for a flag: %v", err)
	}

	// A fresh database: refused, and the flag cannot change that.
	st.Version = "0.6.2"
	for _, assume := range []bool{false, true} {
		err := RequireCompatible(st, "0.7.6", RolloutOptions{AssumeCompatible: assume, Compat: compatFor(t, "0.6.2")})
		if err == nil {
			t.Fatalf("a fresh-database release was let through (--assume-compatible=%v)", assume)
		}
		for _, want := range []string{"version 0.7.6 cannot be upgraded in place from 0.6.2", "needs a fresh database", "https://github.com/versolauth/versola/releases/tag/0.7.6", "versola backup create", "versola uninstall --purge"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--assume-compatible=%v: the refusal lacks %q:\n%v", assume, want, err)
			}
		}
		if strings.Contains(err.Error(), "--allow-fresh-database") {
			t.Errorf("rollout points at a configure flag:\n%v", err)
		}
	}

	// Not stated: the operator confirms, and is told why.
	st.Version = "0.7.1"
	unknown := compatFor(t, "0.7.1")
	err := RequireCompatible(st, "0.7.6", RolloutOptions{Compat: unknown})
	if err == nil || !strings.Contains(err.Error(), "--assume-compatible") || !strings.HasPrefix(err.Error(), "The release does not state compatibility with 0.7.1. ") {
		t.Errorf("unknown without the flag: %v", err)
	}
	if err := RequireCompatible(st, "0.7.6", RolloutOptions{AssumeCompatible: true, Compat: unknown}); err != nil {
		t.Errorf("unknown with the flag: %v", err)
	}

	// A rollout under way was confirmed when it started, whatever is read now.
	under := *st
	under.PreviousVersion, under.Version = "0.7.5", "0.7.6"
	if err := RequireCompatible(&under, "0.7.6", RolloutOptions{Compat: compatFor(t, "0.6.2")}); err != nil {
		t.Errorf("continuing a rollout: %v", err)
	}
}

func TestRolloutRefusalFollowsThePostgresMode(t *testing.T) {
	st := rolloutGuardState(t)
	st.Version = "0.6.2"
	compat := compatFor(t, "0.6.2")
	err := RequireCompatible(st, "0.7.6", RolloutOptions{Compat: compat})
	if err == nil || !strings.Contains(err.Error(), "--postgres-host <host:port> --postgres-db <empty database>") {
		t.Errorf("own Postgres: the refusal does not name the flags: %v", err)
	}
	st.PostgresMode = PostgresModeBundled
	err = RequireCompatible(st, "0.7.6", RolloutOptions{Compat: compat})
	if err == nil || !strings.Contains(err.Error(), "`versola uninstall --purge`") {
		t.Fatalf("bundled: %v", err)
	}
	if strings.Contains(err.Error(), "--postgres-host") || strings.Contains(err.Error(), "--allow-fresh-database") {
		t.Errorf("bundled: the refusal names a flag that cannot work:\n%v", err)
	}
}

func TestRolloutPlanSaysWhatTheReleaseSays(t *testing.T) {
	st := rolloutGuardState(t)
	for name, c := range map[string]struct {
		from string
		o    RolloutOptions
		want []string
	}{
		"rolling": {"0.7.5", RolloutOptions{}, []string{"Compatibility: declared by the release (0.7.5 -> 0.7.6 is a rolling upgrade; release notes: https://github.com/versolauth/versola/releases/tag/0.7.6)"}},
		"fresh":   {"0.6.2", RolloutOptions{AssumeCompatible: true}, []string{"the release says 0.7.6 needs a fresh database -- the rollout refuses, with or without --assume-compatible"}},
		"unknown": {"0.7.1", RolloutOptions{}, []string{"Compatibility: NOT confirmed", "(the release does not state compatibility with 0.7.1)"}},
		"flagged": {"0.7.1", RolloutOptions{AssumeCompatible: true}, []string{"confirmed by you (--assume-compatible)", "(the release does not state compatibility with 0.7.1)"}},
	} {
		c.o.Compat = compatFor(t, c.from)
		st.Version = c.from
		plan := RolloutPlan(st, "0.7.6", c.o)
		for _, want := range c.want {
			if !strings.Contains(plan, want) {
				t.Errorf("%s: the plan lacks %q:\n%s", name, want, plan)
			}
		}
	}
}

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckFreshDatabase(t *testing.T) {
	dir := writeManifest(t, compatManifest)

	// configure from a version the release cannot be reached from: refused, with the flag in the message.
	err := checkFreshDatabase(dir, "0.6.2", "0.7.6", "vps", false, false)
	if err == nil || !strings.Contains(err.Error(), "needs a fresh database") || !strings.Contains(err.Error(), "`versola configure vps 0.7.6 ... --allow-fresh-database`, then `versola migrate` and `versola up`") {
		t.Errorf("a fresh-database upgrade was not refused with the way out: %v", err)
	}
	// With their own Postgres the message also names the flags that put an empty database behind the deployment.
	if err == nil || !strings.Contains(err.Error(), "--postgres-host <host:port> --postgres-db <empty database>") {
		t.Errorf("own Postgres: the refusal does not name the flags: %v", err)
	}
	// The bundled Postgres cannot be swapped by configure: the flag is not offered, nor the own-Postgres hint.
	err = checkFreshDatabase(dir, "0.6.2", "0.7.6", "vps", true, false)
	if err == nil || !strings.Contains(err.Error(), "needs a fresh database") || !strings.Contains(err.Error(), "`versola uninstall --purge`") || !strings.Contains(err.Error(), "`versola bootstrap vps 0.7.6 ...`.") {
		t.Errorf("bundled: the refusal lacks the way out: %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "--allow-fresh-database") || strings.Contains(err.Error(), "--postgres-host")) {
		t.Errorf("bundled: the refusal offers a flag that cannot work:\n%v", err)
	}
	// local: its own way out -- a plain uninstall and bootstrap local, no --purge, no flags, no vps.
	err = checkFreshDatabase(dir, "0.6.2", "0.7.6", "local", true, false)
	if err == nil || !strings.Contains(err.Error(), "needs a fresh database") || !strings.Contains(err.Error(), "`versola uninstall`") || !strings.Contains(err.Error(), "`versola bootstrap local 0.7.6`") {
		t.Errorf("local: the refusal lacks the way out: %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "--purge") || strings.Contains(err.Error(), "vps") || strings.Contains(err.Error(), "--allow-fresh-database") || strings.Contains(err.Error(), "--postgres-host")) {
		t.Errorf("local: the refusal names something that does not exist there:\n%v", err)
	}
	// The flag lets someone who has a new database through.
	if err := checkFreshDatabase(dir, "0.6.2", "0.7.6", "vps", false, true); err != nil {
		t.Errorf("--allow-fresh-database: %v", err)
	}
	// Anything else the manifest says is not configure's business.
	for _, from := range []string{"0.7.5", "0.7.1", "0.7.6", "0.8.0", "dev"} {
		if err := checkFreshDatabase(dir, from, "0.7.6", "vps", false, false); err != nil {
			t.Errorf("from %s: %v", from, err)
		}
	}
	// No manifest (an older release): as before.
	if err := checkFreshDatabase(writeManifest(t, ""), "0.6.2", "0.7.0", "vps", false, false); err != nil {
		t.Errorf("a release without a manifest: %v", err)
	}
	// A manifest this CLI cannot read is an error that names the problem, flag or not.
	bad := writeManifest(t, `{"schemaVersion": 9, "releases": []}`)
	for _, allow := range []bool{false, true} {
		if err := checkFreshDatabase(bad, "0.6.2", "0.7.6", "vps", false, allow); err == nil || !strings.Contains(err.Error(), "schemaVersion 9") {
			t.Errorf("an unknown schema (allow=%v): %v", allow, err)
		}
	}
}

func TestPlanCompatibilityLeavesOutWhatItCannotSay(t *testing.T) {
	var log strings.Builder
	dir := writeManifest(t, compatManifest)
	// local has no manifest verdict: the report is the secrets plan alone.
	if got := planCompatibility(&log, dir, "local", "0.7.6"); got != nil {
		t.Errorf("local: %+v", got)
	}
}

// storedLocal is what `configure local` asks the release about: the recorded
// local deployment, and nothing for no record or one of another target (vps
// has its own path, storedVps).
func TestStoredLocalSeesOnlyALocalRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if st := storedLocal(); st != nil {
		t.Fatalf("no record: %+v", st)
	}
	dir, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &state.State{Target: "vps", Version: "0.6.2"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if got := storedLocal(); got != nil {
		t.Errorf("a vps record: %+v", got)
	}
	st = &state.State{Target: "local", Version: "0.6.2"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if got := storedLocal(); got == nil || got.Version != "0.6.2" {
		t.Errorf("a local record: %+v", got)
	}
}

// What configure local calls: refuses over a local record the release cannot be
// reached from, and asks nothing for no record or a record of another target.
func TestCheckFreshDatabaseLocalFollowsTheRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := writeManifest(t, compatManifest)
	if err := checkFreshDatabaseLocal(dir, "0.7.6"); err != nil {
		t.Errorf("no record: %v", err)
	}
	sd, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &state.State{Target: "vps", Version: "0.6.2"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := checkFreshDatabaseLocal(dir, "0.7.6"); err != nil {
		t.Errorf("a vps record: %v", err)
	}
	st = &state.State{Target: "local", Version: "0.6.2"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := checkFreshDatabaseLocal(dir, "0.7.6"); err == nil || !strings.Contains(err.Error(), "`versola bootstrap local 0.7.6`") {
		t.Errorf("a local record on 0.6.2: %v", err)
	}
	st = &state.State{Target: "local", Version: "0.7.5"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := checkFreshDatabaseLocal(dir, "0.7.6"); err != nil {
		t.Errorf("a local record on 0.7.5: %v", err)
	}
}
