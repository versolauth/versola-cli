package deploy

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

// UpOptions are the choices Up leaves to the caller. They're a struct
// rather than plain parameters because the list only grows from here (a
// deployment that skips the browser today will also want to skip parts of
// the stack, pick a target, and so on), and a growing list of bare
// booleans at a call site stops being readable very quickly.
type UpOptions struct {
	// NoBrowser suppresses opening the admin console once it's ready.
	NoBrowser bool
}

// Up starts the stack that Configure prepared and waits until it's
// actually serving traffic, not merely started.
//
// It reads what to start from the deployment directory rather than taking
// a version argument: by this point the version is a property of what's
// on disk, and re-stating it here would make it possible to ask for one
// version and get another.
func Up(opts UpOptions) error {
	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return fmt.Errorf("nothing has been configured yet — run `versola bootstrap local <version>` first")
		}
		return err
	}

	composePath, exists, err := state.ComposeFile()
	if err != nil {
		return err
	}
	if !exists {
		// State exists but the compose file doesn't: configure didn't
		// finish. Say so plainly rather than letting docker compose fail
		// on a missing file, which reads like a bug in the CLI.
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}
	dir := filepath.Dir(composePath)
	isVps := st.Target == "vps"

	// vps is the one target where this touches a real, shared database —
	// central runs migrations against it as a side effect of starting up
	// (see the comment on state.State.MigratedAt: there's no separate
	// migrate step yet to stop and review first). Everything before this
	// point (Configure, OpenBao, secret resolution) is safe to redo; this
	// is the last point it's still safe to back out of before that
	// changes.
	if isVps {
		if err := confirmVpsDeploy(st.Version); err != nil {
			return err
		}
	}

	// The compose file declares this volume `external: true` (see the
	// comment on it in compose.fragment.yml.template) so OpenBao's storage
	// survives being reconfigured into a fresh bundle directory each run —
	// but `external: true` also means compose refuses to start anything
	// that mounts it until it already exists. `docker volume create` is a
	// no-op if it's already there, so this is safe to run on every `up`,
	// not just the first one.
	if err := docker.Run("volume", "create", "versola-openbao-file"); err != nil {
		return fmt.Errorf("couldn't create the openbao-file volume: %w", err)
	}

	// Postgres and central go up first, on their own — auth/edge's own
	// startup fails fatally if central isn't reachable yet, and confirmed
	// by hand that Docker's restart policy does NOT recover from that
	// failure mode (the process hangs rather than exiting). See the
	// comment on auth's depends_on in
	// versola-tools/compose.fragment.yml.template. vps has no postgres
	// service at all — its Postgres is native on the host (see
	// compose.fragment.vps.yml.template's comment), not something this
	// compose file starts.
	if isVps {
		fmt.Println("\nStarting central...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "central"); err != nil {
			return fmt.Errorf("couldn't start central: %w", err)
		}
	} else {
		fmt.Println("\nStarting Postgres and central...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "postgres", "central"); err != nil {
			return fmt.Errorf("couldn't start postgres/central: %w", err)
		}
	}

	// These URLs, like the service names above and the port checked in
	// Configure, are still hardcoded here. They're deployment facts that
	// by rights belong in the bundle versola-tools generates -- this CLI
	// is meant not to know Versola's topology, so that one build of it can
	// deploy any release (design doc §3.5). Hardcoding them was tolerable
	// while "local" was the only target; vps happens to use the exact
	// same ports (see deploy.md's table), which is the only reason this
	// hasn't forced the issue yet.
	fmt.Println("Waiting for central to be ready...")
	if err := wait.ForReady("http://localhost:8091/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("central never became ready: %w", err)
	}

	// Named explicitly, not a bare "up -d" ("start everything not already
	// running under this project") -- openbao is deliberately left out of
	// that: Configure starts it (or leaves an already-running one alone —
	// see the comment there) under whatever compose project happened to
	// start it, which may not be this bundle's. A bare "up -d" here would
	// see openbao defined in this project's compose file but not part of
	// this project, and try to create a second container under its fixed
	// container_name, which Docker refuses. vps has no nginx/gateway
	// service — its public nginx is native on the VPS, deployed by a
	// separate pipeline (see compose.fragment.vps.yml.template's comment).
	if isVps {
		fmt.Println("Starting auth and edge...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "auth", "edge"); err != nil {
			return fmt.Errorf("couldn't start auth/edge: %w", err)
		}
	} else {
		fmt.Println("Starting auth, edge, and the gateway...")
		if err := docker.Run("compose", "-f", composePath, "up", "-d", "auth", "edge", "nginx"); err != nil {
			return fmt.Errorf("couldn't start the rest of the stack: %w", err)
		}
	}

	fmt.Println("Waiting for auth and edge to be ready...")
	if err := wait.ForReady("http://localhost:8081/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("auth never became ready: %w", err)
	}
	if err := wait.ForReady("http://localhost:8096/readiness", 60*time.Second); err != nil {
		return fmt.Errorf("edge never became ready: %w", err)
	}

	if isVps {
		// id.versola.kz, like the rest of vps's addressing (see
		// gen-env.scala's vps branch), is a fact about the one real VPS
		// this deploys to, not a general "vps" concept — hardcoded here
		// for the same reason it's hardcoded there.
		fmt.Printf("\nVersola %s is running at https://id.versola.kz\n", st.Version)
		// vps doesn't use a fixed literal password the way local's
		// "Admin1234!" is — it's a real secret Configure resolved against
		// OpenBao (see gen-env.scala's bootstrapPasswordDefault). Reading
		// it back out of the resolved secrets file is the only way to
		// show it, since nothing here ever held it directly.
		password, err := adminBootstrapPassword(dir)
		if err != nil {
			fmt.Printf("Login: admin / (couldn't read the bootstrap password: %v — see auth.secrets.env in %s)\n", err, dir)
		} else {
			fmt.Printf("Login: admin / %s\n", password)
		}
		// No browser.Open here, unlike local below: this CLI runs on the
		// VPS itself (typically over SSH), not on the operator's own
		// desktop -- popping a browser window on the server wouldn't
		// reach anyone.
		return nil
	}

	adminURL := "http://localhost:2821/central/admin/"
	fmt.Printf("\nVersola %s is running at http://localhost:2821\n", st.Version)
	fmt.Println("Login: admin / Admin1234!")

	if !opts.NoBrowser {
		// Best-effort only: the deployment having succeeded shouldn't
		// hinge on a desktop environment being around to pop a browser
		// window in (e.g. running over SSH) — failure here is a note, not
		// an error.
		if err := browser.Open(adminURL); err != nil {
			fmt.Printf("(couldn't open a browser automatically: %v — open %s yourself)\n", err, adminURL)
		}
	}
	return nil
}

// confirmVpsDeploy asks for an explicit go-ahead before Up touches the
// real VPS deployment — see the comment on its call site. There's no
// separate migrate step yet to review before this (see
// state.State.MigratedAt's comment), so this is the only checkpoint
// before central runs migrations against the live database as a side
// effect of starting.
func confirmVpsDeploy(version string) error {
	fmt.Printf("\nThis will deploy Versola %s to the VPS, including running database migrations against the live database.\n", version)
	fmt.Print("Continue? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

// adminBootstrapPassword reads the real admin bootstrap password back out
// of auth.secrets.env, which Configure's resolveSecrets already resolved
// against OpenBao (an existing value there wins over a freshly generated
// one — see gen-env.scala's bootstrapPasswordDefault for why that's safe
// to do unattended). This is the only place that value exists by the time
// Up runs; nothing keeps it in memory across the two.
func adminBootstrapPassword(dir string) (string, error) {
	values, err := readDotenv(filepath.Join(dir, "auth.secrets.env"))
	if err != nil {
		return "", err
	}
	password, ok := values["ADMIN_BOOTSTRAP_PASSWORD"]
	if !ok {
		return "", fmt.Errorf("ADMIN_BOOTSTRAP_PASSWORD not found in auth.secrets.env")
	}
	return password, nil
}
