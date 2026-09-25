package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// ProxyOptions: how a vps deployment's reverse proxy is set up (see
// package proxy). Ignored for local.
type ProxyOptions struct {
	// Mode is proxy.ModeNginx (the proxy owns 80/443 and does TLS) or
	// proxy.ModeExternal (behind the host's existing web server).
	Mode string
	// ACMEStaging requests the certificate from Let's Encrypt's staging
	// environment: untrusted, but without production's rate limits.
	ACMEStaging bool
}

// checkProxyPorts fails early, before anything is generated, when the
// port(s) the proxy is about to bind are already taken by something
// else -- typically a web server already installed on the host.
func checkProxyPorts(auth proxy.AuthURL, mode string) error {
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

	for _, port := range proxy.Ports(auth, mode) {
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
// prepares the volume the ACME module keeps its state in.
func setUpProxy(dir string, auth proxy.AuthURL, opts ProxyOptions) error {
	cfg := proxy.Config{Mode: opts.Mode, AuthURL: auth, ACMEDirectory: proxy.ACMEProduction}
	if opts.ACMEStaging {
		cfg.ACMEDirectory = proxy.ACMEStaging
	}
	if opts.Mode == proxy.ModeNginx {
		cfg.IPv6 = proxy.HostHasGlobalIPv6()
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

// startProxy (re)starts the proxy and waits until it actually serves
// Versola. --force-recreate: the proxy's config lives in bind-mounted
// files, and Compose only recreates a container when its service
// definition changes, not when the content of a mounted file does.
func startProxy(composePath string, st *state.State) error {
	auth, err := proxy.ParseAuthURL(st.AuthURL, st.ProxyMode)
	if err != nil {
		return fmt.Errorf("the recorded auth URL is no longer valid -- configure again: %w", err)
	}
	fmt.Println("Starting the reverse proxy...")
	if err := docker.Run(state.ComposeArgs(composePath, "up", "-d", "--force-recreate", proxy.ServiceName)...); err != nil {
		return fmt.Errorf("couldn't start the reverse proxy: %w", err)
	}
	fmt.Println("Waiting for it to serve Versola...")
	err = proxy.WaitReady(st.ProxyMode, auth, 60*time.Second, 120*time.Second)
	if errors.Is(err, proxy.ErrTLSPending) {
		fmt.Printf("\nWarning: %v.\nThat's usually the certificate still being issued -- nginx keeps retrying on its own. Check that %s's DNS points at this server and that ports 80/443 are reachable from the internet; see `docker logs %s` for the ACME module's progress.\n", err, auth.Host, proxy.ContainerName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("the reverse proxy isn't serving Versola: %w (see `docker logs %s`)", err, proxy.ContainerName)
	}
	return nil
}
