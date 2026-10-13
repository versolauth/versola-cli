package deploy

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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
}

var postgresDBPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// postgresOptionsOf returns the options recorded in a deployment's state.
func postgresOptionsOf(st *state.State) PostgresOptions {
	return PostgresOptions{DB: st.PostgresDB, CAFile: st.PostgresCAPath()}
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
	return o
}

// Validate checks the options for target and returns them with CAFile made
// absolute (docker's -v needs an absolute host path). Both options only
// apply to vps; local runs its own Postgres.
func (o PostgresOptions) Validate(target string) (PostgresOptions, error) {
	if o == (PostgresOptions{}) {
		return o, nil
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
	if o.DB != "" {
		args = append(args, "-e", "POSTGRES_DB="+o.DB)
	}
	if o.CAFile != "" {
		args = append(args, "-v", o.CAFile+":"+postgresCAMount+":ro", "-e", "POSTGRES_CA_FILE="+postgresCAMount)
	}
	return args
}
