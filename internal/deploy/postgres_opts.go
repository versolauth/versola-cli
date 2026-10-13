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

// VpsOptions are the optional vps-only settings: the Postgres the services
// connect to (beyond its host:port) and the environment name.
type VpsOptions struct {
	// Env is --env: the environment name written into the services'
	// configs. "" means "prod". Anything but "prod" is a test environment.
	Env string
	// DB is --postgres-db: the database name. "" leaves versola-tools'
	// own default.
	DB string
	// CAFile is --postgres-ca: a PEM file with the CA certificate(s)
	// that signed Postgres' server certificate. "" means none.
	CAFile string
}

// ProdEnv is the one environment name the services treat as production.
const ProdEnv = "prod"

var envPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

var postgresDBPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// vpsOptionsOf returns the options recorded in a deployment's state.
func vpsOptionsOf(st *state.State) VpsOptions {
	return VpsOptions{Env: st.Env, DB: st.PostgresDB, CAFile: st.PostgresCAPath()}
}

// withStored fills what the flags left empty from the recorded vps
// deployment, so that an explicit flag overrides and no flag keeps what was
// configured before. Nothing is taken from a record of another target, or
// when there is no readable record.
func (o VpsOptions) withStored(target string) VpsOptions {
	if target != "vps" {
		return o
	}
	st, err := state.Load()
	if err != nil || st.Target != "vps" {
		return o
	}
	stored := vpsOptionsOf(st)
	if o.Env == "" {
		o.Env = stored.Env
	}
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
func (o VpsOptions) Validate(target string) (VpsOptions, error) {
	if o == (VpsOptions{}) {
		return o, nil
	}
	if target != "vps" {
		if o.Env != "" {
			return o, fmt.Errorf("--env only applies to vps deployments")
		}
		if o.DB != "" {
			return o, fmt.Errorf("--postgres-db only applies to vps deployments")
		}
		return o, fmt.Errorf("--postgres-ca only applies to vps deployments")
	}
	if o.Env != "" && (len(o.Env) > 32 || !envPattern.MatchString(o.Env)) {
		return o, fmt.Errorf("--env %q is not a valid environment name: use 1-32 lower-case letters, digits, '_' or '-'", o.Env)
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

// EffectiveEnv is the environment name these options deploy: Env, or "prod".
func (o VpsOptions) EffectiveEnv() string {
	if o.Env == "" {
		return ProdEnv
	}
	return o.Env
}

// IsTestEnv reports whether the deployment is not production.
func (o VpsOptions) IsTestEnv() bool { return o.EffectiveEnv() != ProdEnv }

// TestModeWarning is printed before a vps configure/bootstrap whose
// effective environment is not "prod"; "" for prod (and for local).
func TestModeWarning(target string, o VpsOptions) string {
	if target != "vps" {
		return ""
	}
	o = o.withStored(target)
	if !o.IsTestEnv() {
		return ""
	}
	return fmt.Sprintf(`
!!! WARNING: environment %q is not "prod" -- this deployment runs in TEST MODE.
!!! The OTP code is fixed (123456) and nothing is sent by SMS or e-mail,
!!! temporary passwords can be revealed, and the /service/* endpoints are
!!! enabled. It must NOT serve real users. (Pass --env prod for production.)
`, o.EffectiveEnv())
}

// toolsArgs returns the docker run arguments that forward the options to
// versola-tools: -e for the database name, and a read-only mount plus -e
// for the CA file. Nothing for unset options.
func (o VpsOptions) toolsArgs() []string {
	var args []string
	args = append(args, "-e", "ENV_NAME="+o.EffectiveEnv())
	if o.DB != "" {
		args = append(args, "-e", "POSTGRES_DB="+o.DB)
	}
	if o.CAFile != "" {
		args = append(args, "-v", o.CAFile+":"+postgresCAMount+":ro", "-e", "POSTGRES_CA_FILE="+postgresCAMount)
	}
	return args
}
