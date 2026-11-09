package cmd

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/exitcode"
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
	project   []string     // containers of the vps compose project (running if also in running)
	tmpFiles  bool         // leave a cut-off atomic write (.tmp-*) in ~/.versola/openbao
	pending   bool         // the note of an unfinished first install of vps, in ~/.versola/active
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
	if c.pending {
		if err := state.SavePending(state.Pending{Target: "vps", SecretsRevision: 1}); err != nil {
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

	if c.tmpFiles {
		dir := filepath.Join(home, ".versola", "openbao")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".tmp-123456"), []byte(fileSecret), 0o600); err != nil {
			t.Fatal(err)
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
	for _, r := range c.project {
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
	oldUp, oldDocker, oldImages, oldRunning, oldVol, oldExists, oldProject := uninstallDockerUp, uninstallDocker, uninstallImages, uninstallIsRunning, uninstallVolumeExists, uninstallContainerExists, uninstallProjectContainers
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
	uninstallProjectContainers = func(project string) ([]string, error) {
		if project != deploy.VpsComposeProject {
			t.Errorf("asked for the containers of project %q", project)
		}
		var out []string
		for _, n := range c.project {
			if exists[n] {
				out = append(out, n)
			}
		}
		return out, nil
	}
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
		uninstallDockerUp, uninstallDocker, uninstallImages, uninstallIsRunning, uninstallVolumeExists, uninstallContainerExists, uninstallProjectContainers = oldUp, oldDocker, oldImages, oldRunning, oldVol, oldExists, oldProject
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

// A shell chaining uninstall and a reinstall must stop after a refusal. The
// deployment and its files stay intact, and the existing Aborted line stays
// the only message (Run recognizes Reported and doesn't print it twice).
func TestUninstallRefusalExitsAbortedWithoutRemovingAnything(t *testing.T) {
	for _, answer := range []string{"n\n", ""} {
		t.Run(answer, func(t *testing.T) {
			r := runUninstallCase(t, uninstallCase{st: vpsState(), files: true, answer: answer})
			if exitcode.Code(r.err) != exitcode.Aborted || !exitcode.IsReported(r.err) {
				t.Fatalf("err=%v code=%d, want reported aborted", r.err, exitcode.Code(r.err))
			}
			if strings.Count(r.out, "Aborted.") != 1 {
				t.Errorf("missing abort output: %s", r.out)
			}
			if len(r.calls) != 0 {
				t.Errorf("refusal ran destructive commands: %v", r.calls)
			}
			if !r.fileExists("vps.json") {
				t.Error("refusal removed credentials")
			}
		})
	}
}

// A plain uninstall already removed the record, but the services of the run
// before it are still up and hold the ports: with no compose file to `down`
// them, the purge finds them by the project label, lists them in the question
// and removes them before the volumes they use.
func TestUninstallPurgeStopsServicesOfAnEarlierRunWithoutRecord(t *testing.T) {
	auth, proxyC := "versola-auth-1", "versola-proxy"
	r := runUninstallCase(t, uninstallCase{volumes: allVol, running: []string{auth, proxyC}, project: []string{auth, proxyC}, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	q := strings.Index(r.out, "Type \"vps\"")
	for _, c := range []string{auth, proxyC} {
		if i := strings.Index(r.out, "container "+c+" (RUNNING"); i < 0 || i > q {
			t.Errorf("%s not named before the question:\n%s", c, r.out)
		}
		rm, vol := r.callIndex("rm", "-f", c), r.callIndex("volume", "rm", obVol)
		if rm < 0 || vol < 0 || rm > vol {
			t.Errorf("%s not removed before the volumes: %v", c, r.calls)
		}
	}
}

// Only the purge looks for them: a plain uninstall of a vps record leaves
// containers it has no business with alone.
func TestUninstallWithoutPurgeDoesNotTouchProjectContainers(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, running: []string{"versola-auth-1"}, project: []string{"versola-auth-1"}, yes: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if r.callIndex("rm", "-f", "versola-auth-1") >= 0 {
		t.Errorf("a plain uninstall removed a container: %v", r.calls)
	}
}

// A write cut off before its rename leaves .tmp-* beside the credentials, maybe
// a partial copy of one: the purge lists it by name, removes it, and never
// prints what is in it (runUninstallCase fails on that).
func TestUninstallPurgeRemovesCutOffWrites(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, files: true, tmpFiles: true, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.out, ".tmp-123456") {
		t.Errorf("the leftover is not named:\n%s", r.out)
	}
	if r.fileExists(".tmp-123456") {
		t.Error("the leftover is still there")
	}
	if !r.fileExists("local.json") {
		t.Error("another target's file was removed")
	}
}

// A leftover alone is something to purge: it is not "nothing more to destroy".
func TestUninstallPurgeCountsCutOffWritesAsSomethingToRemove(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{tmpFiles: true, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if r.fileExists(".tmp-123456") {
		t.Error("the leftover is still there")
	}
}

// What is left of ~/.versola/active after the rest is done: the customer gets
// the one command that finishes it, and is told nothing else remains.
func TestStateDirErrorEndsWithTheCommand(t *testing.T) {
	err := stateDirError("/home/u/.versola/active", &os.PathError{Op: "unlinkat", Path: "/home/u/.versola/active/bundle-1/x.sh", Err: fs.ErrPermission})
	msg := err.Error()
	if !strings.Contains(msg, "already removed") {
		t.Errorf("does not say the rest is done: %s", msg)
	}
	if runtime.GOOS != "windows" && !strings.Contains(msg, "sudo rm -rf '/home/u/.versola/active'") {
		t.Errorf("does not end with the command: %s", msg)
	}
	if strings.Contains(msg, "docker") {
		t.Errorf("a raw docker command in the message: %s", msg)
	}
}

// The customer's path after a failed first `configure vps`: a plain uninstall
// leaves the note of the unfinished install (no record, OpenBao's volume kept),
// so the next configure resumes instead of refusing with unknown-state.
func TestUninstallKeepsTheNoteOfAnUnfinishedFirstInstall(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, pending: true, yes: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	note, err := state.LoadPending()
	if err != nil || note == nil || note.Target != "vps" {
		t.Fatalf("the note is gone after uninstall: %v, %v", note, err)
	}
	if _, err := state.Load(); !errors.Is(err, state.ErrNotConfigured) {
		t.Errorf("uninstall left something that reads as a deployment: %v", err)
	}
	if !r.volumes[obVol] {
		t.Errorf("OpenBao's volume must stay on vps: %v", r.calls)
	}
	if !strings.Contains(r.out, "Kept: the note of the unfinished first install") {
		t.Errorf("the customer is not told:\n%s", r.out)
	}
}

// A note beside a record is what a finished install leaves when removing it
// failed: a live deployment must read as unknown once its record is lost, so
// uninstall removes the note with the record.
func TestUninstallDropsTheNoteOfAFinishedInstall(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{st: vpsState(), volumes: allVol, pending: true, yes: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if note, err := state.LoadPending(); err != nil || note != nil {
		t.Errorf("the note outlived a recorded install: %v, %v", note, err)
	}
	if strings.Contains(r.out, "Kept: the note") {
		t.Errorf("says it kept a note it dropped:\n%s", r.out)
	}
}

// --purge is a fresh start: nothing of the unfinished install stays.
func TestUninstallPurgeDropsTheNote(t *testing.T) {
	r := runUninstallCase(t, uninstallCase{volumes: allVol, files: true, pending: true, answer: "vps\n", purge: true})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if note, err := state.LoadPending(); err != nil || note != nil {
		t.Errorf("the note survived --purge: %v, %v", note, err)
	}
}

// A plain uninstall of a deployment with a Postgres of its own says which one
// it forgets and how to give it back: the record is the only thing that
// remembered it (see deploy.errEmptyBundledPostgres).
func TestUninstallSaysWhichOwnPostgresItForgets(t *testing.T) {
	st := vpsState()
	st.PostgresMode, st.PostgresHost = "", "db.example.com:5432"
	r := runUninstallCase(t, uninstallCase{st: st, volumes: allVol, files: true, answer: "y\n"})
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.out, "--postgres-host db.example.com:5432") {
		t.Errorf("no hint to give the Postgres again:\n%s", r.out)
	}
}

func TestUninstallSaysNothingAboutPostgresWhenThereIsNothingToForget(t *testing.T) {
	bundled := vpsState()
	bundled.PostgresMode, bundled.PostgresHost = deploy.PostgresModeBundled, deploy.BundledPostgresHost
	own := vpsState()
	own.PostgresMode, own.PostgresHost = "", "db.example.com:5432"
	for name, c := range map[string]uninstallCase{
		"bundled":   {st: bundled, volumes: allVol, answer: "y\n"},
		"no record": {volumes: allVol, answer: "y\n"},
		"purge":     {st: own, volumes: allVol, answer: "vps\n", purge: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := runUninstallCase(t, c)
			if r.err != nil {
				t.Fatalf("err %v\n%s", r.err, r.out)
			}
			if strings.Contains(r.out, "To install again over the same data") {
				t.Errorf("a hint where there is nothing to forget:\n%s", r.out)
			}
		})
	}
}
