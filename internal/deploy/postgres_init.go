package deploy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/fsutil"
	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/state"
)

// `versola postgres init` makes the Postgres a vps deployment was pointed at
// ready for it: the role, the database and the schemas auth, central and edge
// that versola-tools' configs name. It is for a Postgres of the customer's
// own; the one that runs in a container creates all of this by itself.
//
// What it does depends on the rights it finds (planInit). It only creates what
// is missing: no DROP, no ALTER of an existing role, database or schema. The
// password of the role is the one stored in OpenBao (and written into
// auth.secrets.env), set only on a role created here.

// initNameRe is what a role or database name has to look like for this command.
var initNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// initSchemas are the schemas the three services keep their tables in, one
// each (deploy.md, "Database, role and schemas").
var initSchemas = []string{"auth", "central", "edge"}

// pgConn is how psql reaches a Postgres. The password goes into the container's
// environment (or is not needed, with sudo), never onto a command line.
type pgConn struct {
	host, port, db, user string
	// sslmode is as in the services' URL; empty is their default, verify-full.
	sslmode string
	// caFile is the CA file on this host, "" when there is none.
	caFile   string
	password string
	// sudo reaches the server on this machine as the postgres OS user, over the
	// local socket: host, port, user, TLS and password do not apply.
	sudo bool
}

// psqlRunner runs sql (fed on stdin) with the psql variables vars on c and
// returns what psql printed on stdout.
type psqlRunner func(c pgConn, sql string, vars map[string]string) (string, error)

// ---- the admin connection ----

// parseAdminURL reads --admin-url. A URL with a password is refused: it would
// stay in the shell's history and in the process list.
func parseAdminURL(raw string) (pgConn, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return pgConn{}, fmt.Errorf("--admin-url is not a URL like postgres://ADMIN@db.example.com:5432/postgres?sslmode=verify-full")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return pgConn{}, fmt.Errorf("--admin-url must start with postgres:// (or postgresql://)")
	}
	if u.User == nil || u.User.Username() == "" {
		return pgConn{}, fmt.Errorf("--admin-url needs the admin user: postgres://ADMIN@%s/postgres", u.Host)
	}
	if _, has := u.User.Password(); has {
		return pgConn{}, fmt.Errorf("--admin-url must not carry a password: it would land in the shell history and the process list. Give it with PGPASSWORD, --admin-password-file or at the prompt")
	}
	q := u.Query()
	for k := range q {
		if k != "sslmode" {
			return pgConn{}, fmt.Errorf("--admin-url: the parameter %q is not supported (only sslmode); the CA file is the deployment's own (--postgres-ca)", k)
		}
	}
	c := pgConn{host: u.Hostname(), port: u.Port(), db: strings.TrimPrefix(u.Path, "/"), user: u.User.Username(), sslmode: q.Get("sslmode")}
	if c.port == "" {
		c.port = "5432"
	}
	if c.db == "" {
		c.db = "postgres"
	}
	return c, nil
}

// adminPassword picks the admin's password: --admin-password-file, else
// PGPASSWORD, else the prompt. The first is the most deliberate, the last the
// least; none leaves it to the prompt to fail when there is no terminal.
func adminPassword(file, envValue string, prompt func() (string, error)) (string, error) {
	if file != "" {
		return readAdminPasswordFile(file)
	}
	if envValue != "" {
		return envValue, nil
	}
	return prompt()
}

// readAdminPasswordFile reads the first line of file. On a system with file
// modes, one that others can read is refused, as ssh does with a key.
func readAdminPasswordFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("--admin-password-file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("--admin-password-file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("--admin-password-file %s is readable by others (mode %04o): run chmod 600 on it", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--admin-password-file: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return "", fmt.Errorf("--admin-password-file %s is empty", path)
	}
	return line, nil
}

// ---- what the deployment asks for ----

// initTarget is the role, the database and the way the services reach them,
// read from the deployment's bundle.
type initTarget struct {
	target     string
	role, db   string
	roleConn   pgConn
	secret     string // the role's password, as stored in OpenBao
	configHost string // host:port as the services dial it
}

// initTargetOf reads dir (the active bundle) of a vps deployment with its own
// Postgres: the user, database, address and TLS mode of auth.conf, and the
// password of auth.secrets.env.
func initTargetOf(st *state.State, dir string) (initTarget, error) {
	if st == nil {
		return initTarget{}, fmt.Errorf("nothing has been configured yet -- run `versola configure vps <version> --auth-url ... --postgres-host ...` first")
	}
	if st.Target != "vps" {
		return initTarget{}, fmt.Errorf("postgres init is for a vps deployment (this one is %q): local runs its own Postgres", st.Target)
	}
	if st.PostgresMode == PostgresModeBundled {
		return initTarget{}, fmt.Errorf("this deployment runs its own Postgres in a container, which creates the role, the database and the schemas by itself: there is nothing to initialise")
	}
	b, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
	if err != nil {
		return initTarget{}, fmt.Errorf("couldn't read the deployment's auth.conf: %w", err)
	}
	t, err := parsePgTarget(string(b))
	if err != nil {
		return initTarget{}, fmt.Errorf("couldn't read the Postgres settings of auth.conf: %v", err)
	}
	// The names go into SQL, where they are quoted, and into psql's meta-commands
	// (\connect, SET ROLE in the script), which have rules of their own: only plain
	// names are accepted, as configure accepts for --postgres-db.
	for what, name := range map[string]string{"user": t.user, "database": t.db} {
		if !initNameRe.MatchString(name) {
			return initTarget{}, fmt.Errorf("the Postgres %s %q of auth.conf is not a plain name (1-63 letters, digits, '_' or '-'): create it by hand as deploy.md describes", what, name)
		}
	}
	values, err := readDotenv(filepath.Join(dir, "auth.secrets.env"))
	if err != nil {
		return initTarget{}, err
	}
	pw := values["POSTGRES_PASSWORD"]
	if pw == "" {
		return initTarget{}, fmt.Errorf("auth.secrets.env has no POSTGRES_PASSWORD -- run `versola configure` again")
	}
	conn := pgConn{host: t.host, port: t.port, db: t.db, user: t.user, sslmode: t.sslmode, password: pw}
	if t.rootCert != "" && pgVerifies(t.sslmode) {
		ca, err := pgCAOnHost(dir, t.rootCert)
		if err != nil {
			return initTarget{}, err
		}
		conn.caFile = ca
	}
	return initTarget{target: st.Target, role: t.user, db: t.db, roleConn: conn, secret: pw, configHost: t.host + ":" + t.port}, nil
}

// ---- the probe ----

// adminProbeSQL is what the admin's connection is asked: its own rights, and
// whether the role and the database exist and whose the database is. psql's
// :'name' quotes the value as a literal.
const adminProbeSQL = `SELECT r.rolsuper, r.rolcreaterole, r.rolcreatedb,
  EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'role'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = :'db'),
  coalesce((SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname = :'db'), ''),
  coalesce((SELECT CASE WHEN current_setting('server_version_num')::int >= 160000
                        THEN pg_catalog.pg_has_role(current_user, :'role', 'SET')
                        ELSE pg_catalog.pg_has_role(current_user, :'role', 'MEMBER') END
            FROM pg_catalog.pg_roles WHERE rolname = :'role'), false)
FROM pg_catalog.pg_roles r WHERE r.rolname = current_user;`

// schemasProbeSQL lists which of the three schemas exist, as the role sees them.
const schemasProbeSQL = `SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname IN ('auth', 'central', 'edge') ORDER BY 1;`

type adminProbe struct {
	super, createRole, createDB bool
	roleExists, dbExists        bool
	// dbOwner is the owner of the database, "" when it does not exist.
	dbOwner string
	// canSetRole: the admin can already SET ROLE to the role (a member of it),
	// which is what owning a database through it needs.
	canSetRole bool
}

// parseAdminProbe reads adminProbeSQL's one row, as `psql -At -F'|'` prints it.
func parseAdminProbe(out string) (adminProbe, error) {
	f := strings.Split(strings.TrimSpace(out), "|")
	if len(f) != 7 {
		return adminProbe{}, fmt.Errorf("unexpected answer from psql")
	}
	b := func(s string) bool { return s == "t" }
	return adminProbe{
		super: b(f[0]), createRole: b(f[1]), createDB: b(f[2]),
		roleExists: b(f[3]), dbExists: b(f[4]), dbOwner: f[5], canSetRole: b(f[6]),
	}, nil
}

// parseSchemas reads schemasProbeSQL's rows.
func parseSchemas(out string) map[string]bool {
	have := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			have[l] = true
		}
	}
	return have
}

// ---- the plan ----

// initStep is one thing to run: on the admin's connection, or on the role's
// (as the role, in the database), with the text that is run and the text that
// is shown, which hides the password's verifier.
type initStep struct {
	asRole bool
	desc   string
	sql    string
	shown  string
}

type initPlan struct {
	steps []initStep
	// blocked is what stops it, as sentences; nothing is run then.
	blocked []string
	// missingRights are the admin's missing rights, when that is why it stops.
	missingRights []string
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// initFacts is what was found.
type initFacts struct {
	// adminKnown: an admin connection was given and probed.
	adminKnown bool
	probe      adminProbe
	// schemas are the three schemas found as the role, when the role could ask
	// (it exists, owns the database).
	schemas map[string]bool
}

// planInit chooses by the rights found (the issue's table):
//
//   - an admin who can create roles and databases: the role (if missing), the
//     database owned by it (if missing), then the schemas as that role;
//   - an existing role that owns the database, with no admin or any: only the
//     schemas missing, as that role;
//   - not enough rights, or a database that is someone else's: nothing is run,
//     blocked says what is missing.
//
// secret is the clause after PASSWORD (a literal), shownSecret its masked twin.
func planInit(f initFacts, t initTarget, secret, shownSecret string) initPlan {
	var plan initPlan
	role, db := t.role, t.db
	needRole, needDB := false, false
	if f.adminKnown {
		p := f.probe
		needRole, needDB = !p.roleExists, !p.dbExists
		if p.dbExists && p.dbOwner != role {
			plan.blocked = append(plan.blocked, fmt.Sprintf("the database %q exists and belongs to %q, not to %q. This command never changes an existing database: let %q own it (ALTER DATABASE %s OWNER TO %s, as its owner or a superuser), or point the deployment at another one with --postgres-db", db, p.dbOwner, role, role, quoteIdent(db), quoteIdent(role)))
		}
		if needRole && !(p.super || p.createRole) {
			plan.missingRights = append(plan.missingRights, "CREATEROLE (or superuser), to create the role "+quoteIdent(role))
		}
		if needDB && !(p.super || p.createDB) {
			plan.missingRights = append(plan.missingRights, "CREATEDB (or superuser), to create the database "+quoteIdent(db))
		}
		if needDB && !needRole && !p.super && !p.canSetRole {
			plan.blocked = append(plan.blocked, fmt.Sprintf("the database %q has to be created owned by the role %q, which exists, and a role that is not a superuser can only do that as a member of it. This command does not grant membership of a role that exists: run it as a superuser, or create the database yourself (CREATE DATABASE %s OWNER %s) and run this again for the schemas", db, role, quoteIdent(db), quoteIdent(role)))
		}
		if len(plan.blocked) > 0 || len(plan.missingRights) > 0 {
			return plan
		}
		if needRole {
			plan.steps = append(plan.steps, initStep{
				desc:  "create the role",
				sql:   fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s;", quoteIdent(role), secret),
				shown: fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s;", quoteIdent(role), shownSecret),
			})
		}
		if needDB {
			sql := ""
			// A role that is not a superuser can only make a database owned by a
			// role it is a member of: the role it has just created, which it may
			// grant itself (from PostgreSQL 16 with the SET option, which is what
			// owning needs and is not given to the creator by default). A role that
			// exists is not granted: the admin is a member of it already (a run that
			// was cut off after creating it), or the plan was refused above.
			if !p.super && needRole {
				sql += grantToAdminSQL(quoteIdent(role))
			}
			sql += fmt.Sprintf("CREATE DATABASE %s OWNER %s;", quoteIdent(db), quoteIdent(role))
			plan.steps = append(plan.steps, initStep{desc: "create the database, owned by the role", sql: sql, shown: sql})
		}
	}
	// The schemas, as the role: all of them in a database made now, else the
	// ones the role found missing.
	var missing []string
	for _, s := range initSchemas {
		if needDB || !f.schemas[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		var b strings.Builder
		for _, s := range missing {
			fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %s;\n", quoteIdent(s))
		}
		sql := strings.TrimRight(b.String(), "\n")
		plan.steps = append(plan.steps, initStep{asRole: true, desc: "create the schemas, as the role", sql: sql, shown: sql})
	}
	return plan
}

// ---- the file for an admin to run ----

// grantToAdminSQL makes the admin a member of the role it has just created,
// with the right to SET ROLE to it: the SET option exists (and is needed) from
// PostgreSQL 16, before it any membership allows it.
func grantToAdminSQL(role string) string {
	return "SELECT current_setting('server_version_num')::int >= 160000 AS versola_pg16 \\gset\n" +
		"\\if :versola_pg16\n" +
		"GRANT " + role + " TO CURRENT_USER WITH SET TRUE;\n" +
		"\\else\n" +
		"GRANT " + role + " TO CURRENT_USER;\n" +
		"\\endif\n"
}

// initScript is what an admin with enough rights runs when this machine could
// not: the same as the plan, as one psql script. It creates only what is
// missing, so it can be run again. A superuser creates the database and then
// the schemas as the role (SET ROLE); an admin that is not one makes itself a
// member of the role it creates first, as the plan does, and never of a role
// that existed before the script ran.
func initScript(t initTarget, secret string) string {
	role, db := quoteIdent(t.role), quoteIdent(t.db)
	var b strings.Builder
	b.WriteString("-- Written by `versola postgres init`: makes this Postgres ready for Versola.\n")
	b.WriteString("-- It creates the role, the database and the schemas that are missing and changes nothing\n")
	b.WriteString("-- that exists. Run it as a Postgres role that can create roles and databases, e.g.\n")
	b.WriteString("--   sudo -u postgres psql < <this file>\n")
	b.WriteString("-- A role that is not a superuser needs to be allowed to SET ROLE to the new role: it is, when this\n")
	b.WriteString("-- file creates the role. It holds the password's SCRAM-SHA-256 verifier (the password itself\n")
	b.WriteString("-- if it is not ASCII): keep it private.\n")
	b.WriteString("\\set ON_ERROR_STOP on\n")
	b.WriteString("SET standard_conforming_strings = on;\n")
	fmt.Fprintf(&b, "SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = %s) AS versola_create_role \\gset\n", sqlLiteral(t.role))
	b.WriteString("\\if :versola_create_role\n")
	fmt.Fprintf(&b, "CREATE ROLE %s WITH LOGIN PASSWORD %s;\n", role, secret)
	b.WriteString("\\endif\n")
	fmt.Fprintf(&b, "SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = %s) AS versola_create_db, (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user) AS versola_super \\gset\n", sqlLiteral(t.db))
	b.WriteString("\\if :versola_create_db\n")
	b.WriteString("\\if :versola_super\n")
	b.WriteString("\\else\n")
	b.WriteString("\\if :versola_create_role\n")
	b.WriteString(grantToAdminSQL(role))
	b.WriteString("\\endif\n")
	b.WriteString("\\endif\n")
	fmt.Fprintf(&b, "CREATE DATABASE %s OWNER %s;\n", db, role)
	b.WriteString("\\endif\n")
	fmt.Fprintf(&b, "\\connect %s\n", db)
	fmt.Fprintf(&b, "SET ROLE %s;\n", role)
	for _, s := range initSchemas {
		fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %s;\n", quoteIdent(s))
	}
	return b.String()
}

// secretClause is the literal after PASSWORD: the SCRAM verifier of the
// password, so that the password is not in the SQL (see scramVerifier), or the
// password itself when it has none. shown is its masked twin.
func secretClause(password string) (clause, shown string, err error) {
	v, ok, err := scramVerifier(password)
	if err != nil {
		return "", "", err
	}
	if !ok {
		v = password
	}
	return sqlLiteral(v), "'<hidden: the POSTGRES_PASSWORD of the deployment>'", nil
}

// ---- the real runner ----

const (
	pgInitAdminEnv = "VERSOLA_PG_PASSWORD"
	pgInitScript   = `PGPASSWORD="$VERSOLA_PG_PASSWORD" exec psql -XAtq -v ON_ERROR_STOP=1 -F '|' "$@" -f -`
)

// pgTLSArgs sets TLS up as the services do: sslmode (verify-full by default),
// checked against caFile (mounted read-only) when there is one, else the
// system's CAs. No root certificate is passed for the modes that do not verify:
// libpq, unlike the JDBC driver, would turn require into verify-ca once it has
// one.
func pgTLSArgs(sslmode, caFile string) []string {
	mode := sslmode
	if mode == "" {
		mode = "verify-full"
	}
	args := []string{"-e", "PGSSLMODE=" + mode}
	switch {
	case !pgVerifies(mode):
	case caFile != "":
		in := pgCAContainerDir + "/ca.crt"
		args = append(args, "-v", caFile+":"+in+":ro", "-e", "PGSSLROOTCERT="+in)
	case mode == "verify-full":
		// libpq accepts "system" only with verify-full; verify-ca without a
		// CA file of its own fails the run, which then says so.
		args = append(args, "-e", "PGSSLROOTCERT=system")
	}
	return args
}

// psqlVarArgs are `-v name=value` pairs in a fixed order; they hold names, never
// a secret.
func psqlVarArgs(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		args = append(args, "-v", k+"="+vars[k])
	}
	return args
}

// pgDockerArgs is the docker command line of a psql run: a throwaway container
// on the host's network, the connection in environment variables, the password
// taken from this process's environment by name (-e NAME), the SQL on stdin.
func pgDockerArgs(c pgConn, vars map[string]string) []string {
	return pgDockerArgsOn(c, vars, "host")
}

// pgDockerArgsOn is pgDockerArgs on the Docker network named network ("host"
// for real; a test shares the network of a container that runs the server).
func pgDockerArgsOn(c pgConn, vars map[string]string, network string) []string {
	args := []string{"run", "--rm", "-i", "--pull", "missing", "--network", network,
		"-e", "PGHOST=" + c.host, "-e", "PGPORT=" + c.port, "-e", "PGDATABASE=" + c.db, "-e", "PGUSER=" + c.user,
		"-e", "PGCONNECT_TIMEOUT=10", "-e", "PGAPPNAME=versola-postgres-init"}
	args = append(args, pgTLSArgs(c.sslmode, c.caFile)...)
	args = append(args, "-e", pgInitAdminEnv, pgCheckImage, "sh", "-c", pgInitScript, "sh")
	return append(args, psqlVarArgs(vars)...)
}

// pgSudoArgs is the command line of psql as the postgres OS user, on this machine.
func pgSudoArgs(c pgConn, vars map[string]string) []string {
	args := []string{"-u", "postgres", "psql", "-XAtq", "-v", "ON_ERROR_STOP=1", "-F", "|", "-d", c.db}
	// The cluster the deployment points at, when this server has more than one.
	if c.port != "" {
		args = append(args, "-p", c.port)
	}
	args = append(args, psqlVarArgs(vars)...)
	return append(args, "-f", "-")
}

// runPsql is the real psqlRunner.
func runPsql(c pgConn, sql string, vars map[string]string) (string, error) {
	return runPsqlOn(c, sql, vars, "host")
}

// runPsqlOn is runPsql on the Docker network named network.
func runPsqlOn(c pgConn, sql string, vars map[string]string, network string) (string, error) {
	var cmd *exec.Cmd
	if c.sudo {
		cmd = exec.Command("sudo", pgSudoArgs(c, vars)...)
	} else {
		cmd = docker.Cmd(pgDockerArgsOn(c, vars, network)...)
		cmd.Env = append(os.Environ(), pgInitAdminEnv+"="+c.password)
	}
	var out, errOut bytes.Buffer
	cmd.Stdin = strings.NewReader(sql)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !c.sudo && errors.As(err, &exit) && exit.ExitCode() == 125 {
			// docker itself failed: pulling the image, or starting it. Its message is not repeated.
			return "", fmt.Errorf("docker couldn't run psql (pulling %s, or starting it, failed)", pgCheckImage)
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%v", err)
		}
		// psql's own errors (refused, authentication failed, no such database)
		// name the host and the user, never the password.
		if line := psqlErrorLine(errOut.String()); line != "" {
			return "", fmt.Errorf("%s", line)
		}
		return "", err
	}
	return out.String(), nil
}

// psqlErrorLine is the line of psql's (or sudo's) stderr that says what went
// wrong: the first ERROR or FATAL line. A "LINE n:" line quotes the statement,
// which may hold a verifier, and is never used; neither is the caret under it.
func psqlErrorLine(stderr string) string {
	last := ""
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "LINE ") || strings.Trim(l, "^ ") == "" {
			continue
		}
		if strings.Contains(l, "ERROR:") || strings.Contains(l, "FATAL:") {
			return l
		}
		last = l
	}
	return last
}

// isLoopbackHost: localhost or a loopback address.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// hostIsLocal reports whether host is this machine: a loopback name or
// address, a socket directory, this machine's own host name, or a name that
// resolves to one of its addresses. --sudo-postgres uses the local server over
// its socket, so it is only right for a deployment that points at this one.
func hostIsLocal(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasPrefix(h, "/") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	if name, err := os.Hostname(); err == nil && h == strings.ToLower(name) {
		return true
	}
	// A name that resolves to a loopback or to an address of this machine. Name
	// prefixes are not compared: "prod" and "prod.other-company.com" are not the
	// same machine, and a wrong "local" would act on the wrong server.
	ips, err := net.LookupIP(h)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if ip.IsLoopback() {
			return true
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		for _, ip := range ips {
			if ip.Equal(ipn.IP) {
				return true
			}
		}
	}
	return false
}

// ---- the command ----

// PostgresInitOptions are the flags of `versola postgres init`.
type PostgresInitOptions struct {
	// AdminURL is --admin-url ("" for none), SudoPostgres is --sudo-postgres.
	AdminURL     string
	SudoPostgres bool
	// AdminPasswordFile is --admin-password-file.
	AdminPasswordFile string
	DryRun, Yes       bool
}

// PostgresInitDeps are the things the command takes from outside: what asks the
// terminal, what runs psql, where the file for an admin goes.
type PostgresInitDeps struct {
	Out io.Writer
	// PGPassword is the value of PGPASSWORD.
	PGPassword string
	// Prompt asks for the admin's password with no echo.
	Prompt func() (string, error)
	// Confirm asks a yes/no question.
	Confirm func(prompt string) bool
	Run     psqlRunner
	// ScriptPath is where the file for an admin goes (0600).
	ScriptPath func(target string) (string, error)
}

// PostgresInit initialises the Postgres of the recorded vps deployment.
func PostgresInit(opts PostgresInitOptions, deps PostgresInitDeps) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) -- configure it again")
	}
	dir := filepath.Dir(composePath)
	if deps.Run == nil {
		deps.Run = runPsql
	}
	if deps.ScriptPath == nil {
		deps.ScriptPath = openbao.PostgresInitScriptPath
	}
	return runPostgresInit(st, dir, opts, deps)
}

func runPostgresInit(st *state.State, dir string, opts PostgresInitOptions, deps PostgresInitDeps) error {
	out := deps.Out
	if opts.AdminURL != "" && opts.SudoPostgres {
		return fmt.Errorf("give --admin-url or --sudo-postgres, not both")
	}
	t, err := initTargetOf(st, dir)
	if err != nil {
		return err
	}
	if opts.SudoPostgres && !hostIsLocal(t.roleConn.host) {
		return fmt.Errorf("--sudo-postgres works on the Postgres of this server, over its socket, but the deployment's Postgres is at %s, which is not this server: use --admin-url with an admin account of that server", t.configHost)
	}

	// The admin's connection, when one was asked for.
	var admin *pgConn
	switch {
	case opts.SudoPostgres:
		admin = &pgConn{sudo: true, db: "postgres", host: "this server", port: t.roleConn.port}
	case opts.AdminURL != "":
		c, err := parseAdminURL(opts.AdminURL)
		if err != nil {
			return err
		}
		// The deployment's mode is the admin's only when it verifies the server, or
		// both are on the loopback (where the deployment may have switched TLS off):
		// a mode that does not verify must not carry over to a server across the
		// network, where the admin's password would cross it in the clear, even to
		// the very host the deployment talks to. Otherwise it is verify-full, as the
		// services' own default.
		if c.sslmode == "" && (pgVerifies(t.roleConn.sslmode) || (isLoopbackHost(c.host) && isLoopbackHost(t.roleConn.host))) {
			c.sslmode = t.roleConn.sslmode
		}
		if c.sslmode == "" || pgVerifies(c.sslmode) {
			c.caFile = t.roleConn.caFile
		}
		c.password, err = adminPassword(opts.AdminPasswordFile, deps.PGPassword, deps.Prompt)
		if err != nil {
			return err
		}
		admin = &c
	default:
		if opts.AdminPasswordFile != "" {
			return fmt.Errorf("--admin-password-file needs --admin-url")
		}
	}

	fmt.Fprintf(out, "Postgres of this deployment: %s, database %q, role %q.\n", t.configHost, t.db, t.role)
	switch {
	case opts.SudoPostgres:
		fmt.Fprintf(out, "Admin: the postgres user of this server (sudo). Make sure the services' address %s is this server's Postgres.\n", t.configHost)
	case admin != nil:
		fmt.Fprintf(out, "Admin: %s at %s:%s.\n", admin.user, admin.host, admin.port)
	default:
		fmt.Fprintf(out, "No admin given: only what the role itself may do (its schemas) is looked at.\n")
	}

	facts := initFacts{adminKnown: admin != nil}
	if admin != nil {
		row, err := deps.Run(*admin, adminProbeSQL, map[string]string{"role": t.role, "db": t.db})
		if err != nil {
			return fmt.Errorf("couldn't ask Postgres as the admin: %v", err)
		}
		if facts.probe, err = parseAdminProbe(row); err != nil {
			return err
		}
	}
	// A role that exists and a database still to make: the role has to be able
	// to log in with the stored password, or the database would be made and the
	// schemas then refused. Asked before anything is changed.
	if admin != nil && facts.probe.roleExists && !facts.probe.dbExists {
		c := t.roleConn
		c.db = "postgres"
		if _, err := deps.Run(c, "SELECT 1;", nil); err != nil {
			return fmt.Errorf("the role %q exists, but it could not log in (to the postgres database) with the password stored for the deployment: %v\nIf it is the password: this command never sets the password of a role that exists, set it to the POSTGRES_PASSWORD of the deployment's auth.secrets.env yourself. If it is not (the role may not be allowed to log in, or to connect to that database from here), fix that. Nothing was changed", t.role, err)
		}
	}
	// The schemas, as the role, when it can ask: it exists and owns the database
	// (with no admin, that is taken for granted and fails below if it is not so).
	if admin == nil || (facts.probe.roleExists && facts.probe.dbExists && facts.probe.dbOwner == t.role) {
		row, err := deps.Run(t.roleConn, schemasProbeSQL, nil)
		if err != nil {
			if admin == nil {
				return fmt.Errorf("couldn't log in as the role %q to %q: %v\nIf the role or the database does not exist yet, give an admin: --admin-url postgres://ADMIN@host:5432/postgres (its password in PGPASSWORD, --admin-password-file or at the prompt), or --sudo-postgres on this server", t.role, t.db, err)
			}
			return fmt.Errorf("the role %q exists, but it could not log in to %q with the password stored for the deployment: %v\nThis command never sets the password of a role that exists: set it to the POSTGRES_PASSWORD of the deployment's auth.secrets.env yourself", t.role, t.db, err)
		}
		facts.schemas = parseSchemas(row)
	}

	secret, shownSecret, err := secretClause(t.secret)
	if err != nil {
		return err
	}
	plan := planInit(facts, t, secret, shownSecret)

	if len(plan.blocked) > 0 || len(plan.missingRights) > 0 {
		return writeBlocked(out, plan, t, secret, deps, opts.DryRun)
	}
	if len(plan.steps) == 0 {
		fmt.Fprintf(out, "Nothing to do: the role %q owns the database %q and the schemas %s exist.\n", t.role, t.db, strings.Join(initSchemas, ", "))
		return nil
	}

	fmt.Fprintln(out, "\nThis will run:")
	for _, s := range plan.steps {
		who := "as the admin"
		if s.asRole {
			who = fmt.Sprintf("as the role %q", t.role)
		}
		fmt.Fprintf(out, "\n-- %s (%s)\n%s\n", s.desc, who, s.shown)
	}
	if opts.DryRun {
		fmt.Fprintln(out, "\nDry run: nothing was changed.")
		return nil
	}
	if !opts.Yes {
		if deps.Confirm == nil || !deps.Confirm("\nContinue?") {
			fmt.Fprintln(out, "Aborted. Nothing was changed.")
			return fmt.Errorf("aborted: nothing was changed (--yes answers the question)")
		}
	}

	// The admin's steps in one run, then the role's.
	var adminSQL, roleSQL []string
	for _, s := range plan.steps {
		if s.asRole {
			roleSQL = append(roleSQL, s.sql)
		} else {
			adminSQL = append(adminSQL, s.sql)
		}
	}
	if len(adminSQL) > 0 {
		if _, err := deps.Run(*admin, "SET standard_conforming_strings = on;\n"+strings.Join(adminSQL, "\n")+"\n", nil); err != nil {
			return fmt.Errorf("the admin's statements failed: %v (what ran before the failure stays; run this again to continue, it creates only what is missing)", err)
		}
	}
	if len(roleSQL) > 0 {
		if _, err := deps.Run(t.roleConn, strings.Join(roleSQL, "\n")+"\n", nil); err != nil {
			return fmt.Errorf("couldn't create the schemas as the role %q: %v\nIf it is a password failure, the role has another password than the deployment's POSTGRES_PASSWORD (auth.secrets.env), which this command never changes on a role that exists: set it to that value, then run this again -- it continues with what is missing", t.role, err)
		}
	}

	// What was asked for is checked, not assumed.
	row, err := deps.Run(t.roleConn, schemasProbeSQL, nil)
	if err != nil {
		return fmt.Errorf("created, but couldn't log in as the role %q to check: %v", t.role, err)
	}
	have := parseSchemas(row)
	for _, s := range initSchemas {
		if !have[s] {
			return fmt.Errorf("the schema %q is still missing after the run", s)
		}
	}
	fmt.Fprintf(out, "\nDone: the role %q, the database %q and the schemas %s are in place. `versola migrate` is next.\n", t.role, t.db, strings.Join(initSchemas, ", "))
	return nil
}

// writeBlocked says what stops the run and, when rights are what is missing,
// writes the script an admin with enough rights runs instead. No script is
// written for a database that belongs to someone else (an admin running it
// would put the schemas into that database), nor in a dry run, which only
// prints. It returns an error: the deployment is not ready.
func writeBlocked(out io.Writer, plan initPlan, t initTarget, secret string, deps PostgresInitDeps, dryRun bool) error {
	fmt.Fprintln(out, "\nNothing was changed: this connection can't do what is needed.")
	for _, r := range plan.missingRights {
		fmt.Fprintf(out, "  - missing: %s\n", r)
	}
	for _, r := range plan.blocked {
		fmt.Fprintf(out, "  - %s\n", r)
	}
	if len(plan.blocked) > 0 {
		return fmt.Errorf("not initialised")
	}
	if dryRun {
		fmt.Fprintln(out, "\nDry run: the SQL for an admin was not written.")
		return fmt.Errorf("not initialised: the rights listed above are missing")
	}
	path, err := deps.ScriptPath(t.target)
	if err == nil {
		err = fsutil.WriteFileAtomic(path, []byte(initScript(t, secret)), 0o600)
	}
	if err != nil {
		return fmt.Errorf("not initialised, and the script for an admin couldn't be written: %v", err)
	}
	fmt.Fprintf(out, "\nThe SQL that does it is in a file only you can read:\n\n  %s\n\n", path)
	fmt.Fprintf(out, "Run it as a Postgres role with those rights -- Postgres on this server:\n\n  sudo -u postgres psql < %s\n\n", shellQuote(path))
	fmt.Fprintf(out, "managed or remote Postgres: psql \"host=<host> user=<admin> dbname=postgres\" < %s\n\n", shellQuote(path))
	fmt.Fprintln(out, "It creates only what is missing, so it is safe to run again. Then run `versola postgres init` again to check, or go on with `versola migrate`.")
	return fmt.Errorf("not initialised: the rights listed above are missing")
}
