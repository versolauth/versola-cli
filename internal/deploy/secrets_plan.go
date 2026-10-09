package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/secrets"
	"github.com/versolauth/versola-cli/internal/state"
)

// What `secrets plan` hands versola-tools for the inputs that don't affect any
// secret: the public URL and the Postgres address only end up in the configs,
// which a plan throws away. (versola-tools insists on both for vps.)
const (
	planAuthURL      = "https://plan.invalid"
	planPostgresHost = "127.0.0.1:5432"
)

// PlanSecrets answers, without changing anything, what `configure <target>
// <version>` would do with each secret: it reads what OpenBao holds, runs the
// given release's versola-tools into a temporary directory to get its schema and
// freshly generated candidates, and weighs them against this machine's record
// of its last deployment (see secrets.Plan for the rules).
//
// Read-only: nothing is written to OpenBao, to the deployment's state, or to its
// bundle. The candidates are real random values, so the temporary directory is
// created 0700 under ~/.versola, and removed before this returns whatever
// happens (except a kill -9, which leaves it behind: its name starts with
// "plan-" and it is safe to delete by hand).
//
// Progress goes to log, never to stdout: the caller prints the result.
func PlanSecrets(ctx context.Context, target, version string, generateMissing []string, log io.Writer) (secrets.Result, error) {
	if target != "local" && target != "vps" {
		return secrets.Result{}, fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported today`, target)
	}
	if version == "" {
		return secrets.Result{}, errors.New("a version is required")
	}

	// OpenBao first: it is the cheapest thing to fail on, before anything is pulled.
	creds, err := openbao.LoadCredentials(target)
	if err != nil {
		if errors.Is(err, openbao.ErrNoCredentials) {
			return secrets.Result{}, fmt.Errorf("no OpenBao credentials stored for %q — run `versola secrets login %s <address> <role-id>` first (it prompts for the secret ID separately)", target, target)
		}
		return secrets.Result{}, err
	}
	client := openbao.NewClient(creds)
	fmt.Fprintf(log, "Reading what OpenBao holds for %s...\n", target)
	if err := client.Login(ctx); err != nil {
		return secrets.Result{}, err
	}
	existing := secrets.Values{}
	for _, service := range secretServices {
		data, _, err := client.ReadSecret(ctx, openbao.SecretPath(target, service))
		if err != nil {
			return secrets.Result{}, fmt.Errorf("couldn't read existing %s secrets from OpenBao: %w", service, err)
		}
		existing[service] = data
	}

	// The machine's record, and its note of an unfinished first install.
	prev, installing, err := loadPrevious(target)
	if err != nil {
		return secrets.Result{}, err
	}

	if daemon := checks.DockerDaemon(); !daemon.OK {
		return secrets.Result{}, fmt.Errorf("%s — `secrets plan` runs versola-tools to learn what version %s generates", daemon.String(), version)
	}

	dir, cleanup, err := planDir()
	if err != nil {
		return secrets.Result{}, err
	}
	defer cleanup(log)
	// An interrupt (Ctrl-C), a termination or a hang-up (an SSH session that
	// drops) must not leave the candidates behind: on the first one they are
	// removed and the process exits.
	defer removeOnSignal(log, cleanup)()

	fmt.Fprintf(log, "Running versola-tools %s to get its schema and candidates...\n", version)
	if err := pullAndRunToolsTo(log, dir, ToolsImage(version), target, planAuthURL, planPostgresHost); err != nil {
		if isManifestUnknown(err) {
			return secrets.Result{}, fmt.Errorf(`version %q of Versola doesn't exist (no "versola-tools" image published for it). Versola releases are tagged WITHOUT a leading "v" (e.g. "0.1.2")`, version)
		}
		return secrets.Result{}, fmt.Errorf("versola-tools failed: %w", err)
	}
	// No restrictGeneratedSecretsPerms here: the directory itself is 0700 and is
	// gone before this returns, and that function reports on stdout, which is the
	// report's.

	schema, err := secrets.Load(filepath.Join(dir, secrets.FileName), target)
	if err != nil {
		if errors.Is(err, secrets.ErrNoSchema) {
			return secrets.Result{}, fmt.Errorf("versola-tools %s writes no %s (it predates the secret schema), so there is nothing to plan: configure resolves its secrets by the old rule, an existing value wins", version, secrets.FileName)
		}
		return secrets.Result{}, err
	}
	candidates := secrets.Values{}
	for _, service := range secretServices {
		c, err := readDotenv(filepath.Join(dir, service+".generated-secrets.env"))
		if err != nil {
			return secrets.Result{}, err
		}
		candidates[service] = c
	}

	opts := SecretOptions{GenerateMissing: generateMissing}
	return secrets.Plan(schema, candidates, existing, prev, secrets.Options{
		Target:            target,
		GenerateMissing:   opts.generateSet(),
		InstallInProgress: installing,
	}), nil
}

// loadPrevious is what this machine knows about the target's earlier
// configures: the record of the deployment (nil if none) and whether a first
// install of this target was begun and not finished. A broken or
// newer-than-this-CLI state, or an unreadable note, is an error: guessing "none"
// there would make a plan out of nothing.
func loadPrevious(target string) (prev *secrets.Previous, installing bool, err error) {
	switch st, err := state.Load(); {
	case err == nil:
		prev = &secrets.Previous{Target: st.Target, Revision: st.EffectiveSecretsRevision()}
	case errors.Is(err, state.ErrNotConfigured):
	default:
		return nil, false, err
	}
	note, err := state.LoadPending()
	if err != nil {
		return nil, false, err
	}
	return prev, note != nil && note.Target == target, nil
}

// planDir makes the 0700 temporary directory under ~/.versola that holds one
// plan's versola-tools output, and the function that removes it. It sits next to
// the deployment's own directories rather than under the OS temp directory so
// Docker can bind-mount it wherever it can mount those (Docker Desktop shares
// the user's profile, not necessarily the temp directory).
func planDir() (dir string, cleanup func(io.Writer), err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("couldn't find home directory: %w", err)
	}
	base := filepath.Join(home, ".versola")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("couldn't create %s: %w", base, err)
	}
	sweepStalePlanDirs(base, stalePlanAge)
	dir, err = os.MkdirTemp(base, "plan-")
	if err != nil {
		return "", nil, fmt.Errorf("couldn't create a directory for the plan in %s: %w", base, err)
	}
	return dir, func(log io.Writer) {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(log, "(couldn't remove %s: %v — it holds generated secret candidates that are never used; delete it by hand)\n", dir, err)
		}
	}, nil
}

// stalePlanAge is how old a plan-* directory has to be before the next plan
// removes it as left behind by a run that was killed. Far longer than a plan takes.
const stalePlanAge = time.Hour

// sweepStalePlanDirs removes plan-* directories in base last modified more than
// maxAge ago: what a plan killed with SIGKILL (which nothing can handle) leaves.
// Best effort; a directory it can't remove is simply tried again next time.
func sweepStalePlanDirs(base string, maxAge time.Duration) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "plan-") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(base, e.Name()))
	}
}

// removeOnSignal runs cleanup and exits with the conventional 128+signal status
// when the process is interrupted or terminated; the returned function stops
// watching. Swallowing the signal without acting on it would be worse than not
// handling it: a hung `docker run` could then not be interrupted at all.
func removeOnSignal(log io.Writer, cleanup func(io.Writer)) (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			cleanup(log)
			code := 130
			if sig == syscall.SIGTERM {
				code = 143
			} else if sig == syscall.SIGHUP {
				code = 129
			}
			os.Exit(code)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}
