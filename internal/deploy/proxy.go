package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// ProxyOptions: how a vps deployment's reverse proxy is set up (see
// package proxy). For local, Configure sets proxy.ModeLocal itself.
type ProxyOptions struct {
	// Mode is proxy.ModeNginx (the proxy owns 80/443 and does TLS) or
	// proxy.ModeExternal (behind the host's existing web server).
	Mode string
	// ACMEStaging requests the certificate from Let's Encrypt's staging
	// environment: untrusted, but without production's rate limits.
	ACMEStaging bool
	// CentralURL is --central-url: how edge reaches central's mutual-TLS
	// listener. Empty: central.<domain> of the auth URL where there is one.
	CentralURL string
}

// checkProxyPorts fails early, before anything is generated, when the
// port(s) the proxy is about to bind are already taken by something
// else -- typically a web server already installed on the host.
func checkProxyPorts(auth proxy.AuthURL, mode string, routesCentral bool) error {
	// Ports our own proxy from the current deployment holds are fine --
	// `up` replaces it. Only those: if this configure switches mode (say
	// external -> nginx), the ports the new mode needs must still be free
	// of anything else, or `up` would tear down a working proxy for one
	// that can't bind.
	ours := map[int]bool{}
	running, err := docker.IsRunning(proxy.ContainerName)
	if err != nil {
		return err
	}
	if running {
		if prev, err := state.Load(); err == nil && prev.Target == "vps" && prev.ProxyMode != "" {
			if prevAuth, err := proxy.ParseAuthURL(prev.AuthURL, prev.ProxyMode); err == nil {
				for _, p := range proxy.Ports(prevAuth, prev.ProxyMode) {
					ours[p] = true
				}
			}
		}
	}

	ports := proxy.Ports(auth, mode)
	if routesCentral {
		// Loopback ports of the stream server on 443 and its relay.
		ports = append(ports, proxy.CentralFrontPort, proxy.CentralRelayPort)
	}
	for _, port := range ports {
		if ours[port] || !proxy.PortInUse(port) {
			continue
		}
		if mode == proxy.ModeExternal {
			return fmt.Errorf("port %d is already in use on this machine -- with --proxy external Versola's proxy listens on 127.0.0.1:%d, so that port has to be free", port, port)
		}
		return fmt.Errorf("port %d is already in use on this machine, probably by a web server that's already installed. Either stop it, or deploy behind it with --proxy external: Versola's proxy then listens on 127.0.0.1:%d and your web server forwards %s to it", port, proxy.ExternalPort, auth.Host)
	}
	return nil
}

// requireAdminConsole: versola-tools puts the built admin console into
// the bundle, and the proxy serves it from there. Versola releases from
// before that don't -- this CLI can't set up their proxy. Checked right
// after versola-tools ran, before anything else is started.
func requireAdminConsole(dir, version string) error {
	if _, err := os.Stat(filepath.Join(dir, "central-ui", "index.html")); err != nil {
		return fmt.Errorf("Versola %s doesn't ship the admin console in versola-tools, so this versola-cli can't set up its reverse proxy -- deploy a newer Versola version", version)
	}
	return nil
}

// setUpProxy writes the proxy's files into the bundle and, for TLS,
// prepares the volume the ACME module keeps its state in. routed is the
// routing part of the config (routedConfig); the rest is filled in here.
func setUpProxy(dir string, auth proxy.AuthURL, opts ProxyOptions, routed proxy.Config) error {
	cfg := routed
	cfg.Mode, cfg.AuthURL, cfg.ACMEDirectory = opts.Mode, auth, proxy.ACMEProduction
	if opts.ACMEStaging {
		cfg.ACMEDirectory = proxy.ACMEStaging
	}
	if opts.Mode == proxy.ModeNginx {
		cfg.IPv6 = proxy.HostHasGlobalIPv6()
	}
	if central, ok, err := proxy.ParseCentralURL(opts.CentralURL, auth); err != nil {
		return err
	} else if ok {
		cfg.CentralHost = central.RoutedHost(auth, opts.Mode)
	}
	tls := auth.TLS(opts.Mode)
	if tls {
		r, err := proxy.Resolvers()
		if err != nil {
			return err
		}
		cfg.Resolvers = r
	}
	if err := proxy.Write(dir, cfg); err != nil {
		return fmt.Errorf("couldn't write the reverse proxy's config: %w", err)
	}
	if !tls {
		return nil
	}

	// External volume, created here like OpenBao's (see proxy.yml's
	// comment on why external). `docker volume create` is idempotent.
	if err := docker.Run("volume", "create", proxy.ACMEVolume); err != nil {
		return fmt.Errorf("couldn't create the %s volume: %w", proxy.ACMEVolume, err)
	}
	// A fresh volume is owned by root; nginx's worker processes run as
	// the image's nginx user (uid/gid 101) and the ACME module has to be
	// able to write its state there. Done through a throwaway container
	// of the proxy image itself -- this CLI doesn't run as root -- which
	// also pulls the image ahead of `up`.
	if err := docker.Run("run", "--rm", "-v", proxy.ACMEVolume+":/state", "--entrypoint", "sh", proxy.Image,
		"-c", "mkdir -p /state/production /state/staging && chown -R 101:101 /state"); err != nil {
		return fmt.Errorf("couldn't prepare the %s volume: %w", proxy.ACMEVolume, err)
	}
	return nil
}

// proxyOps are the docker calls startProxy makes, replaceable in tests.
var proxyOps = struct {
	containerID func(name string) (string, bool, error) // id, running
	compose     func(composePath string, args ...string) error
	apply       func(bundleDir string, c proxy.Config) (string, error)
	writeLocal  func(bundleDir string, c proxy.Config) error
	reload      func(mode string) error
	rollBack    func(dir, previous string) error
	waitReady   func(mode string, a proxy.AuthURL) error
	waitFronts  func(f proxy.Fronts) error
	// waitCentral waits for the front of central's PORT alone.
	waitCentral func(port int) error
}{
	containerID: func(name string) (string, bool, error) {
		out, found, err := docker.Inspect(name, "{{.Id}} {{.State.Running}}")
		if err != nil || !found {
			return "", false, err
		}
		id, running, _ := strings.Cut(strings.TrimSpace(out), " ")
		return id, running == "true", nil
	},
	compose: func(composePath string, args ...string) error {
		return docker.Run(state.ComposeArgs(composePath, args...)...)
	},
	apply:      proxy.Apply,
	writeLocal: proxy.WriteUpstreams,
	reload:     proxy.ReloadCurrent,
	rollBack:   proxy.RollBack,
	waitReady: func(mode string, a proxy.AuthURL) error {
		return proxy.WaitReady(mode, a, 60*time.Second, 120*time.Second)
	},
	waitFronts:  func(f proxy.Fronts) error { return proxy.WaitFronts(f, 30*time.Second) },
	waitCentral: func(port int) error { return proxy.WaitCentralFront(port, 30*time.Second) },
}

// startProxy makes the proxy route as routed says and waits until it
// actually serves Versola.
//
// vps (routed.GenerationsDir set): the new configuration is built and
// checked as a generation of its own and `current` switched to it (Apply)
// -- nothing the running nginx uses changes before that check passes. Then
// `compose up -d proxy`, without --force-recreate: Compose recreates the
// container only when its definition changed (the image, the mounts --
// once, when a deployment moves to config generations), and otherwise
// leaves it running, in which case nginx reloads into `current`. A reload
// nginx refuses puts `current` back.
//
// local: the bundle's files are bind-mounted one by one, which a rewrite
// does not reach, so the container is recreated (--force-recreate).
func startProxy(composePath string, st *state.State, routed proxy.Config) error {
	if err := beginProxy(composePath, st, routed); err != nil {
		return err
	}
	return waitProxy(st, routed)
}

// startProxyForCentral starts the proxy early on vps, as soon as central is
// ready and before auth and edge start: they reach central through the
// proxy's front of central's PORT, and fail at startup when it is not there.
// It waits for that front only -- Versola and the other fronts have nothing
// behind them yet; waitProxy checks them once auth and edge are up.
func startProxyForCentral(composePath string, st *state.State, routed proxy.Config) error {
	if err := beginProxy(composePath, st, routed); err != nil {
		return err
	}
	fmt.Println("Waiting for it to serve central...")
	if err := proxyOps.waitCentral(routed.Fronts.Central); err != nil {
		return fmt.Errorf("the reverse proxy isn't serving its front of central's PORT: %w (see `docker logs %s`)", err, proxy.ContainerFor(st.ProxyMode))
	}
	return nil
}

// beginProxy makes the proxy route as routed says, starting or reloading
// it, without waiting for anything to be served (see startProxy).
func beginProxy(composePath string, st *state.State, routed proxy.Config) error {
	if _, err := proxy.ParseAuthURL(st.AuthURL, st.ProxyMode); err != nil {
		return fmt.Errorf("the recorded auth URL is no longer valid -- configure again: %w", err)
	}
	dir := filepath.Dir(composePath)
	fmt.Println("Starting the reverse proxy...")
	if routed.GenerationsDir == "" {
		if err := proxyOps.writeLocal(dir, routed); err != nil {
			return fmt.Errorf("couldn't write the reverse proxy's upstreams: %w", err)
		}
		if err := proxyOps.compose(composePath, "up", "-d", "--force-recreate", proxy.ServiceName); err != nil {
			return fmt.Errorf("couldn't start the reverse proxy: %w", err)
		}
	} else {
		name := proxy.ContainerFor(st.ProxyMode)
		before, wasRunning, err := proxyOps.containerID(name)
		if err != nil {
			return err
		}
		previous, err := proxyOps.apply(dir, routed)
		if err != nil {
			return fmt.Errorf("couldn't prepare the reverse proxy's config: %w", err)
		}
		if err := proxyOps.compose(composePath, "up", "-d", proxy.ServiceName); err != nil {
			return fmt.Errorf("couldn't start the reverse proxy: %w", err)
		}
		after, _, err := proxyOps.containerID(name)
		if err != nil {
			return err
		}
		// The same container, already running: it still has the config it
		// read last, so it reloads into `current`. A new or restarted one
		// read `current` as it started.
		if wasRunning && after == before {
			fmt.Println("Reloading it (no restart)...")
			if err := proxyOps.reload(st.ProxyMode); err != nil {
				if rerr := proxyOps.rollBack(routed.GenerationsDir, previous); rerr != nil {
					return fmt.Errorf("%w (and pointing the config back at %s failed: %v)", err, previous, rerr)
				}
				return err
			}
		}
	}
	return nil
}

// waitProxy waits until the proxy actually serves Versola, and its fronts.
func waitProxy(st *state.State, routed proxy.Config) error {
	auth, err := proxy.ParseAuthURL(st.AuthURL, st.ProxyMode)
	if err != nil {
		return fmt.Errorf("the recorded auth URL is no longer valid -- configure again: %w", err)
	}
	fmt.Println("Waiting for it to serve Versola...")
	err = proxyOps.waitReady(st.ProxyMode, auth)
	if errors.Is(err, proxy.ErrTLSPending) {
		fmt.Printf("\nWarning: %v.\nThat's usually the certificate still being issued -- nginx keeps retrying on its own. Check that %s's DNS points at this server and that ports 80/443 are reachable from the internet; see `docker logs %s` for the ACME module's progress.\n", err, auth.Host, proxy.ContainerFor(st.ProxyMode))
	} else if err != nil {
		return fmt.Errorf("the reverse proxy isn't serving Versola: %w (see `docker logs %s`)", err, proxy.ContainerFor(st.ProxyMode))
	}
	if routed.Fronts != (proxy.Fronts{}) {
		if err := proxyOps.waitFronts(routed.Fronts); err != nil {
			return fmt.Errorf("the reverse proxy isn't serving its fronts of central's PORT and auth's APORT/MPORT: %w (see `docker logs %s`)", err, proxy.ContainerFor(st.ProxyMode))
		}
	}
	return nil
}

// deployedProxyReservations are the host ports the proxy of an existing
// deployment holds, for the port-clash check of `up` and `replica`.
func deployedProxyReservations(st *state.State) []reservation {
	auth, err := proxy.ParseAuthURL(st.AuthURL, st.ProxyMode)
	if err != nil {
		// The proxy step reports an unusable auth URL itself, but until
		// then its ports are not free to take. Without a scheme to go by,
		// reserve both of nginx's.
		ports := proxy.Ports(proxy.AuthURL{}, st.ProxyMode)
		if st.ProxyMode == proxy.ModeNginx {
			ports = []int{80, 443}
		}
		return proxyReservations(ports, st.ProxyMode)
	}
	return reservationsFor(auth, st.ProxyMode)
}
