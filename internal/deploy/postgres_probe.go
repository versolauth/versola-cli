package deploy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/versolauth/versola-cli/internal/checks"
)

// The probe of a Postgres of the customer's own: can the services reach it the
// way they will (TCP, TLS with the CA they will be given, the role's login),
// which server it is and whether the schemas are there. It runs in `configure
// vps` (with the role's password from the bundle) and in `doctor --target vps
// --postgres-host` (without one: the TLS handshake comes before the login, so
// reaching the login step already proves the transport).
//
// One psql run answers all of it: libpq's error text says which step failed.
// The probe only reads (`SELECT` on the catalogs) and, like the rollout
// pre-flight and `postgres init`, puts the password only into a container's
// environment, never on a command line, and prints none of it.

// PgProbeKind is how a probe ended.
type PgProbeKind int

const (
	// PgProbeOK: connected (or, without a password, got as far as the login).
	PgProbeOK PgProbeKind = iota
	// PgProbeWarn: the probe could not run, or found something that deserves a
	// look but is not definite. configure goes on.
	PgProbeWarn
	// PgProbeNotYet: the login (or the database) is refused because the role is
	// not set up yet -- what the first configure of a Postgres of one's own
	// looks like until the role SQL it printed has been run.
	PgProbeNotYet
	// PgProbeFail: definite, and the services would crash-loop on it at `up`:
	// the certificate or the TLS mode. configure stops.
	PgProbeFail
)

// PgProbe is the outcome: one line for the operator.
type PgProbe struct {
	Kind   PgProbeKind
	Detail string
}

// pgProbeSQL is the whole read: the server's version number and which of the
// three schemas exist, as `-At -F '|'` prints it: "180005|auth,central".
const pgProbeSQL = `SELECT current_setting('server_version_num') || '|' || coalesce((SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_catalog.pg_namespace WHERE nspname IN ('auth', 'central', 'edge')), '');`

// pgVerifiedMajor is the Postgres major version the project's CI runs against.
const pgVerifiedMajor = 18

var (
	pgCertNameRe = regexp.MustCompile(`server certificate for "([^"]*)".*does not match host name "([^"]*)"`)
	pgDBMissing  = regexp.MustCompile(`database "[^"]*" does not exist`)
)

// ProbePostgres connects with c and reads pgProbeSQL. loginTested is false when
// c has no password (doctor): a refused login is then the expected end, not a
// finding. first is true on the configure that has just generated the role's
// password: the role cannot exist yet.
func ProbePostgres(c pgConn, loginTested, first bool, run psqlRunner) PgProbe {
	out, err := run(c, pgProbeSQL, nil)
	if err != nil {
		return classifyPgProbeError(err.Error(), c, loginTested, first)
	}
	return readPgProbe(out, c)
}

// readPgProbe turns the answer of pgProbeSQL into a line.
func readPgProbe(out string, c pgConn) PgProbe {
	f := strings.Split(strings.TrimSpace(out), "|")
	num, err := strconv.Atoi(f[0])
	if len(f) != 2 || err != nil {
		return PgProbe{Kind: PgProbeWarn, Detail: "psql's answer was not understood, so the connection could not be judged"}
	}
	major, minor := num/10000, num%10000
	have := map[string]bool{}
	for _, s := range strings.Split(f[1], ",") {
		have[s] = true
	}
	var missing []string
	for _, s := range initSchemas {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	line := fmt.Sprintf("connected to %s:%s as %s (TLS and login ok), PostgreSQL %d.%d", c.host, c.port, c.user, major, minor)
	if c.password == "" {
		line = fmt.Sprintf("%s:%s answers over TLS (the server let a password-less connection in), PostgreSQL %d.%d", c.host, c.port, major, minor)
	}
	if len(missing) == 0 {
		line += ", schemas auth, central and edge exist"
	} else {
		line += ", schemas missing: " + strings.Join(missing, ", ") + " (`versola postgres init` creates them)"
	}
	if major < pgVerifiedMajor {
		return PgProbe{Kind: PgProbeWarn, Detail: line + fmt.Sprintf("; CI runs against PostgreSQL %d, older versions are not verified", pgVerifiedMajor)}
	}
	return PgProbe{Kind: PgProbeOK, Detail: line}
}

// classifyPgProbeError words a failed run: what is wrong and the flag or step
// that fixes it. msg is psql's error line (no password in it) or ours.
func classifyPgProbeError(msg string, c pgConn, loginTested, first bool) PgProbe {
	addr := c.host + ":" + c.port
	low := strings.ToLower(msg)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("docker couldn't run psql", "executable file not found", "cannot find the file", "cannot connect to the docker", "docker daemon"):
		return PgProbe{Kind: PgProbeWarn, Detail: "the connection could not be checked, because " + trimDot(msg) + " -- it is not a problem of the database; the services will connect when they start"}
	case pgCertNameRe.MatchString(msg):
		m := pgCertNameRe.FindStringSubmatch(msg)
		return PgProbe{Kind: PgProbeFail, Detail: fmt.Sprintf("the certificate of %s is issued for %s, not %s -- pass the name from the certificate's SAN as --postgres-host, or the CA that signed it with --postgres-ca", addr, trimDot(m[1]), trimDot(m[2]))}
	case has("does not match host name"):
		return PgProbe{Kind: PgProbeFail, Detail: fmt.Sprintf("the certificate of %s does not carry the name %s -- pass the name from the certificate's SAN as --postgres-host", addr, c.host)}
	case has("root certificate") && c.caFile == "":
		return PgProbe{Kind: PgProbeFail, Detail: "no CA file was given and the system's CAs were not enough: " + trimDot(msg) + " -- pass the CA that signed the server's certificate as --postgres-ca <file>"}
	case has("root certificate"):
		return PgProbe{Kind: PgProbeFail, Detail: "the CA file given with --postgres-ca could not be used: " + trimDot(msg)}
	case has("certificate verify failed", "unable to get local issuer", "self-signed", "self signed", "unknown ca", "certificate has expired", "certificate is not yet valid"):
		return PgProbe{Kind: PgProbeFail, Detail: fmt.Sprintf("the certificate of %s is not accepted (%s) -- pass the CA that signed it as --postgres-ca <file>, or fix the server's certificate chain or its dates", addr, trimDot(sslReason(msg)))}
	case has("does not support ssl"):
		return PgProbe{Kind: PgProbeFail, Detail: fmt.Sprintf("%s does not offer TLS, but the services connect with %s -- turn ssl on in the server, or ask for it in your provider's settings", addr, orDefault(c.sslmode, "verify-full"))}
	case has("password authentication failed", "no password supplied", "authentication failed", "peer authentication failed"):
		switch {
		case !loginTested:
			return PgProbe{Kind: PgProbeOK, Detail: fmt.Sprintf("%s answers over TLS (the certificate is accepted); the login is not tried here -- `versola configure` tries it with the role's password", addr)}
		case first:
			return PgProbe{Kind: PgProbeNotYet, Detail: fmt.Sprintf("TLS to %s is fine; the login is refused -- not yet: run the role SQL printed above (or `versola postgres init`), then `versola migrate`", addr)}
		default:
			return PgProbe{Kind: PgProbeWarn, Detail: fmt.Sprintf("TLS to %s is fine, but the login as %s is refused: the role does not exist or has another password than the deployment's POSTGRES_PASSWORD -- run `versola postgres init`, or set it by hand", addr, c.user)}
		}
	case pgDBMissing.MatchString(msg):
		if first {
			return PgProbe{Kind: PgProbeNotYet, Detail: fmt.Sprintf("connected to %s, but the database %q does not exist yet -- not yet: `versola postgres init` creates it", addr, c.db)}
		}
		return PgProbe{Kind: PgProbeWarn, Detail: fmt.Sprintf("connected to %s, but the database %q does not exist -- `versola postgres init` creates it", addr, c.db)}
	case has("pg_hba.conf") && !loginTested:
		return PgProbe{Kind: PgProbeOK, Detail: fmt.Sprintf("%s answers over TLS; whether the role may log in from here is tried by `versola configure`", addr)}
	case has("pg_hba.conf"):
		return PgProbe{Kind: PgProbeWarn, Detail: fmt.Sprintf("%s has no pg_hba.conf entry for this host, user and database over TLS -- add a `hostssl` line for them (%s)", addr, trimDot(msg))}
	case has("could not translate host name", "name or service not known", "no such host"):
		return PgProbe{Kind: PgProbeWarn, Detail: fmt.Sprintf("the name %s does not resolve from this host -- check --postgres-host", c.host)}
	case has("connection refused", "is the server running", "accepting tcp/ip", "timeout expired", "timed out", "no route to host", "network is unreachable", "could not connect"):
		return PgProbe{Kind: PgProbeWarn, Detail: fmt.Sprintf("%s can't be reached from this host (%s) -- check the address, that Postgres listens on it, and the firewall", addr, trimDot(msg))}
	}
	return PgProbe{Kind: PgProbeWarn, Detail: "the connection to " + addr + " could not be checked: " + trimDot(msg)}
}

// sslReason is the part of libpq's message that says why TLS failed.
func sslReason(msg string) string {
	if i := strings.Index(strings.ToLower(msg), "ssl error:"); i >= 0 {
		return strings.TrimSpace(msg[i+len("ssl error:"):])
	}
	return msg
}

// trimDot makes a server's or libpq's text fit one line: control characters
// (a server chooses what it sends) become spaces.
func trimDot(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimRight(strings.TrimSpace(s), ".")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// PgProbeResult shows a probe the way doctor lists its checks.
func PgProbeResult(p PgProbe) checks.Result {
	return checks.Result{Name: "Postgres connection", OK: p.Kind != PgProbeFail, Warn: p.Kind == PgProbeWarn || p.Kind == PgProbeNotYet, Detail: p.Detail}
}

// bundlePgConn is how the services of the bundle in dir reach a Postgres of the
// customer's own: auth.conf's address, database, role and TLS settings, the CA
// file of the bundle and the role's password of auth.secrets.env.
func bundlePgConn(dir string) (pgConn, error) {
	b, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
	if err != nil {
		return pgConn{}, fmt.Errorf("couldn't read auth.conf: %w", err)
	}
	t, err := parsePgTarget(string(b))
	if err != nil {
		return pgConn{}, fmt.Errorf("couldn't read the Postgres settings of auth.conf: %v", err)
	}
	values, err := readDotenv(filepath.Join(dir, "auth.secrets.env"))
	if err != nil {
		return pgConn{}, err
	}
	c := pgConn{host: t.host, port: t.port, db: t.db, user: t.user, sslmode: t.sslmode, password: values["POSTGRES_PASSWORD"]}
	if t.rootCert != "" && pgVerifies(t.sslmode) {
		if c.caFile, err = pgCAOnHost(dir, t.rootCert); err != nil {
			return pgConn{}, err
		}
	}
	return c, nil
}

// ProbeConfiguredPostgres is the probe configure runs on the bundle in dir,
// once the role's password is known. first: that password was generated by
// this configure, so the role cannot have it yet.
func ProbeConfiguredPostgres(dir string, first bool) PgProbe {
	return probeConfiguredPostgres(dir, first, runPsql)
}

func probeConfiguredPostgres(dir string, first bool, run psqlRunner) PgProbe {
	c, err := bundlePgConn(dir)
	if err != nil {
		return PgProbe{Kind: PgProbeWarn, Detail: "the connection could not be checked: " + err.Error()}
	}
	return ProbePostgres(c, c.password != "", first, run)
}

// PostgresProbeCheck is doctor's probe of the Postgres given as --postgres-host
// (or recorded), with the CA of --postgres-ca. Nothing without a Postgres of
// the customer's own, or when it is on this host (the services do not verify a
// loopback address). No role or password is known before configure: the probe
// ends at the login, which proves the address and the TLS.
func PostgresProbeCheck(postgresHost, caFile string) (checks.Result, bool) {
	stored, err := storedVps()
	if err != nil && postgresHost == "" {
		return checks.Result{}, false
	}
	mode, host, err := resolvePostgres("vps", postgresHost, stored, nil)
	if err != nil || mode == PostgresModeBundled || host == "" {
		return checks.Result{}, false
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		h, port = strings.Trim(host, "[]"), "5432"
	}
	if isLoopbackHost(h) {
		return checks.Result{}, false
	}
	c := pgConn{host: h, port: port, db: "postgres", user: "versola"}
	// Without --postgres-ca, the CA the deployment here was configured with.
	// Only when it is the same Postgres: another one has its own CA.
	recorded := false
	if caFile == "" && stored != nil && (postgresHost == "" || postgresHost == stored.PostgresHost) {
		if caFile = stored.PostgresCAPath(); caFile != "" {
			recorded = true
		}
	}
	if caFile != "" {
		abs, err := filepath.Abs(caFile)
		if err == nil {
			var fi os.FileInfo
			if fi, err = os.Stat(abs); err == nil && !fi.Mode().IsRegular() {
				err = fmt.Errorf("not a file")
			}
		}
		if err == nil && strings.Contains(strings.TrimPrefix(abs, filepath.VolumeName(abs)), ":") {
			err = fmt.Errorf("a ':' in the path can't be mounted")
		}
		if err != nil && recorded {
			return checks.Result{Name: "Postgres connection", OK: true, Warn: true, Detail: fmt.Sprintf("not checked: the CA recorded by configure can't be read (%v) -- pass --postgres-ca <file>", err)}, true
		}
		if err != nil {
			return checks.Result{Name: "Postgres connection", Detail: fmt.Sprintf("--postgres-ca %s can't be read: %v", caFile, err)}, true
		}
		c.caFile = abs
	}
	return PgProbeResult(ProbePostgres(c, false, false, runPsql)), true
}
