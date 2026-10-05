package proxy

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"text/template"

	"github.com/versolauth/versola-cli/internal/docker"
)

//go:embed templates/*
var templates embed.FS

// Config is everything the generated proxy files depend on.
type Config struct {
	Mode          string
	AuthURL       AuthURL
	ACMEDirectory string // ACMEProduction or ACMEStaging; used only with TLS
	IPv6          bool   // HostHasGlobalIPv6(); used only in ModeNginx
	Resolvers     string // Resolvers(); required only with TLS

	// Auth and Edge are the replicas the auth_backend / edge_backend
	// upstreams route to, at least one each. Where they listen comes from
	// the deployment's compose file (package topology), not from here.
	Auth []Backend
	Edge []Backend
}

// Backend is one replica an upstream routes to.
type Backend struct {
	// Service is the replica's compose service name -- its host name on
	// local's bridge network. Unused on vps, where it is reached on the
	// host's loopback.
	Service string
	// Port is the replica's PORT: the one carrying application traffic.
	Port int
}

// upstreamAddrs returns the `server` address of each backend: on the
// host's loopback (vps, which shares the host's network) or by compose
// service name (local, on the project's bridge network).
func upstreamAddrs(mode string, backends []Backend) []string {
	addrs := make([]string, len(backends))
	for i, b := range backends {
		if mode == ModeLocal {
			addrs[i] = fmt.Sprintf("%s:%d", b.Service, b.Port)
		} else {
			addrs[i] = fmt.Sprintf("127.0.0.1:%d", b.Port)
		}
	}
	return addrs
}

// serviceName: what a compose service name may look like, which is also
// what keeps a name from carrying nginx syntax into the config.
var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// checkBackends: what an upstream needs to be a valid nginx block -- at
// least one server, each a real port, none listed twice (nginx would
// accept a duplicate and silently double its weight).
func checkBackends(name, mode string, backends []Backend) error {
	if len(backends) == 0 {
		return fmt.Errorf("the %s upstream needs at least one replica", name)
	}
	seen := map[string]bool{}
	for _, b := range backends {
		if b.Port < 1 || b.Port > 65535 {
			return fmt.Errorf("the %s upstream has a replica on port %d, which is not a port", name, b.Port)
		}
		if mode == ModeLocal && b.Service == "" {
			return fmt.Errorf("the %s upstream has a replica without a service name", name)
		}
		if b.Service != "" && !serviceName.MatchString(b.Service) {
			return fmt.Errorf("the %s upstream has a replica with an invalid service name %q", name, b.Service)
		}
	}
	for _, addr := range upstreamAddrs(mode, backends) {
		if seen[addr] {
			return fmt.Errorf("the %s upstream lists %s twice", name, addr)
		}
		seen[addr] = true
	}
	return nil
}

// UpstreamsFile is the file with the auth/edge upstreams, relative to the
// deployment bundle. Reload rewrites it alone.
const UpstreamsFile = "proxy/conf.d/upstreams.conf"

// Files renders the proxy's files, keyed by their path relative to the
// deployment bundle.
func (c Config) Files() (map[string][]byte, error) {
	if err := validMode(c.Mode); err != nil {
		return nil, err
	}
	if err := checkBackends("auth", c.Mode, c.Auth); err != nil {
		return nil, err
	}
	if err := checkBackends("edge", c.Mode, c.Edge); err != nil {
		return nil, err
	}
	// The same address in both would send auth traffic to edge (or back).
	edgeAddrs := map[string]bool{}
	for _, a := range upstreamAddrs(c.Mode, c.Edge) {
		edgeAddrs[a] = true
	}
	for _, a := range upstreamAddrs(c.Mode, c.Auth) {
		if edgeAddrs[a] {
			return nil, fmt.Errorf("the auth and edge upstreams both route to %s", a)
		}
	}
	tls := c.AuthURL.TLS(c.Mode)
	if tls && c.Resolvers == "" {
		return nil, fmt.Errorf("TLS needs at least one DNS resolver")
	}
	if tls && c.ACMEDirectory != ACMEProduction && c.ACMEDirectory != ACMEStaging {
		return nil, fmt.Errorf("unknown ACME directory %q", c.ACMEDirectory)
	}
	ipv6 := c.IPv6 && c.Mode == ModeNginx

	var listen []string
	switch {
	case c.Mode == ModeLocal:
		listen = []string{strconv.Itoa(LocalPort)}
	case c.Mode == ModeExternal:
		listen = []string{"127.0.0.1:" + strconv.Itoa(ExternalPort)}
	case tls:
		listen = []string{"443 ssl"}
		if ipv6 {
			listen = append(listen, "[::]:443 ssl")
		}
	default:
		listen = []string{"80"}
		if ipv6 {
			listen = append(listen, "[::]:80")
		}
	}

	routes, err := templates.ReadFile("templates/routes.conf")
	if err != nil {
		return nil, err
	}
	data := map[string]any{
		"TLS":           tls,
		"IPv6":          ipv6,
		"RealIP":        c.Mode == ModeExternal,
		"Listen":        listen,
		"Host":          c.AuthURL.Host,
		"Resolvers":     c.Resolvers,
		"ACMEDirectory": c.ACMEDirectory,
		"ACMEStateDir":  acmeStateDir(c.ACMEDirectory),
		"Routes":        string(routes),
		"Image":         Image,
		"ContainerName": ContainerFor(c.Mode),
		"ACMEVolume":    ACMEVolume,
		"ExternalPort":  ExternalPort,
		"LocalPort":     LocalPort,
		"HostNetwork":   c.Mode != ModeLocal,
		"Resolve":       c.Mode == ModeLocal,
		"AuthUpstreams": upstreamAddrs(c.Mode, c.Auth),
		"EdgeUpstreams": upstreamAddrs(c.Mode, c.Edge),
	}

	files := map[string][]byte{}
	for out, tmpl := range map[string]string{
		ComposeFile:                 "templates/proxy.yml.tmpl",
		"proxy/conf.d/versola.conf": "templates/versola.conf.tmpl",
		UpstreamsFile:               "templates/upstreams.conf.tmpl",
	} {
		b, err := render(tmpl, data)
		if err != nil {
			return nil, err
		}
		files[out] = b
	}
	for out, src := range map[string]string{
		"proxy/nginx.conf":        "templates/nginx.conf",
		"proxy/proxy_params.conf": "templates/proxy_params.conf",
	} {
		b, err := templates.ReadFile(src)
		if err != nil {
			return nil, err
		}
		files[out] = b
	}
	return files, nil
}

// acmeStateDir is the ACME module's state directory (inside ACMEVolume)
// for the given directory -- see versola.conf.tmpl.
func acmeStateDir(directory string) string {
	if directory == ACMEStaging {
		return "staging"
	}
	return "production"
}

func render(name string, data any) ([]byte, error) {
	t, err := template.New(filepath.Base(name)).Option("missingkey=error").ParseFS(templates, name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// Write renders the proxy's files into the deployment bundle dir.
func Write(bundleDir string, c Config) error {
	files, err := c.Files()
	if err != nil {
		return err
	}
	for rel, content := range files {
		path := filepath.Join(bundleDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("couldn't write %s: %w", path, err)
		}
	}
	return nil
}

// Reload makes the running proxy route to the replicas in c: rewrites the
// upstreams file in the bundle, has nginx check the result, and reloads it
// (no restart: connections in flight finish on the old workers, see
// worker_shutdown_timeout in nginx.conf). If nginx rejects the new file the
// old one is put back, so the bundle never holds a config the next start of
// the proxy would fail on, and the running proxy is untouched.
func Reload(bundleDir string, c Config) error {
	files, err := c.Files()
	if err != nil {
		return err
	}
	path := filepath.Join(bundleDir, UpstreamsFile)
	old, readErr := os.ReadFile(path)
	if err := os.WriteFile(path, files[UpstreamsFile], 0o644); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	name := ContainerFor(c.Mode)
	if _, err := docker.Output("exec", name, "nginx", "-t"); err != nil {
		if readErr == nil {
			_ = os.WriteFile(path, old, 0o644)
		}
		// nginx's own words (stderr): which line it did not like.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return fmt.Errorf("nginx rejected the new upstreams (the proxy keeps its old ones): %s", bytes.TrimSpace(ee.Stderr))
		}
		return fmt.Errorf("nginx rejected the new upstreams (the proxy keeps its old ones): %w", err)
	}
	if err := docker.Run("exec", name, "nginx", "-s", "reload"); err != nil {
		return fmt.Errorf("nginx accepted the new upstreams but the reload failed (the proxy keeps routing to the old ones; the bundle has the new): %w", err)
	}
	return nil
}
