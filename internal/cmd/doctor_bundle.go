package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// What the diagnostics archive asks Docker, replaceable in tests.
var (
	diagDocker = func(timeout time.Duration, args ...string) ([]byte, string, error) {
		return docker.OutputWithin(timeout, args...)
	}
	diagLogs     = func(w io.Writer, args ...string) (string, error) { return docker.RunTo(w, args...) }
	diagServices = deploy.DeploymentServices
)

// diagGeneratedFiles are the files of the deployment's generated bundle that
// go into the archive: the compose files and the release manifest. A name list,
// not "whatever is there": the bundle also holds the *.secrets.env files, which
// never go in.
var diagGeneratedFiles = []string{"compose.yml", "replicas.yml", "proxy.yml", "release.json"}

// writeDiagBundle makes the archive `doctor --bundle` names, with doctorOut (what
// doctor printed) in it, and says what it holds.
func writeDiagBundle(out io.Writer, doctorOut string, dockerOK bool) error {
	st, stErr := state.Load()
	composePath, exists, composeErr := state.ComposeFile()
	meta := deploy.DiagMeta{CLIVersion: version, OS: runtime.GOOS, Arch: runtime.GOARCH, LogsIncluded: doctorLogs}
	if stErr == nil {
		meta.Target, meta.VersolaVersion = st.Target, st.Version
	}
	sections := diagSections(doctorOut, st, stErr, composePath, exists, composeErr, doctorLogs, dockerOK)
	path, m, err := deploy.WriteDiagBundle(doctorBundle, meta, sections, time.Now)
	if err != nil {
		return err
	}
	deploy.PrintDiagSummary(out, path, m)
	return nil
}

// diagSections are the members of the archive, in order. st is the deployment's
// record (nil when stErr says why it cannot be read); composePath and exists
// say whether its bundle is on this machine.
func diagSections(doctorOut string, st *state.State, stErr error, composePath string, exists bool, composeErr error, withLogs, dockerOK bool) []deploy.DiagSection {
	none := func(what string) error { return deploy.DiagNone(what) }
	// Why there is no bundle to read: nothing is deployed, or a record is
	// there and its files are not.
	noBundle := "no deployment"
	if stErr == nil && !exists {
		noBundle = "the deployment's bundle files are not on this machine"
		if composeErr != nil {
			noBundle += ": " + diagFirstLine(composeErr.Error())
		}
	}
	// Docker did not answer doctor's own check: what needs it is skipped, not
	// waited on (a daemon that accepts a connection and never replies would hold
	// the whole archive).
	noDocker := "Docker did not answer within doctor's check (see doctor.txt)"
	sections := []deploy.DiagSection{
		{Name: "doctor.txt", ScrubLight: true, Run: func(w io.Writer) error { _, err := io.WriteString(w, doctorOut); return err }},
		{Name: "versions.txt", Run: diagVersions},
		{Name: "state.json", ScrubLight: true, Run: func(w io.Writer) error {
			if stErr != nil {
				return none("the deployment's record: " + stErr.Error())
			}
			dir, err := state.Dir()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				return err
			}
			_, err = w.Write(b)
			return err
		}},
		{Name: "status.json", ScrubLight: true, Run: func(w io.Writer) error {
			if exists && !dockerOK {
				return none(noDocker)
			}
			return writeStatusJSON(w, composePath, exists)
		}},
		{Name: "stats.json", ScrubLight: true, Run: func(w io.Writer) error {
			if !exists {
				return none(noBundle)
			}
			if !dockerOK {
				return none(noDocker)
			}
			return writeStats(w, io.Discard, true)
		}},
	}
	for _, name := range diagGeneratedFiles {
		sections = append(sections, deploy.DiagSection{Name: "files/" + name, Scrub: true, Run: func(w io.Writer) error {
			if !exists {
				return none(noBundle)
			}
			p := filepath.Join(filepath.Dir(composePath), name)
			// A plain file only, and never more than the section cap: a link
			// in the bundle (to OpenBao's credentials, say) is not followed.
			fi, err := os.Lstat(p)
			if errors.Is(err, os.ErrNotExist) {
				return none(name + " is not in the deployment's bundle")
			}
			if err != nil {
				return err
			}
			if !fi.Mode().IsRegular() {
				return none(name + " is not a plain file")
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, io.LimitReader(f, 8<<20))
			return err
		}})
	}
	if withLogs {
		sections = append(sections, diagLogSections(composePath, exists, dockerOK, noBundle, noDocker)...)
	}
	return sections
}

// diagVersions are the versions in play: this CLI, the machine, Docker and
// Compose. A failing docker call is a line in the file, not a missing file.
func diagVersions(w io.Writer) error {
	fmt.Fprintf(w, "versola %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "PRETTY_NAME=") {
				fmt.Fprintf(w, "os %s\n", strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`))
			}
		}
	}
	// The client's version needs no daemon, so it is there when the daemon is not.
	for _, c := range []struct {
		label string
		args  []string
	}{
		{"docker client", []string{"--version"}},
		{"docker server", []string{"version", "--format", "{{.Server.Version}}"}},
		{"docker compose", []string{"compose", "version", "--short"}},
	} {
		out, stderr, err := diagDocker(30*time.Second, c.args...)
		if err != nil {
			why := firstLineOf(stderr)
			if why == "" {
				why = ": docker is not on PATH, or did not answer"
			}
			fmt.Fprintf(w, "%s: could not be asked%s\n", c.label, why)
			continue
		}
		fmt.Fprintf(w, "%s %s\n", c.label, strings.TrimSpace(string(out)))
	}
	return nil
}

// diagFirstLine is the first line of s, cut short and printable.
func diagFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

// firstLineOf is ": <first line>" of s, or "".
func firstLineOf(s string) string {
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 160 {
				l = l[:160]
			}
			return ": " + l
		}
	}
	return ""
}

// diagLogSections are the services' last lines, one file each: `docker compose
// logs` as `versola logs` runs it, with timestamps. Not scrubbed, and said so.
func diagLogSections(composePath string, exists, dockerOK bool, noBundle, noDocker string) []deploy.DiagSection {
	if !exists {
		return []deploy.DiagSection{{Name: "logs", Run: func(io.Writer) error { return deploy.DiagNone(noBundle) }}}
	}
	if !dockerOK {
		return []deploy.DiagSection{{Name: "logs", Run: func(io.Writer) error { return deploy.DiagNone(noDocker) }}}
	}
	services, err := diagServices(composePath)
	if err != nil {
		return []deploy.DiagSection{{Name: "logs", Run: func(io.Writer) error { return err }}}
	}
	var out []deploy.DiagSection
	for _, svc := range services {
		if !diagServiceName(svc) {
			continue
		}
		args, err := deploy.LogsArgs(composePath, deploy.LogsOptions{Services: []string{svc}, Timestamps: true})
		out = append(out, deploy.DiagSection{Name: "logs/" + svc + ".log", ScrubLog: true, Run: func(w io.Writer) error {
			if err != nil {
				return err
			}
			// Compose's own stderr is not quoted: it parses the env files, and
			// its parse errors can repeat the line that is wrong.
			if _, rerr := diagLogs(w, args...); rerr != nil {
				return fmt.Errorf("docker compose logs failed (%v)", rerr)
			}
			return nil
		}})
	}
	return out
}

// diagServiceName accepts a compose service name as a file name: letters,
// digits, dot, dash and underscore only.
func diagServiceName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
