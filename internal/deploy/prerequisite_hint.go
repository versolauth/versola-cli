package deploy

import (
	"fmt"
	"strings"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// prerequisiteFailure is the error of a failed prerequisite check. It ends
// with the command that shows every check again. On vps that is `versola
// doctor --target vps` with the flags of this run: bare `versola doctor`
// checks local, with other memory thresholds and no port 5432, and would
// answer "ready for bootstrap local" to a customer whose server just failed.
func prerequisiteFailure(target, authURL, postgresHost string, pg PostgresOptions, proxyOpts ProxyOptions, registry string) error {
	if target != "vps" {
		return fmt.Errorf("prerequisite check failed -- run `versola doctor` for details")
	}
	return fmt.Errorf("prerequisite check failed -- run `%s` to see every check",
		doctorVpsCommand(authURL, postgresHost, pg, proxyOpts, registry))
}

// runsBundledPostgres: the deployment recorded on this machine runs its own
// Postgres in a container.
func runsBundledPostgres(stored *state.State) bool {
	return stored != nil && stored.Target == "vps" && stored.PostgresMode == PostgresModeBundled
}

// bundledPostgresPortFailure is the error of a taken port 5432 when this
// deployment runs its own Postgres: the way out, not a pointer to doctor.
// recorded is true when the deployment on this machine is already recorded as
// running its own Postgres: configure then refuses --postgres-host (moving the
// data is a backup and a restore), so the only way out is to free the port.
func bundledPostgresPortFailure(recorded bool) error {
	if recorded {
		return fmt.Errorf("port %d is already in use on this server, and this deployment's own Postgres needs it. Stop whatever holds it, then run this again",
			bundledPostgresPort)
	}
	return fmt.Errorf("port %d is already in use on this server. Stop whatever holds it, or, if it is a Postgres you want to keep, run this again with --postgres-host %s to use it as your own Postgres (`versola postgres init` prepares its role and database)",
		bundledPostgresPort, BundledPostgresHost)
}

// doctorVpsCommand is the `versola doctor --target vps` that checks what
// configure was given: the values after validation (the normalized auth URL,
// the resolved Postgres), a flag only when it differs from what doctor would
// assume without it.
func doctorVpsCommand(authURL, postgresHost string, pg PostgresOptions, proxyOpts ProxyOptions, registry string) string {
	parts := []string{"versola doctor --target vps", "--auth-url " + shellQuote(authURL)}
	add := func(flag, value string) {
		if value != "" {
			parts = append(parts, flag+" "+shellQuote(value))
		}
	}
	if proxyOpts.Mode != "" && proxyOpts.Mode != proxy.ModeNginx {
		add("--proxy", proxyOpts.Mode)
	}
	if pg.Mode != PostgresModeBundled {
		add("--postgres-host", postgresHost)
		add("--postgres-ca", pg.CAFile)
	}
	add("--registry", registry)
	add("--central-url", proxyOpts.CentralURL)
	add("--mtls-domain", proxyOpts.Public.MTLS)
	add("--central-mtls-domain", proxyOpts.Public.CentralMTLS)
	add("--central-domain", proxyOpts.Public.Enroll)
	return strings.Join(parts, " ")
}
