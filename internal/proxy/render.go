package proxy

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/fsutil"
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

	// Fronts, vps only: the loopback addresses the proxy takes over in front
	// of auth's APORT and MPORT, balancing them over Auth's AdditionalPort
	// and MutualTLSPort. Zero: none (local, where every container has its
	// own ports and edge reaches auth by name).
	Fronts Fronts

	// GenerationsDir, vps only: the host directory the proxy's configuration
	// lives in, as generations (see generation.go), instead of the bundle.
	// Empty: the bundle's files are mounted as they are (local).
	GenerationsDir string
}

// Fronts are the ports the proxy listens on, on 127.0.0.1, in front of
// auth's additional (APORT) and mutual-TLS (MPORT) listeners: the ports
// the compose file gives auth, which edge and central connect to.
type Fronts struct {
	Additional int
	MutualTLS  int
}

func (f Fronts) set() bool { return f.Additional != 0 || f.MutualTLS != 0 }

// Backend is one replica an upstream routes to.
type Backend struct {
	// Service is the replica's compose service name -- its host name on
	// local's bridge network. Unused on vps, where it is reached on the
	// host's loopback.
	Service string
	// Port is the replica's PORT: the one carrying application traffic.
	Port int
	// AdditionalPort and MutualTLSPort: an auth replica's APORT and MPORT,
	// when the proxy is in front of them (Config.Fronts); 0 otherwise.
	AdditionalPort int
	MutualTLSPort  int
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

// checkUpstreams: both upstreams are valid nginx blocks and share no address.
func (c Config) checkUpstreams() error {
	if err := checkBackends("auth", c.Mode, c.Auth); err != nil {
		return err
	}
	if err := checkBackends("edge", c.Mode, c.Edge); err != nil {
		return err
	}
	// The same address in both would send auth traffic to edge (or back).
	edgeAddrs := map[string]bool{}
	for _, a := range upstreamAddrs(c.Mode, c.Edge) {
		edgeAddrs[a] = true
	}
	for _, a := range upstreamAddrs(c.Mode, c.Auth) {
		if edgeAddrs[a] {
			return fmt.Errorf("the auth and edge upstreams both route to %s", a)
		}
	}
	return c.checkFronts()
}

// checkFronts: the proxy in front of auth's APORT/MPORT needs both ports,
// on vps, and every auth replica's own pair -- none of them the proxy's own
// port, which would send the proxy's requests back to itself.
func (c Config) checkFronts() error {
	if !c.Fronts.set() {
		return nil
	}
	if c.Mode == ModeLocal {
		return fmt.Errorf("the proxy only fronts auth's APORT/MPORT on vps")
	}
	front := map[int]string{}
	for name, p := range map[string]int{"APORT": c.Fronts.Additional, "MPORT": c.Fronts.MutualTLS} {
		if p < 1 || p > 65535 {
			return fmt.Errorf("the proxy's %s front is on port %d, which is not a port", name, p)
		}
		front[p] = name
	}
	if len(front) != 2 {
		return fmt.Errorf("the proxy's APORT and MPORT fronts are both on port %d", c.Fronts.Additional)
	}
	seen := map[int]bool{}
	for _, b := range c.Auth {
		for name, p := range map[string]int{"APORT": b.AdditionalPort, "MPORT": b.MutualTLSPort} {
			if p < 1 || p > 65535 {
				return fmt.Errorf("auth replica %s has %s %d, which is not a port", b.Service, name, p)
			}
			if f, ok := front[p]; ok {
				return fmt.Errorf("auth replica %s listens for %s on %d, where the proxy itself listens for %s", b.Service, name, p, f)
			}
			if seen[p] {
				return fmt.Errorf("auth's APORT/MPORT upstreams list port %d twice", p)
			}
			seen[p] = true
		}
	}
	return nil
}

// InternalFile and StreamFile are the proxy's fronts of auth's APORT and
// MPORT (vps), relative to the deployment bundle like UpstreamsFile. They
// are written with it, and absent when there are no fronts.
const (
	InternalFile = "proxy/conf.d/internal.conf"
	StreamFile   = "proxy/stream.d/auth-mtls.conf"
)

// dynamicData is what the files that change with the replicas are
// rendered from.
func (c Config) dynamicData() map[string]any {
	var additional, mutual []string
	for _, b := range c.Auth {
		additional = append(additional, fmt.Sprintf("127.0.0.1:%d", b.AdditionalPort))
		mutual = append(mutual, fmt.Sprintf("127.0.0.1:%d", b.MutualTLSPort))
	}
	return map[string]any{
		"Resolve":             c.Mode == ModeLocal,
		"AuthUpstreams":       upstreamAddrs(c.Mode, c.Auth),
		"EdgeUpstreams":       upstreamAddrs(c.Mode, c.Edge),
		"Fronted":             c.Fronts.set(),
		"AdditionalListen":    c.Fronts.Additional,
		"MutualTLSListen":     c.Fronts.MutualTLS,
		"AdditionalUpstreams": additional,
		"MutualTLSUpstreams":  mutual,
	}
}

// DynamicFiles renders the files that change with the replicas, keyed by
// their path relative to the bundle: UpstreamsFile always, InternalFile and
// StreamFile when the proxy fronts auth's APORT/MPORT. It needs only the
// mode, the backends and the fronts, not the rest of the configuration.
func (c Config) DynamicFiles() (map[string][]byte, error) {
	if err := validMode(c.Mode); err != nil {
		return nil, err
	}
	if err := c.checkUpstreams(); err != nil {
		return nil, err
	}
	data := c.dynamicData()
	files := map[string][]byte{}
	templates := map[string]string{UpstreamsFile: "templates/upstreams.conf.tmpl"}
	if c.Fronts.set() {
		templates[InternalFile] = "templates/internal.conf.tmpl"
		templates[StreamFile] = "templates/auth-mtls.conf.tmpl"
	}
	for out, tmpl := range templates {
		b, err := render(tmpl, data)
		if err != nil {
			return nil, err
		}
		files[out] = b
	}
	return files, nil
}

// Upstreams renders UpstreamsFile alone.
func (c Config) Upstreams() ([]byte, error) {
	files, err := c.DynamicFiles()
	if err != nil {
		return nil, err
	}
	return files[UpstreamsFile], nil
}

// HasUpstreamsFile reports whether the bundle keeps its upstreams in
// UpstreamsFile. A bundle configured by a versola-cli from before replicas
// has them inline in versola.conf instead; this CLI does not run those (a
// new configure replaces them), since a second definition next to the
// inline ones would make nginx refuse the config.
func HasUpstreamsFile(bundleDir string) bool {
	_, err := os.Stat(filepath.Join(bundleDir, UpstreamsFile))
	return err == nil
}

// WriteUpstreams rewrites the files that change with the replicas in the
// bundle (DynamicFiles), each atomically, and removes a front file that no
// longer applies.
func WriteUpstreams(bundleDir string, c Config) error {
	files, err := c.DynamicFiles()
	if err != nil {
		return err
	}
	return writeDynamic(bundleDir, files)
}

func writeDynamic(dir string, files map[string][]byte) error {
	for _, rel := range []string{UpstreamsFile, InternalFile, StreamFile} {
		path := filepath.Join(dir, rel)
		b, ok := files[rel]
		if !ok {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("couldn't remove %s: %w", path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(path, b, 0o644); err != nil {
			return fmt.Errorf("couldn't write %s: %w", path, err)
		}
	}
	return nil
}

// Files renders the proxy's files, keyed by their path relative to the
// deployment bundle.
func (c Config) Files() (map[string][]byte, error) {
	if err := validMode(c.Mode); err != nil {
		return nil, err
	}
	if err := c.checkUpstreams(); err != nil {
		return nil, err
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

	if c.GenerationsDir != "" && c.Mode == ModeLocal {
		return nil, fmt.Errorf("the local proxy mounts its bundle's files, not config generations")
	}
	if c.GenerationsDir != "" && !filepath.IsAbs(c.GenerationsDir) {
		return nil, fmt.Errorf("the proxy's config directory %q is not an absolute path", c.GenerationsDir)
	}
	htmlRoot := "/usr/share/nginx/html"
	if c.GenerationsDir != "" {
		htmlRoot = GenerationMount + "/" + CurrentGeneration + "/" + generationHTML
	}
	routes, err := render("templates/routes.conf", map[string]any{"HTMLRoot": htmlRoot})
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
		"Stream":        c.Fronts.set(),
		"Generations":   c.GenerationsDir != "",
		// JSON strings are YAML's double-quoted scalars: a path with spaces
		// or a colon stays one value.
		"ConfPath": quoteYAML(GenerationMount + "/" + CurrentGeneration + "/nginx.conf"),
		// `$` doubled: Compose would otherwise interpolate it.
		"GenerationsMount": quoteYAML(strings.ReplaceAll(c.GenerationsDir, "$", "$$") + ":" + GenerationMount + ":ro"),
	}

	files, err := c.DynamicFiles()
	if err != nil {
		return nil, err
	}
	for out, tmpl := range map[string]string{
		ComposeFile:                 "templates/proxy.yml.tmpl",
		"proxy/conf.d/versola.conf": "templates/versola.conf.tmpl",
		"proxy/nginx.conf":          "templates/nginx.conf.tmpl",
	} {
		b, err := render(tmpl, data)
		if err != nil {
			return nil, err
		}
		files[out] = b
	}
	for out, src := range map[string]string{
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

// quoteYAML is s as a YAML double-quoted scalar.
func quoteYAML(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
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

// Reload makes the running proxy route to the replicas in c. On vps (config
// generations) that is Apply and ReloadCurrent, with `current` put back if
// the running nginx refuses it. Locally: it rewrites the
// upstreams file in the bundle, has nginx check the result, and reloads it
// (no restart: connections in flight finish on the old workers, see
// worker_shutdown_timeout in nginx.conf). If nginx rejects the new file the
// old one is put back, so the bundle never holds a config the next start of
// the proxy would fail on, and the running proxy is untouched.
// The docker calls Reload makes, replaceable in tests.
var (
	dockerOutput = docker.Output
	dockerRun    = docker.Run
)

func Reload(bundleDir string, c Config) error {
	if c.GenerationsDir != "" {
		return reloadGenerations(bundleDir, c)
	}
	content, err := c.Upstreams()
	if err != nil {
		return err
	}
	path := filepath.Join(bundleDir, UpstreamsFile)
	old, readErr := os.ReadFile(path)
	if err := fsutil.WriteFileAtomic(path, content, 0o644); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	name := ContainerFor(c.Mode)
	if _, err := dockerOutput("exec", name, "nginx", "-t"); err != nil {
		if readErr == nil {
			_ = fsutil.WriteFileAtomic(path, old, 0o644)
		} else if os.IsNotExist(readErr) {
			_ = os.Remove(path)
		}
		// nginx's own words (stderr): which line it did not like.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return fmt.Errorf("nginx rejected the new upstreams (the proxy keeps its old ones): %s", bytes.TrimSpace(ee.Stderr))
		}
		return fmt.Errorf("nginx rejected the new upstreams (the proxy keeps its old ones): %w", err)
	}
	if err := dockerRun("exec", name, "nginx", "-s", "reload"); err != nil {
		return fmt.Errorf("nginx accepted the new upstreams but the reload failed (the proxy keeps routing to the old ones; the bundle has the new): %w", err)
	}
	return nil
}

func reloadGenerations(bundleDir string, c Config) error {
	previous, err := Apply(bundleDir, c)
	if err != nil {
		return err
	}
	if err := ReloadCurrent(c.Mode); err != nil {
		if rerr := RollBack(c.GenerationsDir, previous); rerr != nil {
			return fmt.Errorf("%w (and pointing the config back at %s failed: %v)", err, previous, rerr)
		}
		return err
	}
	return nil
}

// Stage makes c the configuration the proxy starts with next, when it is
// not running: Apply on vps, the bundle's upstreams locally.
func Stage(bundleDir string, c Config) error {
	if c.GenerationsDir != "" {
		_, err := Apply(bundleDir, c)
		return err
	}
	return WriteUpstreams(bundleDir, c)
}
