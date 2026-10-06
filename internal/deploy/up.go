package deploy

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/browser"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
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

// Up starts the stack described by `st` and waits until it's actually
// serving traffic, not merely started.
//
// Takes an already-loaded state rather than reading its own -- same
// reasoning as deploy.Migrate's own comment: this is normally the
// deployment a caller (cmd/up.go) just confirmed with the operator, or the
// one bootstrap.go's own Configure/Migrate just acted on, and re-reading
// state here instead would leave a window for a concurrent `configure` to
// finalize a different deployment into the gap (flagged in review). `st ==
// nil` means the caller found nothing configured (see confirmIfVpsState).
func Up(opts UpOptions, st *state.State) error {
	if st == nil {
		return fmt.Errorf("nothing has been configured yet — run `versola bootstrap local <version>` first")
	}

	composePath, exists, err := st.ComposeFilePath()
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

	// Where each service listens -- and so where it reports readiness --
	// comes from the compose file, with the replica slots recorded in
	// state (a deployment made before slots existed is one replica of
	// auth and of edge, in slot 1).
	topo, err := loadTopology(composePath)
	if err != nil {
		return err
	}
	centralReplicas, err := replicasOf(topo, CentralService, []state.Slot{{N: 1, Version: st.Version}})
	if err != nil {
		return err
	}
	authReplicas, err := replicasOf(topo, AuthService, st.ActiveSlots(AuthService))
	if err != nil {
		return err
	}
	edgeReplicas, err := replicasOf(topo, EdgeService, st.ActiveSlots(EdgeService))
	if err != nil {
		return err
	}
	// Ports and readiness URLs up front: a clash, or a diagnostics port
	// that cannot be asked, is found before anything is started, not after
	// central's migrations.
	readyURLs, err := checkTopology(topo, isVps, deployedProxyReservations(st), centralReplicas, authReplicas, edgeReplicas)
	if err != nil {
		return err
	}
	if len(authReplicas) > 1 || len(edgeReplicas) > 1 {
		if err := requireOverrideSupport(); err != nil {
			return err
		}
	}
	// Replicas past slot 1 are defined in replicas.yml next to the compose
	// file (see replicasYAML); written before anything is started so the
	// compose commands below see them.
	if err := writeReplicasFile(dir, filepath.Base(composePath), authReplicas, edgeReplicas); err != nil {
		return err
	}
	steps := appStartSteps(authReplicas, edgeReplicas, !(isVps || st.ProxyMode != ""))

	// A warning, not a hard failure: the services themselves are the real
	// check (they validate their schema at startup now rather than silently
	// running against whatever's there -- see RUN_MIGRATIONS in
	// compose.fragment.yml.template), and this record can legitimately be
	// absent for a deployment made before it existed. But when it IS absent,
	// saying so here turns "central won't start and I don't know why" into
	// one line read before the failure rather than after it.
	if st.MigratedAt == nil {
		fmt.Println("\nNote: no migration has been recorded for this deployment — if `versola migrate` hasn't run, the services will refuse to start against an out-of-date schema.")
	}

	// The vps confirmation doesn't live here: Configure's own Finalize
	// (which overwrites state.json to point at the new deployment and
	// deletes the previous bundle -- see its comment) runs to completion
	// before Up is ever called, so by this point that's already
	// irreversible -- declining here, or Up failing partway, couldn't
	// actually get anyone back to the old deployment record even though
	// the old containers might still be the ones really serving traffic
	// (flagged in review on versolauth/versola-cli#7). cmd/bootstrap.go
	// asks once, before Configure runs at all; cmd/up.go asks again on its
	// own when Up is run standalone (see ConfirmVpsDeploy's own comment on
	// why one fixed prompt can't cover both callers).

	// The compose file declares this volume `external: true` (see the
	// comment on it in compose.fragment.yml.template) so OpenBao's storage
	// survives being reconfigured into a fresh bundle directory each run —
	// but `external: true` also means compose refuses to start anything
	// that mounts it until it already exists. `docker volume create` is a
	// no-op if it's already there, so this is safe to run on every `up`,
	// not just the first one.
	if err := docker.Run("volume", "create", OpenbaoVolumeName(st.Target)); err != nil {
		return fmt.Errorf("couldn't create the openbao-file volume: %w", err)
	}

	// From here on containers may be (re)started from this bundle -- keep
	// it from being deleted by a later configure even if this `up` fails
	// halfway (see state.MarkStarting).
	if err := state.MarkStarting(); err != nil {
		return fmt.Errorf("couldn't record the deployment being started: %w", err)
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
		if err := docker.Run(state.ComposeArgs(composePath, "up", "-d", "central")...); err != nil {
			return fmt.Errorf("couldn't start central: %w", err)
		}
	} else {
		fmt.Println("\nStarting Postgres and central...")
		if err := docker.Run(state.ComposeArgs(composePath, "up", "-d", "postgres", "central")...); err != nil {
			return fmt.Errorf("couldn't start postgres/central: %w", err)
		}
	}

	// The readiness URL is read from the compose file (see loadTopology)
	// rather than written down here: this CLI is meant not to know
	// Versola's topology, so that one build of it can deploy any release
	// (design doc §3.5). The service names are what is still assumed.
	fmt.Println("Waiting for central to be ready...")
	if err := wait.ForReady(readyURLs[centralReplicas[0].Service], 60*time.Second); err != nil {
		return fmt.Errorf("central never became ready: %w", err)
	}

	// Named explicitly, not a bare "up -d" ("start everything not already
	// running under this project") -- openbao is deliberately left out of
	// that: Configure starts it (or leaves an already-running one alone —
	// see the comment there) under whatever compose project happened to
	// start it, which may not be this bundle's. A bare "up -d" here would
	// see openbao defined in this project's compose file but not part of
	// this project, and try to create a second container under its fixed
	// container_name, which Docker refuses. The reverse proxy isn't in
	// versola-tools' compose file at all -- versola-cli generates it
	// (proxy.yml, see package proxy) and startProxy below starts it last.
	// One step per batch: slot 1 of each service together, as before
	// (with its depends_on), then every further replica on its own and
	// waited for before the next -- a replica that does not come up stops
	// the rollout at that replica instead of after all of them.
	for _, step := range steps {
		fmt.Println(step.Message)
		if err := docker.Run(state.ComposeArgs(composePath, step.Args...)...); err != nil {
			return fmt.Errorf("couldn't start %s: %w", step.Name, err)
		}
		for _, svc := range step.Ready {
			if err := wait.ForReady(readyURLs[svc], 60*time.Second); err != nil {
				return fmt.Errorf("%s never became ready: %w", svc, err)
			}
		}
	}

	// The reverse proxy versola-cli generated at configure time (see
	// package proxy) -- started last, once what it routes to is ready, so
	// it never serves traffic to a backend that isn't. Deployments
	// configured before versola-cli generated it have no ProxyMode: vps
	// ones keep relying on a proxy set up by hand, local ones on the
	// gateway started above. A local one moving to the proxy first loses
	// that gateway, which holds the same port.
	if st.ProxyMode != "" {
		if !isVps {
			if err := removeLegacyGateway(); err != nil {
				return err
			}
		}
		// The upstreams follow the replicas recorded in state, whatever
		// configure or the last `replica add|remove` wrote.
		if proxy.HasUpstreamsFile(dir) {
			if err := proxy.WriteUpstreams(dir, proxy.Config{Mode: st.ProxyMode, Auth: proxyBackends(authReplicas), Edge: proxyBackends(edgeReplicas)}); err != nil {
				return fmt.Errorf("couldn't write the reverse proxy's upstreams: %w", err)
			}
		} else if len(authReplicas) > 1 || len(edgeReplicas) > 1 {
			// Configured by an older versola-cli: upstreams inline in
			// versola.conf, for one replica each.
			return fmt.Errorf("this deployment's reverse proxy config predates replicas -- run `versola configure` again")
		}
		if err := startProxy(composePath, st); err != nil {
			return err
		}
	}

	// Everything is up and serving: the stack now runs from this
	// deployment's bundle, and the one it ran from before can go.
	markRunning()

	if isVps {
		// st.AuthURL is whatever --auth-url Configure was given (see
		// state.Finalize) -- not hardcoded here anymore, since that broke
		// for any deployment other than the one original VPS this used to
		// assume (flagged in review on versolauth/versola-cli#7). Older
		// deployments recorded before AuthURL existed will have it empty;
		// falling back to the compose-generated auth.conf's own issuer
		// would need reading and parsing that file just for a print
		// statement, so this just says so plainly instead.
		authURL := st.AuthURL
		if authURL == "" {
			authURL = "(unknown -- reconfigure to record it)"
		}
		fmt.Printf("\nVersola %s is running at %s\n", st.Version, authURL)
		if st.ProxyMode == proxy.ModeExternal {
			fmt.Printf("It listens on http://127.0.0.1:%d -- your own web server has to forward %s there, keeping the Host header, setting X-Forwarded-For and X-Forwarded-Proto ($proxy_add_x_forwarded_for and $scheme in nginx) and allowing 8m request bodies (client_max_body_size 8m).\n", proxy.ExternalPort, authURL)
		}
		// vps doesn't use a fixed literal password the way local's
		// "Admin1234!" is — it's a real, standing admin credential Configure
		// resolved against OpenBao (see gen-env.scala's
		// bootstrapPasswordDefault), not a throwaway dev one. Deliberately
		// NOT echoed to stdout here the way local's fixed password is below:
		// unlike local, this runs on every redeploy of the same target, not
		// just the first, and a real production credential printed to a
		// terminal on every run (often over SSH, sometimes logged or
		// recorded) is needless repeated exposure for something that's
		// already sitting in auth.secrets.env for whoever's actually
		// authorized to read it.
		fmt.Printf("Login: admin / (see ADMIN_BOOTSTRAP_PASSWORD in %s)\n", filepath.Join(dir, "auth.secrets.env"))
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

// ConfirmVpsDeploy asks for an explicit go-ahead before anything touches
// a real VPS deployment. action is the already-formatted sentence
// describing what this particular call is actually about to do --
// configure/migrate/up are separate commands now (see cmd/configure.go,
// cmd/migrate.go, cmd/up.go), each with a different real effect on a
// live deployment, so one fixed message here would be wrong for at least
// two of the three callers. bootstrap.go calls this once, before
// Configure runs at all (see its own comment on why not later), covering
// the whole chain in one prompt; cmd/migrate.go and cmd/up.go each call
// it again themselves when run standalone, since nothing then guarantees
// the same process just showed one moments ago.
func ConfirmVpsDeploy(action string) error {
	fmt.Printf("\nThis will %s.\n", action)
	fmt.Print("Continue? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

// markRunning records that the stack now runs from this deployment's
// bundle, which lets the previous one be removed (see
// state.MarkRunning). Not fatal: the deployment is up either way, and the
// worst case is an old bundle directory left on disk.
func markRunning() {
	if err := state.MarkRunning(openbaoBundles()...); err != nil {
		fmt.Printf("(couldn't record which deployment is running: %v)\n", err)
	}
}
