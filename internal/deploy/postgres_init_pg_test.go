package deploy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/state"
)

// These run `versola postgres init` against a real Postgres, in a container of
// the postgres image (15, which has no SET option on memberships, and 18). They
// need Docker and the images, so they only run with VERSOLA_PG_IT=1:
//
//	VERSOLA_PG_IT=1 go test ./internal/deploy -run PostgresInitReal -v
//
// psql is the one in the container (docker exec) instead of a throwaway
// container on the host network, so what is tested is the SQL, the psql
// variables and the stdin script, and what a real server answers to them.

const pgItSuperPassword = "it-super-password"

// startItPostgres starts the container and returns its name and its address on
// the bridge network. The image's pg_hba.conf trusts connections from the
// loopback, so the tests connect to that address, where passwords are checked.
func startItPostgres(t *testing.T, image string) (string, string) {
	t.Helper()
	if os.Getenv("VERSOLA_PG_IT") == "" {
		t.Skip("set VERSOLA_PG_IT=1 to run against a real Postgres in Docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "versola-init-it-" + hex.EncodeToString(b)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "-e", "POSTGRES_PASSWORD="+pgItSuperPassword, image)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("couldn't start %s: %v\n%s", image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	// The image starts a server on the socket only to initialise, then restarts
	// it for real: a TCP login with the password means the real one is up.
	deadline := time.Now().Add(90 * time.Second)
	for {
		c := exec.Command("docker", "exec", "-e", "PGPASSWORD="+pgItSuperPassword, name, "psql", "-XAtq", "-h", "127.0.0.1", "-U", "postgres", "-c", "SELECT 1")
		if err := c.Run(); err == nil {
			ip, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
			if err != nil || strings.TrimSpace(string(ip)) == "" {
				t.Fatalf("no address for %s: %v", name, err)
			}
			return name, strings.TrimSpace(string(ip))
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not come up", image)
		}
		time.Sleep(time.Second)
	}
}

// itRunner is the psqlRunner of these tests: psql inside the container, over TCP
// so that passwords are really checked.
func itRunner(container, host string) psqlRunner {
	return func(c pgConn, sql string, vars map[string]string) (string, error) {
		user, pw := c.user, c.password
		if c.sudo {
			user, pw = "postgres", pgItSuperPassword
		}
		args := []string{"exec", "-i", "-e", "PGPASSWORD=" + pw, container, "psql", "-XAtq", "-v", "ON_ERROR_STOP=1", "-F", "|", "-h", host, "-U", user, "-d", c.db}
		args = append(args, psqlVarArgs(vars)...)
		args = append(args, "-f", "-")
		cmd := exec.Command("docker", args...)
		cmd.Stdin = strings.NewReader(sql)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			return "", errors.New(psqlErrorLine(errOut.String()))
		}
		return out.String(), nil
	}
}

func itFixture(t *testing.T, role, db, password string) (*state.State, string) {
	t.Helper()
	dir := t.TempDir()
	conf := fmt.Sprintf("postgres {\n  url = \"jdbc:postgresql://127.0.0.1:5432/%s?currentSchema=auth&sslmode=disable\"\n  user = \"%s\"\n  password = ${POSTGRES_PASSWORD}\n}\n", db, role)
	if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.secrets.env"), []byte("POSTGRES_PASSWORD="+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &state.State{Target: "vps", PostgresHost: "127.0.0.1:5432"}, dir
}

type itEnv struct {
	t         *testing.T
	container string
	run       psqlRunner
	scripts   string
}

func (e itEnv) deps(out *bytes.Buffer, adminPassword string) PostgresInitDeps {
	return PostgresInitDeps{
		Out:        out,
		PGPassword: adminPassword,
		Prompt:     func() (string, error) { return "", errors.New("no terminal") },
		Confirm:    func(string) bool { return true },
		Run:        e.run,
		ScriptPath: func(target string) (string, error) { return filepath.Join(e.scripts, target+"-postgres-init.sql"), nil },
	}
}

// super runs sql as the superuser, and returns what psql printed.
func (e itEnv) super(db, sql string) string {
	e.t.Helper()
	out, err := e.run(pgConn{sudo: true, db: db}, sql, nil)
	if err != nil {
		e.t.Fatalf("as the superuser: %v\n%s", err, sql)
	}
	return strings.TrimSpace(out)
}

func (e itEnv) init(role, db, password string, opts PostgresInitOptions, adminPassword string) (string, error) {
	e.t.Helper()
	st, dir := itFixture(e.t, role, db, password)
	var out bytes.Buffer
	opts.Yes = true
	err := runPostgresInit(st, dir, opts, e.deps(&out, adminPassword))
	return out.String(), err
}

// ready checks what the deployment needs: the database is the role's, the three
// schemas are the role's, and the role logs in with the password.
func (e itEnv) ready(role, db, password string) {
	e.t.Helper()
	owner := e.super("postgres", fmt.Sprintf("SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = %s;", sqlLiteral(db)))
	if owner != role {
		e.t.Errorf("the database %s belongs to %q, want %q", db, owner, role)
	}
	owners := e.super(db, "SELECT string_agg(nspname || ':' || pg_get_userbyid(nspowner), ',' ORDER BY nspname) FROM pg_namespace WHERE nspname IN ('auth', 'central', 'edge');")
	want := fmt.Sprintf("auth:%[1]s,central:%[1]s,edge:%[1]s", role)
	if owners != want {
		e.t.Errorf("schemas: %q, want %q", owners, want)
	}
	if out, err := e.run(pgConn{user: role, password: password, db: db}, "SELECT current_user;", nil); err != nil || strings.TrimSpace(out) != role {
		e.t.Errorf("the role can't log in with the deployment's password: %q, %v", out, err)
	}
}

func TestPostgresInitReal(t *testing.T) {
	for _, image := range []string{"postgres:15-alpine", "postgres:18-alpine"} {
		image := image
		t.Run(image, func(t *testing.T) {
			container, host := startItPostgres(t, image)
			e := itEnv{t: t, container: container, scripts: t.TempDir()}
			e.run = itRunner(container, host)
			// The password is really checked on this connection (the loopback is trusted by the image).
			if _, err := e.run(pgConn{user: "postgres", password: "wrong-password", db: "postgres"}, "SELECT 1;", nil); err == nil {
				t.Fatal("the test connection does not check passwords")
			}
			// A role that can create roles and databases, but is not a superuser,
			// as a managed Postgres hands out.
			e.super("postgres", "CREATE ROLE it_admin LOGIN PASSWORD 'it-admin-pw' CREATEROLE CREATEDB;")
			e.super("postgres", "CREATE ROLE it_limited LOGIN PASSWORD 'it-limited-pw';")

			t.Run("a superuser makes everything, and a second run has nothing to do", func(t *testing.T) {
				e := e
				e.t = t
				// A password that is not ASCII goes in as itself (it has no verifier here).
				out, err := e.init("it_role1", "it_db1", "pässword-1", PostgresInitOptions{AdminURL: "postgres://postgres@127.0.0.1/postgres?sslmode=disable"}, pgItSuperPassword)
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				e.ready("it_role1", "it_db1", "pässword-1")
				out, err = e.init("it_role1b", "it_db1b", "Generated-pw_1.ascii", PostgresInitOptions{AdminURL: "postgres://postgres@127.0.0.1/postgres?sslmode=disable"}, pgItSuperPassword)
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if strings.Contains(out, "Generated-pw_1.ascii") || strings.Contains(out, "SCRAM-SHA-256") {
					t.Errorf("the output holds a secret:\n%s", out)
				}
				e.ready("it_role1b", "it_db1b", "Generated-pw_1.ascii")
				out, err = e.init("it_role1b", "it_db1b", "Generated-pw_1.ascii", PostgresInitOptions{AdminURL: "postgres://postgres@127.0.0.1/postgres?sslmode=disable"}, pgItSuperPassword)
				if err != nil || !strings.Contains(out, "Nothing to do") {
					t.Errorf("second run: %v\n%s", err, out)
				}
			})

			t.Run("an admin that is not a superuser", func(t *testing.T) {
				e := e
				e.t = t
				out, err := e.init("it_role2", "it_db2", "Generated-pw_2.ascii", PostgresInitOptions{AdminURL: "postgres://it_admin@127.0.0.1/postgres?sslmode=disable"}, "it-admin-pw")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				e.ready("it_role2", "it_db2", "Generated-pw_2.ascii")
			})

			t.Run("no admin: only the schemas, as the role", func(t *testing.T) {
				e := e
				e.t = t
				// The role owns an empty database, made by hand.
				e.super("postgres", "CREATE ROLE it_role3 LOGIN PASSWORD 'Generated-pw_3.ascii';")
				e.super("postgres", "CREATE DATABASE it_db3 OWNER it_role3;")
				out, err := e.init("it_role3", "it_db3", "Generated-pw_3.ascii", PostgresInitOptions{}, "")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				e.ready("it_role3", "it_db3", "Generated-pw_3.ascii")
			})

			t.Run("a non-superuser admin continues a run cut off after the role was made", func(t *testing.T) {
				e := e
				e.t = t
				// What a first run leaves behind: the role, and the admin a member of it
				// (plain GRANT: with the SET option from PostgreSQL 16, as the run's own does).
				e.super("postgres", "CREATE ROLE it_role8 LOGIN PASSWORD 'Generated-pw_8.ascii';")
				e.super("postgres", "GRANT it_role8 TO it_admin;")
				out, err := e.init("it_role8", "it_db8", "Generated-pw_8.ascii", PostgresInitOptions{AdminURL: "postgres://it_admin@127.0.0.1/postgres?sslmode=disable"}, "it-admin-pw")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				e.ready("it_role8", "it_db8", "Generated-pw_8.ascii")

				// The same without the membership: refused, nothing is made.
				e.super("postgres", "CREATE ROLE it_role9 LOGIN PASSWORD 'Generated-pw_9.ascii';")
				out, err = e.init("it_role9", "it_db9", "Generated-pw_9.ascii", PostgresInitOptions{AdminURL: "postgres://it_admin@127.0.0.1/postgres?sslmode=disable"}, "it-admin-pw")
				if err == nil || !strings.Contains(out, "does not grant membership of a role that exists") {
					t.Fatalf("got %v\n%s", err, out)
				}
				if n := e.super("postgres", "SELECT count(*) FROM pg_database WHERE datname = 'it_db9';"); n != "0" {
					t.Errorf("the database was made without the admin being allowed to own it through the role: %s", n)
				}
			})

			t.Run("the script never grants membership of a role that existed", func(t *testing.T) {
				e := e
				e.t = t
				// The role exists, the database does not, and the admin (CREATEROLE, CREATEDB)
				// is not a member of it: the script must not make it one.
				e.super("postgres", "CREATE ROLE it_role10 LOGIN PASSWORD 'Generated-pw_10.ascii';")
				st, dir := itFixture(t, "it_role10", "it_db10", "Generated-pw_10.ascii")
				tgt, err := initTargetOf(st, dir)
				if err != nil {
					t.Fatal(err)
				}
				clause, _, err := secretClause(tgt.secret)
				if err != nil {
					t.Fatal(err)
				}
				admin := pgConn{user: "it_admin", password: "it-admin-pw", db: "postgres"}
				if _, err := e.run(admin, initScript(tgt, clause), nil); err == nil {
					t.Error("the script must fail where the admin cannot own a database through the role")
				}
				if got := e.super("postgres", "SELECT pg_has_role('it_admin', 'it_role10', 'MEMBER');"); got != "f" {
					t.Errorf("the admin became a member of a role that existed: %s", got)
				}
				if n := e.super("postgres", "SELECT count(*) FROM pg_database WHERE datname = 'it_db10';"); n != "0" {
					t.Errorf("the database was made: %s", n)
				}
			})

			t.Run("a role that exists with another password: nothing is created", func(t *testing.T) {
				e := e
				e.t = t
				e.super("postgres", "CREATE ROLE it_role4 LOGIN PASSWORD 'another-password';")
				out, err := e.init("it_role4", "it_db4", "Generated-pw_4.ascii", PostgresInitOptions{AdminURL: "postgres://postgres@127.0.0.1/postgres?sslmode=disable"}, pgItSuperPassword)
				if err == nil || !strings.Contains(err.Error(), "never sets the password of a role that exists") {
					t.Fatalf("got %v\n%s", err, out)
				}
				if n := e.super("postgres", "SELECT count(*) FROM pg_database WHERE datname = 'it_db4';"); n != "0" {
					t.Errorf("the database was made although the role can't log in: %s", n)
				}
			})

			t.Run("a database that is someone else's is left alone", func(t *testing.T) {
				e := e
				e.t = t
				e.super("postgres", "CREATE ROLE it_role5 LOGIN PASSWORD 'Generated-pw_5.ascii';")
				e.super("postgres", "CREATE DATABASE it_db5;")
				out, err := e.init("it_role5", "it_db5", "Generated-pw_5.ascii", PostgresInitOptions{AdminURL: "postgres://postgres@127.0.0.1/postgres?sslmode=disable"}, pgItSuperPassword)
				if err == nil || !strings.Contains(out, "belongs to") {
					t.Fatalf("got %v\n%s", err, out)
				}
				if n := e.super("it_db5", "SELECT count(*) FROM pg_namespace WHERE nspname IN ('auth', 'central', 'edge');"); n != "0" {
					t.Errorf("schemas were made in a database that is someone else's: %s", n)
				}
				if owner := e.super("postgres", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'it_db5';"); owner != "postgres" {
					t.Errorf("the owner changed: %s", owner)
				}
			})

			t.Run("not enough rights: the script an admin runs works, for a superuser and for another non-superuser", func(t *testing.T) {
				e := e
				e.t = t
				out, err := e.init("it_role6", "it_db6", "Generated-pw_6.ascii", PostgresInitOptions{AdminURL: "postgres://it_limited@127.0.0.1/postgres?sslmode=disable"}, "it-limited-pw")
				if err == nil || !strings.Contains(out, "CREATEROLE") {
					t.Fatalf("got %v\n%s", err, out)
				}
				script, readErr := os.ReadFile(filepath.Join(e.scripts, "vps-postgres-init.sql"))
				if readErr != nil {
					t.Fatalf("no script: %v", readErr)
				}
				if n := e.super("postgres", "SELECT count(*) FROM pg_roles WHERE rolname = 'it_role6';"); n != "0" {
					t.Errorf("the limited admin created the role: %s", n)
				}
				// A superuser runs it (twice: it is safe to run again).
				for i := 0; i < 2; i++ {
					if _, err := e.run(pgConn{sudo: true, db: "postgres"}, string(script), nil); err != nil {
						t.Fatalf("the script as a superuser, run %d: %v", i+1, err)
					}
				}
				e.ready("it_role6", "it_db6", "Generated-pw_6.ascii")

				// The same script for a fresh deployment, run by the admin that is not a superuser.
				out, err = e.init("it_role7", "it_db7", "Generated-pw_7.ascii", PostgresInitOptions{AdminURL: "postgres://it_limited@127.0.0.1/postgres?sslmode=disable"}, "it-limited-pw")
				if err == nil {
					t.Fatalf("expected the limited admin to be refused\n%s", out)
				}
				script, readErr = os.ReadFile(filepath.Join(e.scripts, "vps-postgres-init.sql"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				admin := pgConn{user: "it_admin", password: "it-admin-pw", db: "postgres"}
				if _, err := e.run(admin, string(script), nil); err != nil {
					t.Fatalf("the script as a non-superuser admin: %v", err)
				}
				e.ready("it_role7", "it_db7", "Generated-pw_7.ascii")
			})
		})
	}
}

// TestPostgresInitRealRunner runs the real runner's command line, the sh wrapper
// that hands psql its variables and the SQL on stdin, and the way its output is
// read, against a real server: the throwaway container shares the network of the
// server's container instead of the host's (which Docker Desktop does not give).
// The loopback of the image is trusted, so what this adds is the wrapper, not
// the password check.
func TestPostgresInitRealRunner(t *testing.T) {
	container, _ := startItPostgres(t, "postgres:18-alpine")
	run := func(c pgConn, sql string, vars map[string]string) (string, error) {
		return runPsqlOn(c, sql, vars, "container:"+container)
	}
	c := pgConn{host: "127.0.0.1", port: "5432", db: "postgres", user: "postgres", password: pgItSuperPassword, sslmode: "disable"}

	out, err := run(c, adminProbeSQL, map[string]string{"role": "postgres", "db": "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseAdminProbe(out)
	if err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if !p.super || !p.roleExists || !p.dbExists || p.dbOwner != "postgres" {
		t.Errorf("the probe through the real runner: %+v (%q)", p, out)
	}

	// The SQL arrives on stdin and runs; a variable with a quote in it is a literal.
	if _, err := run(c, "CREATE ROLE it_runner LOGIN;\nCREATE SCHEMA IF NOT EXISTS it_schema;", nil); err != nil {
		t.Fatal(err)
	}
	if out, err := run(c, "SELECT :'name' || '|' || count(*)::text FROM pg_roles WHERE rolname = 'it_runner';", map[string]string{"name": `a'b`}); err != nil || strings.TrimSpace(out) != `a'b|1` {
		t.Errorf("a variable and a statement through the real runner: %q, %v", out, err)
	}

	// A failure is one line of psql's own, not the statement.
	_, err = run(pgConn{host: "127.0.0.1", port: "5432", db: "no_such_db", user: "postgres", password: pgItSuperPassword, sslmode: "disable"}, "SELECT 1;", nil)
	if err == nil || !strings.Contains(err.Error(), "no_such_db") || strings.Contains(err.Error(), pgItSuperPassword) {
		t.Errorf("a failure: %v", err)
	}
	_, err = run(c, "SELECT * FROM no_such_table WHERE 'SCRAM-SHA-256$4096:x' = 'y';", nil)
	if err == nil || !strings.Contains(err.Error(), "no_such_table") || strings.Contains(err.Error(), "SCRAM") {
		t.Errorf("an error of a statement must not quote the statement: %v", err)
	}
}
