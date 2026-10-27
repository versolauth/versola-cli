package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// The Postgres a vps deployment runs itself, as a service of the compose file
// versola-tools generates when it is given POSTGRES_MODE=bundled (see
// versolauth/versola, docker/versola-tools). The names below are the ones its
// template hardcodes.
const (
	// PostgresModeBundled is PostgresOptions.Mode and State.PostgresMode of a
	// deployment with its own Postgres in a container. An external one is "".
	PostgresModeBundled = "bundled"
	// BundledPostgresHost is where the container listens, and so what the
	// services' URLs and State.PostgresHost say. Nothing else is accepted by
	// versola-tools in this mode: the container is not reachable from outside.
	BundledPostgresHost = "127.0.0.1:5432"

	bundledPostgresContainer = "versola-postgres-vps"
	bundledPostgresVolume    = "versola-postgres-data-vps"
	bundledPostgresPort      = 5432
	// postgresSecretsFile is the env_file of the postgres service: the one
	// variable the image creates the role's password from, and nothing of
	// the services' own secrets.
	postgresSecretsFile = "postgres.secrets.env"
)

// bundledPostgresInitSourceFormat is a docker inspect template printing the
// host path the container's first-start script is bind-mounted from.
const bundledPostgresInitSourceFormat = `{{range .Mounts}}{{if eq .Destination "/docker-entrypoint-initdb.d/10-schemas.sh"}}{{.Source}}{{end}}{{end}}`

// PostgresVolumeName is the Docker volume that holds the bundled Postgres's
// data directory. It is declared `external: true` by the compose file, like
// OpenBao's (OpenbaoVolumeName): `down --volumes`, and so `uninstall`, never
// removes it, and a redeploy into a fresh bundle directory finds the data
// where it was left.
func PostgresVolumeName() string { return bundledPostgresVolume }

// resolvePostgres decides which Postgres a vps deployment uses, from
// --postgres-host and from what was recorded before, and returns the mode
// (PostgresModeBundled or "") and the host:port the services reach it at.
//
// The default is a container, but only for a deployment that has none yet:
// leaving --postgres-host out over an existing external one keeps that one,
// at its recorded host, rather than quietly pointing the services at a new,
// empty database. Going the other way is not a configure's job: a deployment
// that runs its own Postgres refuses --postgres-host, because moving the data
// is a backup and a restore.
//
// A rolling upgrade (staged) never changes the mode: it is the one recorded.
func resolvePostgres(target, host string, stored *state.State, staged *stagedConfigure) (mode, resolved string, err error) {
	if target != "vps" {
		return "", host, nil
	}
	if staged != nil {
		if staged.from.PostgresMode == PostgresModeBundled {
			return PostgresModeBundled, BundledPostgresHost, nil
		}
		return "", host, nil
	}
	if stored != nil && stored.Target == "vps" {
		if stored.PostgresMode == PostgresModeBundled {
			if host != "" {
				return "", "", fmt.Errorf("this deployment runs its own Postgres in a container, and --postgres-host would point it at another one. Moving the data is a backup and a restore, which configure does not do: leave --postgres-host out")
			}
			return PostgresModeBundled, BundledPostgresHost, nil
		}
		if host == "" {
			if stored.PostgresHost != "" {
				return "", stored.PostgresHost, nil
			}
			// A record from before layout 3 does not say which Postgres the
			// services use. It has none of its own (the container did not exist),
			// so it is an external one whose address is unknown: guessing
			// "a new container" would swap it for an empty database.
			return "", "", fmt.Errorf("the record of this deployment does not say which Postgres it uses (it was made by an older versola-cli): give --postgres-host")
		}
	}
	if host == "" {
		return PostgresModeBundled, BundledPostgresHost, nil
	}
	return "", host, nil
}

// storedVps is the recorded deployment when it is a vps one, nil when there is
// none (nothing deployed, a record of another target, or one a newer versola
// wrote, which configure refuses on its own). A record that exists but cannot
// be read is an error, not "none": without it configure cannot tell whether
// the deployment runs its own Postgres, and guessing "none" would point the
// services at a new, empty database.
func storedVps() (*state.State, error) {
	st, err := state.Load()
	switch {
	case err == nil:
	case errors.Is(err, state.ErrNotConfigured), errors.Is(err, state.ErrOldInstall), errors.Is(err, state.ErrNewerSchema):
		return nil, nil
	default:
		return nil, err
	}
	if st.Target != "vps" {
		return nil, nil
	}
	return st, nil
}

// refuseBundledDBChange: the container creates the database once, under the
// name --postgres-db had at the first start (auth by default); asking for
// another name later would leave the services pointing at a database that
// does not exist.
func refuseBundledDBChange(stored *state.State, db string) error {
	if stored == nil || stored.PostgresMode != PostgresModeBundled || db == "" {
		return nil
	}
	current := stored.PostgresDB
	if current == "" {
		current = "auth"
	}
	if db != current {
		return fmt.Errorf("--postgres-db %q: this deployment's Postgres was created with the database %q, and it is only created on the first start. Renaming is a dump and a restore, which configure does not do", db, current)
	}
	return nil
}

// bundledServiceLine is how the compose file declares the service: a key of
// the services map, which is indented by two spaces.
var bundledServiceLine = regexp.MustCompile(`(?m)^  postgres:\s*$`)

// requireBundledPostgres fails when the compose file versola-tools generated
// has no postgres service: an older release ignores POSTGRES_MODE and writes
// the file it always wrote, which `up` would then fail on, after the
// configuration was already replaced.
func requireBundledPostgres(composeFile, version string, rollout bool) error {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return fmt.Errorf("couldn't read the generated compose file: %w", err)
	}
	if !bundledServiceLine.Match(data) {
		if rollout {
			return fmt.Errorf("versola-tools %s cannot run Postgres in a container: its compose file has no postgres service, and this deployment's Postgres is one. Roll out to a release that can (nothing was started)", version)
		}
		return fmt.Errorf("versola-tools %s cannot run Postgres in a container: its compose file has no postgres service. Use a release that can, or give --postgres-host to use a Postgres of your own (nothing was started)", version)
	}
	return nil
}

// writePostgresSecretsEnv writes dir/postgres.secrets.env (0600) with the
// password the services use, taken from the auth.secrets.env that was just
// resolved against OpenBao. The container creates the role with it the first
// time it starts, so there is no SQL for the operator to run. It holds that
// one variable: the postgres service has no business with the rest of auth's
// secrets, and Compose's env_file passes a whole file into the container.
func writePostgresSecretsEnv(dir string) error {
	values, err := readDotenv(filepath.Join(dir, "auth.secrets.env"))
	if err != nil {
		return err
	}
	password := values["POSTGRES_PASSWORD"]
	if password == "" {
		return fmt.Errorf("auth.secrets.env has no POSTGRES_PASSWORD, which the Postgres container is created with -- this release's secrets schema does not carry one")
	}
	return writeDotenv(filepath.Join(dir, postgresSecretsFile), map[string]string{"POSTGRES_PASSWORD": password})
}

// bundledPostgresDataExists reports whether the data volume is already there:
// then a Postgres initialised earlier may be in it, with the password it was
// created with.
func bundledPostgresDataExists() bool {
	_, err := docker.Output("volume", "inspect", "--format", "{{.Name}}", bundledPostgresVolume)
	return err == nil
}

// ensureBundledPostgresVolume creates the data volume if it is missing. The
// compose file declares it `external: true`, so Compose refuses to start the
// service until it exists; `docker volume create` is a no-op when it does.
func ensureBundledPostgresVolume() error {
	if err := docker.Run("volume", "create", bundledPostgresVolume); err != nil {
		return fmt.Errorf("couldn't create the Postgres data volume: %w", err)
	}
	return nil
}

// checkBundledPostgresPort is the prerequisite of the container: it uses the
// host network, so port 5432 on the server must be free. A Postgres already
// listening there stops it from starting, and Docker would then restart it
// in a loop. The deployment's own container holding the port is not a conflict.
func checkBundledPostgresPort() checks.Result {
	if running, err := docker.IsRunning(bundledPostgresContainer); err == nil && running {
		return checks.Result{Name: fmt.Sprintf("Port %d free", bundledPostgresPort), OK: true, Detail: "held by this deployment's own Postgres"}
	}
	r := checks.PortFree(bundledPostgresPort, bundledPostgresContainer)
	if !r.OK {
		r.Detail += fmt.Sprintf(" -- the Postgres of this deployment listens on 127.0.0.1:%d: stop the one already there, or give --postgres-host to use it instead", bundledPostgresPort)
	}
	return r
}

// forgottenPostgresPasswordNote is printed when this configure generated a new
// password for the role while the data volume of an earlier one is still
// there: the data directory keeps the role it was created with, so the
// services would be refused. Nothing is changed: the volume is the data.
func forgottenPostgresPasswordNote(role, dir string) string {
	return fmt.Sprintf(`Postgres: the data volume %s already exists, and this configure generated a new
password for the role %q (the OpenBao it reads is new). If a Postgres was initialised in
that volume, it still has the old password, and the services will be refused until
the role is given the new one:

  docker exec -it %s psql -U %s -d postgres -c '\password %s'

and enter the POSTGRES_PASSWORD from %s (it is also in OpenBao). The container has
to be running for that, which `+"`versola up`"+` does once this configure has finished (the services
will be refused until the role has the new password). Nothing in the volume was changed.
`, bundledPostgresVolume, role, bundledPostgresContainer, role, role, filepath.Join(dir, "auth.secrets.env"))
}
