package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// fileSecret is written into every OpenBao credentials file of a test and must
// never appear in what uninstall prints.
const fileSecret = "S3CR3T-IN-A-CREDENTIALS-FILE"

type uninstallCase struct {
	st        *state.State // recorded deployment with a compose file; nil for none
	volumes   []string     // Docker volumes that exist
	files     bool         // create the vps files in ~/.versola/openbao
	running   []string     // containers IsRunning reports as running
	stopped   []string     // containers that exist but are not running
	rmFails   string       // a volume whose `docker volume rm` fails
	volumeErr string       // a volume Docker cannot be asked about
	dockerOff bool
	answer    string
	yes       bool
	purge     bool
}

type uninstallResult struct {
	calls   [][]string // every Docker command, in order
	volumes map[string]bool
	home    string
	out     string
	err     error
}

func vpsFileNames() []string {
	return []string{"vps.json", "vps-admin.json", "vps-postgres-role.sql", "vps-postgres-init.sql"}
}

func runUninstallCase(t *testing.T, c uninstallCase) uninstallResult {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if c.st != nil {
		dir := filepath.Join(home, ".versola", "active", "bundle-1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.st.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if c.files {
		dir := filepath.Join(home, ".versola", "openbao")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range append(vpsFileNames(), "local.json") {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(fileSecret), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	res := uninstallResult{volumes: map[string]bool{}, home: home}
	for _, v := range c.volumes {
		res.volumes[v] = true
	}
	running, exists := map[string]bool{}, map[string]bool{}
	for _, r := range c.running {
		running[r], exists[r] = true, true
	}
	for _, r := range c.stopped {
		exists[r] = true
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(c.answer)
	w.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outDone := make(chan string)
	go func() {
		b, _ := io.ReadAll(outR)
		outDone <- string(b)
	}()

	oldIn, oldOut := os.Stdin, os.Stdout
	oldUp, oldDocker, oldImages, oldRunning, oldVol, oldExists := uninstallDockerUp, uninstallDocker, uninstallImages, uninstallIsRunning, uninstallVolumeExists, uninstallContainerExists
	oldYes, oldPurge := assumeYes, uninstallPurge
	os.Stdin, os.Stdout = r, outW
	uninstallDockerUp = func() bool { return !c.dockerOff }
	uninstallImages = func(string) ([]string, error) { return nil, nil }
	uninstallIsRunning = func(name string) (bool, error) { return running[name], nil }
	uninstallVolumeExists = func(name string) (bool, error) {
		if name == c.volumeErr {
			return false, errors.New("permission denied on the Docker socket")
		}
		return res.volumes[name], nil
	}
	uninstallContainerExists = func(name string) (bool, error) { return exists[name], nil }
	uninstallDocker = func(args ...string) error {
		res.calls = append(res.calls, args)
		if len(args) == 3 && args[0] == "rm" && args[1] == "-f" {
			delete(running, args[2])
			delete(exists, args[2])
		}
		if len(args) == 3 && args[0] == "volume" && args[1] == "rm" {
			if args[2] == c.rmFails {
				return errors.New("volume is in use")
			}
			delete(res.volumes, args[2])
		}
		return nil
	}
	assumeYes, uninstallPurge = c.yes, c.purge
	defer func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		uninstallDockerUp, uninstallDocker, uninstallImages, uninstallIsRunning, uninstallVolumeExists, uninstallContainerExists = oldUp, oldDocker, oldImages, oldRunning, oldVol, oldExists
		assumeYes, uninstallPurge = oldYes, oldPurge
		r.Close()
	}()

	res.err = runUninstall(uninstallCmd, nil)
	outW.Close()
	res.out = <-outDone
	if strings.Contains(res.out, fileSecret) {
		t.Errorf("a credentials file's content reached the output:\n%s", res.out)
	}
	return res
}

func (r uninstallResult) fileExists(name string) bool {
	_, err := os.Stat(filepath.Join(r.home, ".versola", "openbao", name))
	return err == nil
}

func (r uninstallResult) volumeRemoved(name string) bool {
	for _, c := range r.calls {
		if len(c) == 3 && c[0] == "volume" && c[1] == "rm" && c[2] == name {
			return true
		}
	}
	return false
}

// index of the first Docker call whose arguments contain all of want, -1 if none.
func (r uninstallResult) callIndex(want ...string) int {
	for i, c := range r.calls {
		joined := " " + strings.Join(c, " ") + " "
		ok := true
		for _, w := range want {
			if !strings.Contains(joined, " "+w+" ") {
				ok = false
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

var (
	obVol  = deploy.OpenbaoVolumeName("vps")
	pgVol  = deploy.PostgresVolumeName()
	allVol = []string{deploy.OpenbaoVolumeName("vps"), deploy.PostgresVolumeName(), proxy.ACMEVolume}
)

func vpsState() *state.State {
	return &state.State{Target: "vps", Version: "0.7.2", BundleDir: "bundle-1", AuthURL: "https://id.example.com", ProxyMode: "nginx", PostgresMode: deploy.PostgresModeBundled}
}

// The case the purge is for: a plain uninstall already removed the record,
// and configure now refuses the OpenBao volume it left. One command, "vps"
// typed, and the host is fresh -- the certificates excepted.
func TestUninstallPurgeWithoutRecord(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, files: true, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if !r.volumeRemoved(obVol) || !r.volumeRemoved(pgVol) {
		t.Errorf("volumes not both removed: %v", r.calls)
	}
	if r.volumeRemoved(proxy.ACMEVolume) {
		t.Errorf("the TLS certificate volume was removed: %v", r.calls)
	}
	for _, f := range vpsFileNames() {
		if r.fileExists(f) {
			t.Errorf("%s left behind", f)
		}
	}
	if !r.fileExists("local.json") {
		t.Errorf("local's credentials were removed too")
	}
	for _, want := range append(vpsFileNames(), obVol, pgVol, proxy.ACMEVolume, "DESTROY") {
		if !strings.Contains(r.out, want) {
			t.Errorf("summary does not name %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, "docker volume rm") {
		t.Errorf("prints a raw docker command:\n%s", r.out)
	}
}

// Anything but the typed word aborts with an error and removes nothing, and
// --yes does not answer the question.
func TestUninstallPurgeNeedsTheWordTyped(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer string
		yes    bool
	}{
		{"y", "y\n", false},
		{"yes", "yes\n", false},
		{"empty", "\n", false},
		{"closed stdin", "", false},
		{"--yes alone", "", true},
		{"--yes and y", "y\n", true},
		{"VPS", "VPS\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := runUninstallCase(t, uninstallCase{st: vpsState(), volumes: allVol, files: true, answer: c.answer, yes: c.yes, purge: true})
			if r.err == nil || !strings.Contains(r.err.Error(), "aborted") {
				t.Errorf("err %v, want aborted", r.err)
			}
			if len(r.calls) != 0 {
				t.Errorf("Docker was called: %v", r.calls)
			}
			for _, f := range vpsFileNames() {
				if !r.fileExists(f) {
					t.Errorf("%s removed", f)
				}
			}
			if _, err := os.Stat(filepath.Join(r.home, ".versola", "active")); err != nil {
				t.Errorf("~/.versola/active removed: %v", err)
			}
		})
	}
}

func TestUninstallPurgeRefusedOnLocalRecord(t *testing.T) {
	st := &state.State{Target: "local", Version: "0.7.2", BundleDir: "bundle-1"}
	r := runUninstallCase(t, uninstallCase{st: st, volumes: allVol, files: true, answer: "vps\n", purge: true})
	if r.err == nil || !strings.Contains(r.err.Error(), "--purge is for vps") {
		t.Errorf("err %v, want the local refusal", r.err)
	}
	if len(r.calls) != 0 || !r.fileExists("vps.json") {
		t.Errorf("something was removed: %v", r.calls)
	}
}

func TestUninstallPurgeNeedsDocker(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{st: vpsState(), files: true, answer: "vps\n", purge: true, dockerOff: true})
	if r.err == nil || !strings.Contains(r.err.Error(), "needs Docker") {
		t.Errorf("err %v, want refused without Docker", r.err)
	}
	if !r.fileExists("vps.json") {
		t.Errorf("credentials removed without their volume")
	}
}

// The stack and a leftover OpenBao container go before the volumes they hold.
func TestUninstallPurgeStopsWhatUsesTheVolumesFirst(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{st: vpsState(), volumes: allVol, files: true, running: []string{deploy.OpenbaoContainerName("vps")}, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	down, rmContainer, rmVolume := r.callIndex("down", "--volumes"), r.callIndex("rm", "-f"), r.callIndex("volume", "rm", obVol)
	if down < 0 || rmContainer < 0 || rmVolume < 0 || !(down < rmVolume && rmContainer < rmVolume) {
		t.Errorf("order: down %d, rm -f %d, volume rm %d; calls %v", down, rmContainer, rmVolume, r.calls)
	}
}

// OpenBao's volume that could not be removed keeps the credentials that open
// it, and stops before the database.
func TestUninstallPurgeKeepsCredentialsWhileOpenBaoStays(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, files: true, rmFails: obVol, answer: "vps\n", purge: true})
	if r.err == nil || !strings.Contains(r.err.Error(), "uninstall --purge") {
		t.Errorf("err %v, want a failure naming the command to run again", r.err)
	}
	if !r.fileExists("vps.json") || !r.fileExists("vps-admin.json") {
		t.Errorf("credentials removed although OpenBao's volume stayed")
	}
	if r.volumeRemoved(pgVol) {
		t.Errorf("the database was removed after the failure: %v", r.calls)
	}
}

// A plain uninstall on vps keeps the data, names it, and ends with the one
// command that removes it -- also when nothing else is left to remove.
func TestUninstallPlainVpsPointsAtPurge(t *testing.T) {
	for _, c := range []struct {
		name string
		st   *state.State
	}{{"recorded", vpsState()}, {"no record", nil}} {
		t.Run(c.name, func(t *testing.T) {
			r := runUninstallCase(t, uninstallCase{st: c.st, volumes: allVol, files: true, answer: "y\n"})
			if r.err != nil {
				t.Fatalf("err %v\n%s", r.err, r.out)
			}
			if r.volumeRemoved(obVol) || r.volumeRemoved(pgVol) || !r.fileExists("vps.json") {
				t.Errorf("vps data removed without --purge: %v", r.calls)
			}
			if !strings.Contains(r.out, "To start over from scratch: versola uninstall --purge") {
				t.Errorf("no pointer at --purge:\n%s", r.out)
			}
			if strings.Contains(r.out, "docker volume rm") {
				t.Errorf("prints a raw docker command:\n%s", r.out)
			}
		})
	}
}

// Nothing to purge: --purge asks the usual question, not the typed one.
func TestUninstallPurgeWithNothingToPurge(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{st: vpsState(), answer: "y\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.out, "nothing more to destroy") {
		t.Errorf("does not say there is nothing to purge:\n%s", r.out)
	}
}

// A container that exists but is stopped still holds its volume: the purge
// removes Versola's own, running or not, before the volumes.
func TestUninstallPurgeRemovesStoppedContainersFirst(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, files: true, stopped: []string{deploy.OpenbaoContainerName("vps"), deploy.PostgresContainerName()}, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	ob, pg := r.callIndex("rm", "-f", deploy.OpenbaoContainerName("vps")), r.callIndex("rm", "-f", deploy.PostgresContainerName())
	obv := r.callIndex("volume", "rm", obVol)
	if ob < 0 || pg < 0 || obv < 0 || ob > obv || pg > obv {
		t.Errorf("containers not removed before the volumes: %v", r.calls)
	}
}

// A record with a Postgres of its own gets the warning about it, even with a
// bundled volume from an earlier attempt next to it.
func TestUninstallPurgeWarnsAboutOwnPostgres(t *testing.T) {
	st := vpsState()
	st.PostgresMode, st.PostgresHost = "", "db.example.com:5432"
	r := runUninstallCase(t, uninstallCase{st: st, volumes: allVol, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.out, "recreate its schemas") {
		t.Errorf("no warning about the external Postgres:\n%s", r.out)
	}
}

// A volume Docker cannot be asked about is not "absent": the purge stops
// before anything, instead of calling the server fresh.
func TestUninstallPurgeStopsWhenDockerCannotAnswer(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, files: true, volumeErr: pgVol, answer: "vps\n", purge: true})
	if r.err == nil || !strings.Contains(r.err.Error(), "Nothing was removed") {
		t.Errorf("err %v, want a refusal", r.err)
	}
	if len(r.calls) != 0 || !r.fileExists("vps.json") {
		t.Errorf("something was removed: %v", r.calls)
	}
}

// The question names the containers it removes, and Versola running with no
// record here gets the lost-~/.versola warning before it.
func TestUninstallPurgeNamesRunningContainers(t *testing.T) {
	pg := deploy.PostgresContainerName()
	r := runUninstallCase(t, uninstallCase{volumes: allVol, running: []string{pg}, answer: "n\n", purge: true})
	if r.err == nil {
		t.Fatalf("answer n went through")
	}
	q := strings.Index(r.out, "Type \"vps\"")
	for _, want := range []string{"container " + pg + " (RUNNING", "do NOT purge"} {
		if i := strings.Index(r.out, want); i < 0 || i > q {
			t.Errorf("%q not shown before the question:\n%s", want, r.out)
		}
	}
}
