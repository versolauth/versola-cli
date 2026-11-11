package deploy

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/versolauth/versola-cli/internal/state"
)

// postgresCAMount is where --postgres-ca's file appears inside the
// versola-tools container (and what POSTGRES_CA_FILE points at).
const postgresCAMount = "/input/postgres-ca.crt"

// PostgresOptions are the optional vps-only facts about the Postgres the
// services connect to, beyond its host:port.
type PostgresOptions struct {
	// DB is --postgres-db: the database name. "" leaves versola-tools'
	// own default.
	DB string
	// CAFile is --postgres-ca: a PEM file with the CA certificate(s)
	// that signed Postgres' server certificate. "" means none.
	CAFile string
	// Mode is PostgresModeBundled when the deployment runs its own Postgres
	// in a container, "" for an external one. Not a flag: it is decided from
	// --postgres-host and the recorded deployment (resolvePostgres), and
	// recorded, so that a rolling upgrade generates the same bundle again.
	Mode string
}

var postgresDBPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// postgresOptionsOf returns the options recorded in a deployment's state.
func postgresOptionsOf(st *state.State) PostgresOptions {
	return PostgresOptions{DB: st.PostgresDB, CAFile: st.PostgresCAPath(), Mode: st.PostgresMode}
}

// withStored fills what the flags left empty from the recorded vps
// deployment, so that an explicit flag overrides and no flag keeps what was
// configured before. Nothing is taken from a record of another target, or
// when there is no readable record.
func (o PostgresOptions) withStored(target string) PostgresOptions {
	if target != "vps" {
		return o
	}
	st, err := state.Load()
	if err != nil || st.Target != "vps" {
		return o
	}
	stored := postgresOptionsOf(st)
	if o.DB == "" {
		o.DB = stored.DB
	}
	if o.CAFile == "" {
		o.CAFile = stored.CAFile
	}
	if o.Mode == "" {
		o.Mode = stored.Mode
	}
	return o
}

// Validate checks the options for target and returns them with CAFile made
// absolute (docker's -v needs an absolute host path). Both options only
// apply to vps; local runs its own Postgres.
func (o PostgresOptions) Validate(target string) (PostgresOptions, error) {
	if o == (PostgresOptions{}) {
		return o, nil
	}
	if o.Mode != "" && o.Mode != PostgresModeBundled {
		return o, fmt.Errorf("unknown Postgres mode %q", o.Mode)
	}
	if o.Mode == PostgresModeBundled && o.CAFile != "" {
		return o, fmt.Errorf("--postgres-ca cannot be combined with the Postgres that runs in a container: it speaks no TLS, on loopback only")
	}
	if target != "vps" {
		if o.DB != "" {
			return o, fmt.Errorf("--postgres-db only applies to vps deployments")
		}
		return o, fmt.Errorf("--postgres-ca only applies to vps deployments")
	}
	if o.DB != "" {
		if len(o.DB) > 63 || !postgresDBPattern.MatchString(o.DB) {
			return o, fmt.Errorf("--postgres-db %q is not a valid database name: use 1-63 letters, digits, '_' or '-'", o.DB)
		}
	}
	if o.CAFile != "" {
		abs, err := filepath.Abs(o.CAFile)
		if err != nil {
			return o, fmt.Errorf("--postgres-ca %q: %w", o.CAFile, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return o, fmt.Errorf("--postgres-ca: %w", err)
		}
		if !info.Mode().IsRegular() {
			return o, fmt.Errorf("--postgres-ca %s is not a regular file", abs)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return o, fmt.Errorf("--postgres-ca: %w", err)
		}
		if !bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----")) {
			return o, fmt.Errorf("--postgres-ca %s does not contain a PEM certificate (no \"-----BEGIN CERTIFICATE-----\" line)", abs)
		}
		o.CAFile = abs
	}
	return o, nil
}

// toolsArgs returns the docker run arguments that forward the options to
// versola-tools: -e for the database name, and a read-only mount plus -e
// for the CA file. Nothing for unset options.
func (o PostgresOptions) toolsArgs() []string {
	var args []string
	if o.Mode == PostgresModeBundled {
		args = append(args, "-e", "POSTGRES_MODE="+PostgresModeBundled)
	}
	if o.DB != "" {
		args = append(args, "-e", "POSTGRES_DB="+o.DB)
	}
	if o.CAFile != "" {
		args = append(args, "-v", o.CAFile+":"+postgresCAMount+":ro", "-e", "POSTGRES_CA_FILE="+postgresCAMount)
	}
	return args
}

// postgresHostShape is the one sentence every refusal of --postgres-host ends
// with. The refused value itself is never part of an error: a customer who
// pastes the connection URL pastes its password with it.
const postgresHostShape = "--postgres-host is the address of your Postgres, as host or host:port (db.example.com, 10.0.0.5:5432, [::1]:5432): no scheme, user name, password, path or spaces"

// ParsePostgresHost checks a --postgres-host value: a name or an IP, with an
// optional port 1-65535, an IPv6 address in brackets. "" (the flag left out)
// is valid. The value is returned as given -- it is what the record keeps and
// what a later run compares against -- and the error never repeats it.
func ParsePostgresHost(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	fail := func(why string) (string, error) {
		return "", fmt.Errorf("%s; %s", postgresHostShape, why)
	}
	switch {
	case strings.Contains(raw, "://"), strings.ContainsAny(raw, "@/?#\\ \t\r\n"):
		return fail("what was given looks like a URL or holds characters a host cannot; if it was a connection URL, its password stays out of the command line")
	}
	host, port := raw, ""
	if strings.HasPrefix(raw, "[") {
		end := strings.Index(raw, "]")
		if end < 0 {
			return fail("an IPv6 address needs its closing bracket")
		}
		host, port = raw[1:end], raw[end+1:]
		ip := net.ParseIP(host)
		if ip == nil || !strings.Contains(host, ":") {
			return fail("the brackets are for an IPv6 address")
		}
		if port != "" {
			if port[0] != ':' {
				return fail("after the closing bracket only :port may follow")
			}
			port = port[1:]
			if err := checkPostgresPort(port); err != nil {
				return fail(err.Error())
			}
		}
		return raw, nil
	}
	if strings.Count(raw, ":") > 1 {
		return fail("an IPv6 address goes in brackets")
	}
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		host, port = raw[:i], raw[i+1:]
		if err := checkPostgresPort(port); err != nil {
			return fail(err.Error())
		}
	}
	if host == "" || len(host) > 253 {
		return fail("the host is empty or longer than 253 characters")
	}
	if strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_") != "" {
		return fail("a host name has only letters, digits, '.', '-' and '_'")
	}
	// One trailing dot is the absolute form of a name (db.example.com.).
	for label := range strings.SplitSeq(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 {
			return fail("a host name has labels of 1-63 characters between its dots")
		}
	}
	return raw, nil
}

func checkPostgresPort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" {
		return fmt.Errorf("the port is a number from 1 to 65535")
	}
	return nil
}
