package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/exitcode"
	"github.com/versolauth/versola-cli/internal/state"
)

// readDiagArchive returns the members of an archive, in order, with their content.
func readDiagArchive(t *testing.T, path string) ([]string, map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	content := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, content
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		content[h.Name] = string(b)
	}
}

// stubDiag replaces what the archive asks Docker and the services.
func stubDiag(t *testing.T) {
	t.Helper()
	od, ol, osv := diagDocker, diagLogs, diagServices
	oc := collectStats
	t.Cleanup(func() { diagDocker, diagLogs, diagServices, collectStats = od, ol, osv, oc })
	diagDocker = func(_ time.Duration, args ...string) ([]byte, string, error) {
		if args[0] == "compose" {
			return []byte("2.40.0\n"), "", nil
		}
		return []byte("client 29.5.2, server 29.5.2\n"), "", nil
	}
	diagServices = func(string) ([]string, error) { return []string{"auth", "central", "postgres", "../evil"}, nil }
	diagLogs = func(w io.Writer, args ...string) (string, error) {
		io.WriteString(w, "2026-11-10T09:00:00Z started "+args[len(args)-1]+"\n")
		return "", nil
	}
	collectStats = func(st *state.State, _ io.Writer) (deploy.StatsReport, error) {
		return deploy.StatsReport{Target: st.Target, Containers: []deploy.ContainerStats{}, Warnings: []string{}}, nil
	}
}

func withDoctorFlags(t *testing.T, bundle string, logs bool) {
	t.Helper()
	ob, ol := doctorBundle, doctorLogs
	t.Cleanup(func() { doctorBundle, doctorLogs = ob, ol })
	doctorBundle, doctorLogs = bundle, logs
}

// The archive holds what doctor said, the versions, the record, status, stats
// and the generated files -- and not one value of the secrets files, nor the
// files themselves, nor OpenBao's.
func TestDoctorBundleHoldsNoSecret(t *testing.T) {
	st := vpsFixture()
	composePath := recordDeployment(t, st)
	stubStatus(t, nil, nil)
	stubDiag(t)
	home, _ := os.UserHomeDir()
	// Next to the generated files: what must never be in the archive.
	bundle := filepath.Dir(composePath)
	os.WriteFile(filepath.Join(bundle, "central.secrets.env"), []byte("JWT_PRIVATE_KEY="+fixtureSecret+"\n"), 0o600)
	os.WriteFile(filepath.Join(bundle, "postgres.secrets.env"), []byte("POSTGRES_PASSWORD="+fixtureSecret+"\n"), 0o600)
	os.MkdirAll(filepath.Join(home, ".versola", "openbao"), 0o700)
	os.WriteFile(filepath.Join(home, ".versola", "openbao", "vps-admin.json"), []byte(`{"rootToken":"`+fixtureSecret+`","unsealKey":"`+fixtureSecret+`"}`), 0o600)
	// A compose file that, wrongly, carries a value: the scrub is the second line of defence.
	os.WriteFile(composePath, []byte("services:\n  postgres:\n    environment:\n      POSTGRES_PASSWORD: "+fixtureSecret+"\n    env_file:\n      - ./postgres.secrets.env\n"), 0o600)

	out := filepath.Join(t.TempDir(), "d.tar.gz")
	withDoctorFlags(t, out, false)
	var said bytes.Buffer
	if err := writeDiagBundle(&said, "PASS Docker daemon\n", true); err != nil {
		t.Fatalf("%v\n%s", err, said.String())
	}
	names, content := readDiagArchive(t, out)
	if names[0] != "manifest.json" {
		t.Errorf("the manifest is not first: %v", names)
	}
	for _, want := range []string{"doctor.txt", "versions.txt", "state.json", "status.json", "stats.json", "files/compose.yml"} {
		if _, ok := content[want]; !ok {
			t.Errorf("%s is missing: %v", want, names)
		}
	}
	for name, body := range content {
		if strings.Contains(body, fixtureSecret) {
			t.Errorf("%s holds a secret value", name)
		}
		if strings.Contains(name, "secrets.env") || strings.Contains(name, "openbao") || strings.HasPrefix(name, "logs/") {
			t.Errorf("a member that must not be there: %s", name)
		}
	}
	if !strings.Contains(content["files/compose.yml"], "POSTGRES_PASSWORD: <redacted>") || !strings.Contains(content["files/compose.yml"], "./postgres.secrets.env") {
		t.Errorf("the compose file: %q", content["files/compose.yml"])
	}
	if !strings.Contains(content["versions.txt"], "client 29.5.2") || !strings.Contains(content["doctor.txt"], "Docker daemon") {
		t.Errorf("versions / doctor: %q / %q", content["versions.txt"], content["doctor.txt"])
	}
	var m deploy.DiagManifest
	if err := json.Unmarshal([]byte(content["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Target != "vps" || m.LogsIncluded || len(m.NotIncluded) < 5 || !strings.Contains(strings.Join(m.NotIncluded, "|"), "--logs") {
		t.Errorf("manifest: %+v", m)
	}
	if strings.Contains(said.String(), fixtureSecret) || !strings.Contains(said.String(), "It holds no password or key") || !strings.Contains(said.String(), "Not in it:") {
		t.Errorf("summary: %s", said.String())
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("the archive is %v, want 0600 (%v)", fi.Mode().Perm(), err)
		}
	}
}

// --logs adds the services' last lines, one file each, under names that
// cannot leave the archive, and says they are not redacted.
func TestDoctorBundleWithLogs(t *testing.T) {
	recordDeployment(t, vpsFixture())
	stubStatus(t, nil, nil)
	stubDiag(t)
	out := filepath.Join(t.TempDir(), "d.tar.gz")
	withDoctorFlags(t, out, true)
	var said bytes.Buffer
	if err := writeDiagBundle(&said, "", true); err != nil {
		t.Fatal(err)
	}
	names, content := readDiagArchive(t, out)
	for _, want := range []string{"logs/auth.log", "logs/central.log", "logs/postgres.log"} {
		if !strings.Contains(content[want], "started") {
			t.Errorf("%s: %q (%v)", want, content[want], names)
		}
	}
	for _, n := range names {
		if strings.Contains(n, "..") || strings.Contains(n, "evil") {
			t.Errorf("a service name became a path: %s", n)
		}
	}
	if !strings.Contains(said.String(), "not redacted") {
		t.Errorf("no warning about the logs: %s", said.String())
	}
}

// With nothing deployed (the usual state of a failed first install) and
// Docker down, the archive is still made, and says what could not be.
func TestDoctorBundleWithoutADeployment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	stubDiag(t)
	diagDocker = func(time.Duration, ...string) ([]byte, string, error) {
		return nil, "Cannot connect to the Docker daemon", errors.New("exit status 1")
	}
	out := filepath.Join(home, "d.tar.gz")
	withDoctorFlags(t, out, true)
	var said bytes.Buffer
	if err := writeDiagBundle(&said, "FAIL Docker daemon\n", true); err != nil {
		t.Fatalf("%v\n%s", err, said.String())
	}
	_, content := readDiagArchive(t, out)
	if !strings.Contains(content["doctor.txt"], "FAIL Docker daemon") || !strings.Contains(content["versions.txt"], "could not be asked") {
		t.Errorf("doctor / versions: %q / %q", content["doctor.txt"], content["versions.txt"])
	}
	var m deploy.DiagManifest
	if err := json.Unmarshal([]byte(content["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, p := range m.Problems {
		problems = append(problems, p.Name)
	}
	for _, want := range []string{"state.json", "stats.json", "files/compose.yml", "logs"} {
		if !strings.Contains(strings.Join(problems, ","), want) {
			t.Errorf("%s is not listed as missing: %v", want, problems)
		}
	}
	if !strings.Contains(said.String(), "Could not be made") {
		t.Errorf("summary: %s", said.String())
	}
}

func TestDoctorLogsNeedsBundle(t *testing.T) {
	withDoctorFlags(t, "", true)
	err := runDoctor(doctorCmd, nil)
	if err == nil || exitcode.Code(err) != exitcode.Usage || !strings.Contains(err.Error(), "--logs goes with --bundle") {
		t.Fatalf("%v", err)
	}
}

func TestDoctorBundleFlags(t *testing.T) {
	for _, name := range []string{"bundle", "logs"} {
		if doctorCmd.Flags().Lookup(name) == nil {
			t.Errorf("doctor has no --%s", name)
		}
	}
}

// A taken name is refused before any check runs, and a flag doctor refuses
// leaves no archive behind (the customer would otherwise find the name taken
// when they run it again).
func TestRunDoctorBundleWritesNoArchiveWhenNoCheckRan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	taken := filepath.Join(home, "taken.tar.gz")
	if err := os.WriteFile(taken, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"a taken name":             {[]string{"--bundle", taken}, exitcode.Usage, "already exists"},
		"a bad target":             {[]string{"--bundle", filepath.Join(home, "a.tar.gz"), "--target", "staging"}, exitcode.Usage, "--target must be"},
		"a vps flag on local":      {[]string{"--bundle", filepath.Join(home, "b.tar.gz"), "--auth-url", "https://id.example.com"}, exitcode.Usage, "only applies to --target vps"},
		"an auth url that is none": {[]string{"--bundle", filepath.Join(home, "c.tar.gz"), "--target", "vps", "--auth-url", "not a url"}, exitcode.Failure, ""},
	} {
		cmd := doctorWith(t, c.args...)
		err := runDoctor(cmd, nil)
		if err == nil || exitcode.Code(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: code %d (%v), want %d with %q", name, exitcode.Code(err), err, c.code, c.want)
		}
		for _, f := range []string{"a.tar.gz", "b.tar.gz", "c.tar.gz"} {
			if _, serr := os.Stat(filepath.Join(home, f)); serr == nil {
				t.Errorf("%s: %s was written although no check ran", name, f)
			}
		}
	}
	if b, _ := os.ReadFile(taken); string(b) != "keep" {
		t.Errorf("the taken file was changed: %q", b)
	}
}

// When Docker did not answer doctor's own check, what needs it is skipped with
// the reason, not waited on; the rest is still in the archive.
func TestDoctorBundleSkipsDockerSectionsWhenDockerDidNotAnswer(t *testing.T) {
	recordDeployment(t, vpsFixture())
	stubStatus(t, nil, nil)
	stubDiag(t)
	asked := 0
	statusContainers = func(string) ([]deploy.ContainerStatus, error) { asked++; return nil, nil }
	diagServices = func(string) ([]string, error) { asked++; return []string{"auth"}, nil }
	collectStats = func(*state.State, io.Writer) (deploy.StatsReport, error) { asked++; return deploy.StatsReport{}, nil }
	out := filepath.Join(t.TempDir(), "d.tar.gz")
	withDoctorFlags(t, out, true)
	if err := writeDiagBundle(io.Discard, "FAIL Docker daemon\n", false); err != nil {
		t.Fatal(err)
	}
	if asked != 0 {
		t.Errorf("Docker was asked %d times although it did not answer", asked)
	}
	_, content := readDiagArchive(t, out)
	var m deploy.DiagManifest
	if err := json.Unmarshal([]byte(content["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	var skipped []string
	for _, p := range m.Problems {
		if strings.Contains(p.Reason, "Docker did not answer") {
			skipped = append(skipped, p.Name)
		}
	}
	if len(skipped) != 3 {
		t.Errorf("status, stats and logs should be skipped for Docker: %v", m.Problems)
	}
	for _, want := range []string{"doctor.txt", "state.json", "files/compose.yml", "versions.txt"} {
		if _, ok := content[want]; !ok {
			t.Errorf("%s is missing", want)
		}
	}
}

// A deployment whose record is there and whose bundle is not is not called "no
// deployment".
func TestDoctorBundleNamesAMissingBundle(t *testing.T) {
	recordDeployment(t, vpsFixture())
	stubStatus(t, nil, nil)
	stubDiag(t)
	_, _, composeErr := state.ComposeFile()
	sections := diagSections("", vpsFixture(), nil, "nowhere/compose.yml", false, composeErr, false, true)
	var reason string
	for _, s := range sections {
		if s.Name == "files/compose.yml" {
			reason = s.Run(io.Discard).Error()
		}
	}
	if !strings.Contains(reason, "bundle files are not on this machine") || strings.Contains(reason, "no deployment") {
		t.Errorf("reason %q", reason)
	}
}

// The wiring of runDoctor, with no Docker on the machine: the checks fail (exit
// code 3), the archive is written anyway, what needs Docker is skipped with the
// reason, and a password a flag carried is not in doctor's output, the record or
// the logs.
func TestRunDoctorBundleWithoutDocker(t *testing.T) {
	st := vpsFixture()
	st.PostgresHost = "postgres://versola:hunter2@db.example.com:5432"
	st.PostgresMode = ""
	recordDeployment(t, st)
	stubStatus(t, nil, nil)
	stubDiag(t)
	t.Setenv("PATH", "") // no docker: doctor's daemon check fails
	out := filepath.Join(t.TempDir(), "d.tar.gz")
	cmd := doctorWith(t, "--bundle", out)
	err := runDoctor(cmd, nil)
	if code := exitcode.Code(err); code != exitcode.Prerequisites {
		t.Fatalf("exit code %d (%v), want %d: a failing check still fails the command", code, err, exitcode.Prerequisites)
	}
	if doctorDockerOK {
		t.Errorf("doctorDockerOK is still true: what needs Docker would be waited on")
	}
	_, content := readDiagArchive(t, out)
	for _, want := range []string{"manifest.json", "doctor.txt", "state.json", "versions.txt"} {
		if _, ok := content[want]; !ok {
			t.Errorf("%s is missing from the archive", want)
		}
	}
	for name, body := range content {
		if strings.Contains(body, "hunter2") {
			t.Errorf("%s carries the password a flag held", name)
		}
	}
	var m deploy.DiagManifest
	if err := json.Unmarshal([]byte(content["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, p := range m.Problems {
		if strings.Contains(p.Reason, "Docker did not answer") {
			skipped++
		}
	}
	if skipped < 2 {
		t.Errorf("status and stats should be skipped because Docker did not answer: %v", m.Problems)
	}
}

// A write error is its own exit code (1), not the checks' (3), and both are said.
func TestRunDoctorBundleWriteErrorIsFailure(t *testing.T) {
	recordDeployment(t, vpsFixture())
	stubStatus(t, nil, nil)
	stubDiag(t)
	t.Setenv("PATH", "")
	blocker := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A name under a regular file: no directory can be made there.
	err := runDoctor(doctorWith(t, "--bundle", filepath.Join(blocker, "d.tar.gz")), nil)
	if code := exitcode.Code(err); code != exitcode.Failure {
		t.Fatalf("exit code %d (%v), want %d", code, err, exitcode.Failure)
	}
	if err == nil || !strings.Contains(err.Error(), "doctor found") {
		t.Errorf("the checks' own error is not said with it: %v", err)
	}
}

// What Postgres logs of a failed role statement is blanked in the logs, which are
// otherwise left as they are.
func TestDoctorBundleLogsBlankWhatPostgresLogs(t *testing.T) {
	recordDeployment(t, vpsFixture())
	stubStatus(t, nil, nil)
	stubDiag(t)
	diagLogs = func(w io.Writer, args ...string) (string, error) {
		io.WriteString(w, "STATEMENT:  CREATE ROLE app LOGIN PASSWORD 'hunter2'\nuser alice@example.com logged in\n")
		return "", nil
	}
	out := filepath.Join(t.TempDir(), "d.tar.gz")
	withDoctorFlags(t, out, true)
	if err := writeDiagBundle(io.Discard, "", true); err != nil {
		t.Fatal(err)
	}
	_, content := readDiagArchive(t, out)
	log := content["logs/auth.log"]
	if strings.Contains(log, "hunter2") || !strings.Contains(log, "alice@example.com") {
		t.Errorf("the log: %q (the password goes, the rest stays as the service wrote it)", log)
	}
}
