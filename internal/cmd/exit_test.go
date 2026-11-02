package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/exitcode"
)

// runIsolated is Run with stderr captured and the home directory pointed at
// an empty one: every case below is refused before anything is read or
// locked, and should one stop being refused, it still never reaches the real
// ~/.versola.
func runIsolated(t *testing.T, args ...string) (int, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	var stderr bytes.Buffer
	code := Run(args, &stderr)
	return code, stderr.String()
}

func TestRunUsageErrorsExit2(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // the error line, printed once
	}{
		{"unknown command", []string{"no-such-command"}, `unknown command "no-such-command" for "versola"`},
		{"unknown flag", []string{"status", "--no-such-flag"}, "unknown flag: --no-such-flag"},
		{"bad flag value", []string{"configure", "local", "0.1.0", "--yes=maybe"}, `invalid argument "maybe" for "-y, --yes" flag`},
		// `configure local` alone is a valid command since the version became optional on
		// local, and this test would run it for real: the cases here must be refused
		// before anything starts.
		{"too few arguments", []string{"configure"}, "accepts between 1 and 2 arg(s), received 0"},
		{"too many arguments for configure", []string{"configure", "local", "0.1.0", "extra"}, "accepts between 1 and 2 arg(s), received 3"},
		{"too many arguments", []string{"migrate", "extra"}, `unknown command "extra" for "versola migrate"`},
		// Checked first, before the lock and any question (deploy.CheckTarget).
		{"unknown target", []string{"configure", "staging", "0.1.0"}, `unsupported target "staging"`},
		{"unknown target bootstrap", []string{"bootstrap", "staging", "0.1.0"}, `unsupported target "staging"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stderr := runIsolated(t, c.args...)
			if code != exitcode.Usage {
				t.Errorf("exit code %d, want %d; stderr:\n%s", code, exitcode.Usage, stderr)
			}
			if n := strings.Count(stderr, c.want); n != 1 {
				t.Errorf("%q printed %d times, want once; stderr:\n%s", c.want, n, stderr)
			}
			if strings.Contains(stderr, "Error:") {
				t.Errorf("cobra printed the error too:\n%s", stderr)
			}
		})
	}
}

func TestRunUnknownCommandKeepsHint(t *testing.T) {
	_, stderr := runIsolated(t, "no-such-command")
	if !strings.Contains(stderr, "Run 'versola --help' for usage.") {
		t.Errorf("the way out is missing:\n%s", stderr)
	}
}

// A failing command's error is printed once (cobra's own print is silenced),
// with its text unchanged, and an unclassified one exits 1; a Reported one is
// not printed at all, since the command already said it.
func TestRunPrintsErrorOnce(t *testing.T) {
	var fail error
	probe := &cobra.Command{
		Use:    "exit-test-probe",
		Hidden: true,
		RunE:   func(*cobra.Command, []string) error { return fail },
	}
	rootCmd.AddCommand(probe)
	t.Cleanup(func() { rootCmd.RemoveCommand(probe) })

	for _, c := range []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{"success", nil, exitcode.OK, ""},
		{"unclassified", errors.New("something broke"), exitcode.Failure, "something broke\n"},
		{"classified and wrapped", fmt.Errorf("rollout: %w", exitcode.With(exitcode.PlanProblems, errors.New("the secrets plan has 1 problem(s)"))), exitcode.PlanProblems, "rollout: the secrets plan has 1 problem(s)\n"},
		{"aborted", exitcode.With(exitcode.Aborted, errors.New("aborted")), exitcode.Aborted, "aborted\n"},
		{"reported", exitcode.Reported(exitcode.Aborted, errors.New("aborted")), exitcode.Aborted, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fail = c.err
			code, stderr := runIsolated(t, "exit-test-probe")
			if code != c.code {
				t.Errorf("exit code %d, want %d", code, c.code)
			}
			if stderr != c.stderr {
				t.Errorf("stderr = %q, want %q", stderr, c.stderr)
			}
		})
	}
}

// Representative errors from the places that classify them, so a change that
// drops a class (or wraps with %v) shows up here.
func TestExitClasses(t *testing.T) {
	days := 30
	_, _, replicaErr := parseReplicaArgs([]string{"central"})
	_, _, countErr := parseReplicaArgs([]string{"auth", "0"})
	_, certDaysErr := parseCertDays("forever")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown target", deploy.CheckTarget("staging"), exitcode.Usage},
		{"known target", deploy.CheckTarget("vps"), exitcode.OK},
		{"replica of a service without replicas", replicaErr, exitcode.Usage},
		{"replica count", countErr, exitcode.Usage},
		{"cert lifetime", certDaysErr, exitcode.Usage},
		{"--no-certs-timer with finite certificates", refuseNoTimerWhenFinite(true, &days), exitcode.Usage},
		{"wrapped with %w", fmt.Errorf("configure: %w", deploy.CheckTarget("staging")), exitcode.Usage},
		{"unclassified", errors.New("docker compose up failed"), exitcode.Failure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitcode.Code(c.err); got != c.want {
				t.Errorf("exit code %d, want %d (err=%v)", got, c.want, c.err)
			}
		})
	}
}

// The four vps flags local used to accept and ignore are refused on
// configure local and bootstrap local, as usage errors, with what local does
// instead. Set on the real commands, so a flag renamed on one of them fails
// here.
func TestLocalRefusesIgnoredVpsFlags(t *testing.T) {
	values := map[string]string{
		"auth-url":       "https://id.example.com",
		"postgres-host":  "127.0.0.1:5432",
		"setup-openbao":  "true",
		"no-certs-timer": "true",
	}
	wantInstead := map[string]string{
		"auth-url":       "local always runs at http://localhost:2821",
		"postgres-host":  "local always runs its own Postgres",
		"setup-openbao":  "local always sets up its own OpenBao",
		"no-certs-timer": "local installs no renewal timer",
	}
	for _, c := range []*cobra.Command{configureCmd, bootstrapCmd} {
		for name, value := range values {
			t.Run(c.Name()+" --"+name, func(t *testing.T) {
				f := c.Flags().Lookup(name)
				if f == nil {
					t.Fatalf("%s has no --%s", c.Name(), name)
				}
				if err := c.Flags().Set(name, value); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = f.Value.Set(f.DefValue)
					f.Changed = false
				})
				err := rejectVpsOnlyFlags(c, "local")
				want := "--" + name + " only applies to vps deployments (" + wantInstead[name]
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("err = %v, want %q...", err, want)
				}
				if code := exitcode.Code(err); code != exitcode.Usage {
					t.Errorf("exit code %d, want %d", code, exitcode.Usage)
				}
				if err := rejectVpsOnlyFlags(c, "vps"); err != nil {
					t.Errorf("refused on vps: %v", err)
				}
			})
		}
	}
}

// Flags that mean the same on both targets stay accepted on local.
func TestLocalAcceptsSharedFlags(t *testing.T) {
	for _, c := range []*cobra.Command{configureCmd, bootstrapCmd} {
		for name, value := range map[string]string{
			"registry":             "cr.example.com/x",
			"generate-missing":     "A",
			"allow-legacy-secrets": "true",
			"yes":                  "true",
		} {
			f := c.Flags().Lookup(name)
			if f == nil {
				t.Fatalf("%s has no --%s", c.Name(), name)
			}
			before := f.Value.String()
			if err := c.Flags().Set(name, value); err != nil {
				t.Fatal(err)
			}
			err := rejectVpsOnlyFlags(c, "local")
			// A slice flag's Set appends; Replace puts the old value back.
			if sv, ok := f.Value.(interface{ Replace([]string) error }); ok {
				_ = sv.Replace(nil)
			} else {
				_ = f.Value.Set(before)
			}
			f.Changed = false
			if err != nil {
				t.Errorf("%s local --%s refused: %v", c.Name(), name, err)
			}
		}
	}
}

// Test in a fresh CLI process: earlier Run calls otherwise create Cobra's
// default completion commands and hide a missed first-invocation wrapper.
func TestExitCompletionProcess(t *testing.T) {
	if os.Getenv("VERSOLA_TEST_EXIT_COMPLETION") != "1" {
		return
	}
	for _, c := range rootCmd.Commands() {
		if c.Name() == "completion" {
			t.Fatal("completion already initialized before the first Run")
		}
	}
	os.Exit(Run([]string{"completion", "bash", "extra"}, os.Stderr))
}

func TestFirstInvocationCompletionUsageExits2(t *testing.T) {
	c := exec.Command(os.Args[0], "-test.run=^TestExitCompletionProcess$")
	c.Env = append(os.Environ(), "VERSOLA_TEST_EXIT_COMPLETION=1")
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitcode.Usage {
		t.Fatalf("exit error=%v, want code 2; output:\n%s", err, out)
	}
	want := `unknown command "extra" for "versola completion bash"`
	if strings.Count(string(out), want) != 1 || strings.Contains(string(out), "Error:") {
		t.Fatalf("want error once, got:\n%s", out)
	}
}
