package proxy

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/template"
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
}

// Files renders the proxy's files, keyed by their path relative to the
// deployment bundle.
func (c Config) Files() (map[string][]byte, error) {
	if _, err := ParseMode(c.Mode); err != nil {
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
		"ContainerName": ContainerName,
		"ACMEVolume":    ACMEVolume,
	}

	files := map[string][]byte{}
	for out, tmpl := range map[string]string{
		ComposeFile:                 "templates/proxy.yml.tmpl",
		"proxy/conf.d/versola.conf": "templates/versola.conf.tmpl",
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
