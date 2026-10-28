package deploy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestParseAdminURL(t *testing.T) {
	c, err := parseAdminURL("postgres://admin@db.example.com:6432/mydb?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "db.example.com" || c.port != "6432" || c.db != "mydb" || c.user != "admin" || c.sslmode != "require" {
		t.Errorf("got %+v", c)
	}
	c, err = parseAdminURL("postgresql://admin@db.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if c.port != "5432" || c.db != "postgres" || c.sslmode != "" {
		t.Errorf("defaults: %+v", c)
	}

	_, err = parseAdminURL("postgres://admin:hunter2@db.example.com/postgres")
	if err == nil || !strings.Contains(err.Error(), "must not carry a password") {
		t.Fatalf("a URL with a password must be refused with the reason, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the refusal repeats the password: %v", err)
	}
	for _, bad := range []string{
		"mysql://admin@db.example.com/x",
		"postgres://db.example.com/postgres",
		"postgres://@db.example.com/postgres",
		"admin@db.example.com",
		"postgres://admin@db.example.com/postgres?sslrootcert=/x",
	} {
		if _, err := parseAdminURL(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestAdminPasswordPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pw")
	if err := os.WriteFile(file, []byte("from-file\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	asked := 0
	prompt := func() (string, error) { asked++; return "from-prompt", nil }

	if got, err := adminPassword(file, "from-env", prompt); err != nil || got != "from-file" {
		t.Errorf("the file comes first: %q, %v", got, err)
	}
	if got, err := adminPassword("", "from-env", prompt); err != nil || got != "from-env" {
		t.Errorf("then PGPASSWORD: %q, %v", got, err)
	}
	if asked != 0 {
		t.Errorf("the prompt was used while a value was given")
	}
	if got, err := adminPassword("", "", prompt); err != nil || got != "from-prompt" || asked != 1 {
		t.Errorf("then the prompt: %q, %v, asked %d", got, err, asked)
	}
	failing := func() (string, error) { return "", errors.New("no terminal") }
	if _, err := adminPassword("", "", failing); err == nil {
		t.Error("no source and no terminal must fail")
	}
}

func TestReadAdminPasswordFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAdminPasswordFile(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty file: %v", err)
	}
	if _, err := readAdminPasswordFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file is an error")
	}
	if runtime.GOOS != "windows" {
		loose := filepath.Join(dir, "loose")
		if err := os.WriteFile(loose, []byte("pw\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readAdminPasswordFile(loose); err == nil || !strings.Contains(err.Error(), "readable by others") {
			t.Errorf("a file others can read must be refused: %v", err)
		}
	}
}

func TestParseProbes(t *testing.T) {
	p, err := parseAdminProbe("t|f|t|f|t|versola_app|t\n")
	if err != nil {
		t.Fatal(err)
	}
	want := adminProbe{super: true, createDB: true, dbExists: true, dbOwner: "versola_app", canSetRole: true}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}
	if _, err := parseAdminProbe("garbage"); err == nil {
		t.Error("an unexpected answer is an error")
	}
	have := parseSchemas("auth\nedge\n\n")
	if !have["auth"] || !have["edge"] || have["central"] {
		t.Errorf("schemas: %v", have)
	}
}

var testTarget = initTarget{target: "vps", role: "versola_app", db: "auth", configHost: "db.example.com:5432"}

const testSecret = `'SCRAM-SHA-256$4096:salt$stored:server'`
const testShown = `'<hidden>'`

func stepsOf(p initPlan) []string {
	var out []string
	for _, s := range p.steps {
		who := "admin"
		if s.asRole {
			who = "role"
		}
		out = append(out, who+": "+s.desc)
	}
	return out
}

func TestPlanInit(t *testing.T) {
	super := adminProbe{super: true, createRole: true, createDB: true}
	all := map[string]bool{"auth": true, "central": true, "edge": true}

	cases := []struct {
		name        string
		facts       initFacts
		wantSteps   []string
		wantRights  int
		wantBlocked bool
	}{
		{
			name:      "an admin with everything: role, database, schemas",
			facts:     initFacts{adminKnown: true, probe: super},
			wantSteps: []string{"admin: create the role", "admin: create the database, owned by the role", "role: create the schemas, as the role"},
		},
		{
			name:      "the role exists, the database does not",
			facts:     initFacts{adminKnown: true, probe: adminProbe{super: true, roleExists: true}},
			wantSteps: []string{"admin: create the database, owned by the role", "role: create the schemas, as the role"},
		},
		{
			name:      "the role owns the database, two schemas are missing",
			facts:     initFacts{adminKnown: true, probe: adminProbe{createRole: true, roleExists: true, dbExists: true, dbOwner: "versola_app"}, schemas: map[string]bool{"auth": true}},
			wantSteps: []string{"role: create the schemas, as the role"},
		},
		{
			name:      "no admin, a role that owns the database: only the schemas",
			facts:     initFacts{schemas: map[string]bool{}},
			wantSteps: []string{"role: create the schemas, as the role"},
		},
		{
			name:  "nothing is missing",
			facts: initFacts{adminKnown: true, probe: adminProbe{roleExists: true, dbExists: true, dbOwner: "versola_app"}, schemas: all},
		},
		{
			name:  "no admin and everything there",
			facts: initFacts{schemas: all},
		},
		{
			name:       "an admin that cannot create roles",
			facts:      initFacts{adminKnown: true, probe: adminProbe{createDB: true}},
			wantRights: 1,
		},
		{
			name:      "a non-superuser that is already a member of the role (a run cut off after creating it)",
			facts:     initFacts{adminKnown: true, probe: adminProbe{createDB: true, roleExists: true, canSetRole: true}},
			wantSteps: []string{"admin: create the database, owned by the role", "role: create the schemas, as the role"},
		},
		{
			name:        "a non-superuser cannot own a database through a role that exists",
			facts:       initFacts{adminKnown: true, probe: adminProbe{createDB: true, roleExists: true}},
			wantBlocked: true,
		},
		{
			name:       "an admin that can neither",
			facts:      initFacts{adminKnown: true, probe: adminProbe{}},
			wantRights: 2,
		},
		{
			name:        "the database is someone else's: never changed",
			facts:       initFacts{adminKnown: true, probe: adminProbe{super: true, roleExists: true, dbExists: true, dbOwner: "someone"}},
			wantBlocked: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planInit(c.facts, testTarget, testSecret, testShown)
			if got := stepsOf(p); strings.Join(got, ";") != strings.Join(c.wantSteps, ";") {
				t.Errorf("steps = %v, want %v", got, c.wantSteps)
			}
			if len(p.missingRights) != c.wantRights {
				t.Errorf("missing rights = %v, want %d", p.missingRights, c.wantRights)
			}
			if (len(p.blocked) > 0) != c.wantBlocked {
				t.Errorf("blocked = %v, want %v", p.blocked, c.wantBlocked)
			}
			if (c.wantRights > 0 || c.wantBlocked) && len(p.steps) > 0 {
				t.Errorf("nothing may run when it is blocked: %v", stepsOf(p))
			}
		})
	}

	// Only what is missing, as the role.
	p := planInit(initFacts{adminKnown: true, probe: adminProbe{roleExists: true, dbExists: true, dbOwner: "versola_app"}, schemas: map[string]bool{"auth": true}}, testTarget, testSecret, testShown)
	sql := p.steps[0].sql
	if strings.Contains(sql, `"auth"`) || !strings.Contains(sql, `"central"`) || !strings.Contains(sql, `"edge"`) {
		t.Errorf("only the missing schemas: %s", sql)
	}
	// A database made now has none of them.
	p = planInit(initFacts{adminKnown: true, probe: super, schemas: all}, testTarget, testSecret, testShown)
	if got := p.steps[len(p.steps)-1].sql; !strings.Contains(got, `"auth"`) || !strings.Contains(got, `"central"`) || !strings.Contains(got, `"edge"`) {
		t.Errorf("a new database gets all three: %s", got)
	}
}

func TestPlanInitSQL(t *testing.T) {
	// The role is created with the verifier; the shown text hides it.
	p := planInit(initFacts{adminKnown: true, probe: adminProbe{super: true}}, testTarget, testSecret, testShown)
	role := p.steps[0]
	if !strings.Contains(role.sql, "CREATE ROLE \"versola_app\" WITH LOGIN PASSWORD "+testSecret) {
		t.Errorf("role statement: %s", role.sql)
	}
	if strings.Contains(role.shown, "SCRAM") || !strings.Contains(role.shown, testShown) {
		t.Errorf("the shown statement must hide the verifier: %s", role.shown)
	}
	if strings.Contains(p.steps[1].sql, "GRANT") {
		t.Errorf("a superuser needs no membership: %s", p.steps[1].sql)
	}

	// A role that is not a superuser makes the database through a membership.
	p = planInit(initFacts{adminKnown: true, probe: adminProbe{createRole: true, createDB: true}}, testTarget, testSecret, testShown)
	got := p.steps[1].sql
	for _, want := range []string{
		`\gset`, `\if :versola_pg16`,
		`GRANT "versola_app" TO CURRENT_USER WITH SET TRUE;`,
		`\else`, `GRANT "versola_app" TO CURRENT_USER;`,
		`CREATE DATABASE "auth" OWNER "versola_app";`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("database statement for a non-superuser lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "GRANT") > strings.Index(got, "CREATE DATABASE") {
		t.Errorf("the membership comes before the database:\n%s", got)
	}

	// No step ever drops or alters.
	for _, s := range p.steps {
		for _, bad := range []string{"DROP ", "ALTER "} {
			if strings.Contains(strings.ToUpper(s.sql), bad) {
				t.Errorf("a step must not contain %q: %s", bad, s.sql)
			}
		}
	}

	// Identifiers are quoted by Postgres' rules.
	odd := initTarget{role: `a"b`, db: `d-b`}
	p = planInit(initFacts{adminKnown: true, probe: adminProbe{super: true}}, odd, testSecret, testShown)
	if !strings.Contains(p.steps[0].sql, `CREATE ROLE "a""b" `) || !strings.Contains(p.steps[1].sql, `CREATE DATABASE "d-b" OWNER "a""b";`) {
		t.Errorf("quoting: %s / %s", p.steps[0].sql, p.steps[1].sql)
	}
}

func TestInitScript(t *testing.T) {
	s := initScript(testTarget, testSecret)
	for _, want := range []string{
		`\set ON_ERROR_STOP on`,
		`\if :versola_create_role`,
		`CREATE ROLE "versola_app" WITH LOGIN PASSWORD ` + testSecret + `;`,
		`\if :versola_create_db`,
		`CREATE DATABASE "auth" OWNER "versola_app";`,
		`\connect "auth"`,
		`SET ROLE "versola_app";`,
		`CREATE SCHEMA IF NOT EXISTS "central";`,
		`\if :versola_super`,
		`GRANT "versola_app" TO CURRENT_USER WITH SET TRUE;`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"DROP ", "ALTER "} {
		if strings.Contains(strings.ToUpper(s), bad) {
			t.Errorf("the script must not %s: it creates only what is missing", bad)
		}
	}
}

func TestSecretClauseHidesThePassword(t *testing.T) {
	clause, shown, err := secretClause("s3cret-Value")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(clause, "'SCRAM-SHA-256$") || strings.Contains(clause, "s3cret-Value") {
		t.Errorf("an ASCII password goes in as its verifier: %s", clause)
	}
	if strings.Contains(shown, "SCRAM") || strings.Contains(shown, "s3cret-Value") {
		t.Errorf("shown: %s", shown)
	}
	// Not ASCII: the password itself (a verifier of it could differ).
	clause, _, err = secretClause("pässword")
	if err != nil || clause != "'pässword'" {
		t.Errorf("a non-ASCII password: %q, %v", clause, err)
	}
}

func TestPsqlCommandLinesCarryNoSecret(t *testing.T) {
	c := pgConn{host: "db.example.com", port: "5432", db: "postgres", user: "admin", sslmode: "verify-full", caFile: "/ca.crt", password: "hunter2"}
	args := strings.Join(pgDockerArgs(c, map[string]string{"role": "versola_app", "db": "auth"}), " ")
	if strings.Contains(args, "hunter2") {
		t.Errorf("the password is on the command line: %s", args)
	}
	for _, want := range []string{"-e VERSOLA_PG_PASSWORD ", "-e PGHOST=db.example.com", "-e PGUSER=admin", "-e PGSSLMODE=verify-full", "/ca.crt:" + pgCAContainerDir + "/ca.crt:ro", "-i ", "--network host", "-v db=auth", "-v role=versola_app"} {
		if !strings.Contains(args+" ", want) {
			t.Errorf("the command line lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "VERSOLA_PG_PASSWORD=") {
		t.Errorf("the environment variable must be passed by name, with no value: %s", args)
	}

	plain := strings.Join(pgDockerArgs(pgConn{host: "h", port: "5432", db: "d", user: "u", sslmode: "disable"}, nil), " ")
	if strings.Contains(plain, "PGSSLROOTCERT") {
		t.Errorf("a mode that does not verify gets no root certificate: %s", plain)
	}
	if def := strings.Join(pgDockerArgs(pgConn{host: "h", port: "5432", db: "d", user: "u"}, nil), " "); !strings.Contains(def, "PGSSLMODE=verify-full") || !strings.Contains(def, "PGSSLROOTCERT=system") {
		t.Errorf("the services' default is verify-full: %s", def)
	}

	sudo := strings.Join(pgSudoArgs(pgConn{sudo: true, db: "postgres"}, map[string]string{"role": "r"}), " ")
	if !strings.HasPrefix(sudo, "-u postgres psql ") || !strings.Contains(sudo, "-v role=r") || !strings.HasSuffix(sudo, "-f -") || strings.Contains(sudo, " -p ") {
		t.Errorf("sudo: %s", sudo)
	}
	// The cluster the deployment points at, not the default one.
	if got := strings.Join(pgSudoArgs(pgConn{sudo: true, db: "postgres", port: "5433"}, nil), " "); !strings.Contains(got, "-p 5433") {
		t.Errorf("sudo with a port: %s", got)
	}
	if got := strings.Join(pgDockerArgsOn(pgConn{host: "h", port: "5432", db: "d", user: "u", sslmode: "disable"}, nil, "container:pg"), " "); !strings.Contains(got, "--network container:pg") || strings.Contains(got, "--network host") {
		t.Errorf("another network: %s", got)
	}
}

// ---- the whole command, with psql replaced ----

// fakeServer answers psql runs from a small model of a Postgres, and records
// them. A statement that creates is applied, so that the check at the end sees it.
type fakeServer struct {
	probe      string // the admin probe's row
	schemas    map[string]bool
	roleLogin  error // what the role's connection answers; nil lets it in
	calls      []fakeCall
	mutating   int
	adminError error
}

type fakeCall struct {
	conn pgConn
	sql  string
}

func (f *fakeServer) run(c pgConn, sql string, vars map[string]string) (string, error) {
	f.calls = append(f.calls, fakeCall{c, sql})
	switch {
	case strings.TrimSpace(sql) == "SELECT 1;":
		if f.roleLogin != nil {
			return "", f.roleLogin
		}
		return "1\n", nil
	case strings.Contains(sql, "rolsuper"):
		if f.adminError != nil {
			return "", f.adminError
		}
		return f.probe + "\n", nil
	case strings.Contains(sql, "pg_namespace"):
		if f.roleLogin != nil {
			return "", f.roleLogin
		}
		var out []string
		for _, s := range initSchemas {
			if f.schemas[s] {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n") + "\n", nil
	}
	f.mutating++
	if strings.Contains(sql, "CREATE SCHEMA") {
		for _, s := range initSchemas {
			if strings.Contains(sql, `"`+s+`"`) {
				f.schemas[s] = true
			}
		}
	}
	return "", nil
}

func initFixture(t *testing.T) (st *state.State, dir string) {
	t.Helper()
	dir = t.TempDir()
	conf := "postgres {\n  url = \"jdbc:postgresql://127.0.0.1:5432/auth?currentSchema=auth&sslmode=require\"\n  user = \"versola_app\"\n  password = ${POSTGRES_PASSWORD}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.secrets.env"), []byte("POSTGRES_PASSWORD=stored-password-1\nOTHER=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &state.State{Target: "vps", PostgresHost: "db.example.com:5432"}, dir
}

func initDeps(f *fakeServer, out *bytes.Buffer, scriptDir string, answer bool) PostgresInitDeps {
	return PostgresInitDeps{
		Out:        out,
		PGPassword: "admin-password-1",
		Prompt:     func() (string, error) { return "", errors.New("no terminal") },
		Confirm:    func(string) bool { return answer },
		Run:        f.run,
		ScriptPath: func(target string) (string, error) { return filepath.Join(scriptDir, target+"-postgres-init.sql"), nil },
	}
}

func assertNoSecrets(t *testing.T, f *fakeServer, out string) {
	t.Helper()
	for _, secret := range []string{"stored-password-1", "admin-password-1"} {
		if strings.Contains(out, secret) {
			t.Errorf("the output holds a password:\n%s", out)
		}
		for _, c := range f.calls {
			if strings.Contains(c.sql, secret) {
				t.Errorf("a statement holds a password: %s", c.sql)
			}
		}
	}
	if strings.Contains(out, "SCRAM-SHA-256") {
		t.Errorf("the output shows a verifier:\n%s", out)
	}
}

func TestPostgresInitFresh(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "t|t|t|f|f||f", schemas: map[string]bool{}}
	var out bytes.Buffer
	err := runPostgresInit(st, dir, PostgresInitOptions{AdminURL: "postgres://admin@db.example.com:5432/postgres?sslmode=require"}, initDeps(f, &out, t.TempDir(), true))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.mutating != 2 {
		t.Errorf("one run as the admin, one as the role, got %d", f.mutating)
	}
	var admin, role fakeCall
	for _, c := range f.calls {
		if strings.Contains(c.sql, "CREATE ROLE") {
			admin = c
		}
		if strings.Contains(c.sql, "CREATE SCHEMA") {
			role = c
		}
	}
	if admin.conn.user != "admin" || admin.conn.password != "admin-password-1" || !strings.Contains(admin.sql, `CREATE DATABASE "auth" OWNER "versola_app"`) {
		t.Errorf("admin run: %+v", admin)
	}
	if role.conn.user != "versola_app" || role.conn.password != "stored-password-1" || role.conn.db != "auth" {
		t.Errorf("the schemas must be made as the role, in its database: %+v", role.conn)
	}
	if !strings.Contains(out.String(), "Done:") {
		t.Errorf("no final line:\n%s", out.String())
	}
	assertNoSecretsExceptRunSQL(t, f, out.String())
}

// assertNoSecretsExceptRunSQL: the CREATE ROLE that runs holds the verifier (not
// the password); the rest of assertNoSecrets applies.
func assertNoSecretsExceptRunSQL(t *testing.T, f *fakeServer, out string) {
	t.Helper()
	assertNoSecrets(t, &fakeServer{calls: f.calls}, out)
	for _, c := range f.calls {
		if strings.Contains(c.sql, "CREATE ROLE") && !strings.Contains(c.sql, "SCRAM-SHA-256$") {
			t.Errorf("the role must be created with a verifier: %s", c.sql)
		}
	}
}

func TestPostgresInitSudoUsesThePostgresUser(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "t|t|t|f|f||f", schemas: map[string]bool{}}
	var out bytes.Buffer
	if err := runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, Yes: true}, initDeps(f, &out, t.TempDir(), false)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !f.calls[0].conn.sudo {
		t.Errorf("the probe must go through sudo: %+v", f.calls[0].conn)
	}
	if strings.Contains(out.String(), "Continue?") {
		t.Errorf("--yes must not ask")
	}
}

func TestPostgresInitDryRunAndDecline(t *testing.T) {
	for _, c := range []struct {
		name   string
		opts   PostgresInitOptions
		answer bool
		want   string
	}{
		{"dry run", PostgresInitOptions{SudoPostgres: true, DryRun: true}, true, "Dry run: nothing was changed."},
		{"declined", PostgresInitOptions{SudoPostgres: true}, false, "Aborted. Nothing was changed."},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, dir := initFixture(t)
			f := &fakeServer{probe: "t|t|t|f|f||f", schemas: map[string]bool{}}
			var out bytes.Buffer
			err := runPostgresInit(st, dir, c.opts, initDeps(f, &out, t.TempDir(), c.answer))
			// A declined question fails the command: `init && migrate` must stop.
			if (err != nil) != (c.name == "declined") {
				t.Fatalf("err = %v", err)
			}
			if f.mutating != 0 {
				t.Errorf("nothing may change, %d statements ran", f.mutating)
			}
			if !strings.Contains(out.String(), c.want) || !strings.Contains(out.String(), "CREATE ROLE") {
				t.Errorf("the SQL is printed first, then %q:\n%s", c.want, out.String())
			}
			assertNoSecrets(t, f, out.String())
		})
	}
}

func TestPostgresInitBlockedWritesTheScript(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "f|f|t|f|f||f", schemas: map[string]bool{}}
	scripts := t.TempDir()
	var out bytes.Buffer
	err := runPostgresInit(st, dir, PostgresInitOptions{AdminURL: "postgres://limited@db.example.com/postgres", Yes: true}, initDeps(f, &out, scripts, true))
	if err == nil || !strings.Contains(err.Error(), "not initialised") {
		t.Fatalf("a missing right must fail the command: %v", err)
	}
	if f.mutating != 0 {
		t.Errorf("nothing may run, %d statements did", f.mutating)
	}
	if !strings.Contains(out.String(), "CREATEROLE") {
		t.Errorf("the missing right must be named:\n%s", out.String())
	}
	path := filepath.Join(scripts, "vps-postgres-init.sql")
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the script was not written: %v", readErr)
	}
	if !strings.Contains(string(b), "CREATE ROLE") || strings.Contains(string(b), "stored-password-1") {
		t.Errorf("the script must create the role with a verifier, never the password:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info == nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the script must be 0600")
		}
	}
	assertNoSecrets(t, &fakeServer{calls: f.calls}, out.String())
}

func TestPostgresInitDatabaseOfSomeoneElse(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "t|t|t|t|t|someone|f", schemas: map[string]bool{}}
	var out bytes.Buffer
	scripts := t.TempDir()
	err := runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, Yes: true}, initDeps(f, &out, scripts, true))
	if err == nil {
		t.Fatal("a database that is someone else's must stop the run")
	}
	if f.mutating != 0 || !strings.Contains(out.String(), `belongs to "someone"`) {
		t.Errorf("nothing may run, and it says why (%d):\n%s", f.mutating, out.String())
	}
	// An admin running the script would put the schemas into that database.
	if _, statErr := os.Stat(filepath.Join(scripts, "vps-postgres-init.sql")); !os.IsNotExist(statErr) {
		t.Errorf("no script may be written for a database that is someone else's: %v", statErr)
	}
}

func TestPostgresInitWithoutAdmin(t *testing.T) {
	// The role owns the database: only the schemas missing.
	st, dir := initFixture(t)
	f := &fakeServer{schemas: map[string]bool{"auth": true}}
	var out bytes.Buffer
	if err := runPostgresInit(st, dir, PostgresInitOptions{Yes: true}, initDeps(f, &out, t.TempDir(), true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.mutating != 1 || strings.Contains(f.calls[1].sql, `"auth"`) {
		t.Errorf("only central and edge, as the role: %+v", f.calls)
	}
	for _, c := range f.calls {
		if c.conn.user != "versola_app" {
			t.Errorf("without an admin every run is the role's: %+v", c.conn)
		}
	}

	// Everything there: nothing to do.
	st, dir = initFixture(t)
	f = &fakeServer{schemas: map[string]bool{"auth": true, "central": true, "edge": true}}
	out.Reset()
	if err := runPostgresInit(st, dir, PostgresInitOptions{Yes: true}, initDeps(f, &out, t.TempDir(), true)); err != nil || f.mutating != 0 || !strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("err=%v mutating=%d:\n%s", err, f.mutating, out.String())
	}

	// The role cannot log in and no admin was given: it says what to give.
	st, dir = initFixture(t)
	f = &fakeServer{roleLogin: errors.New(`FATAL:  password authentication failed for user "versola_app"`)}
	out.Reset()
	err := runPostgresInit(st, dir, PostgresInitOptions{Yes: true}, initDeps(f, &out, t.TempDir(), true))
	if err == nil || !strings.Contains(err.Error(), "--sudo-postgres") || !strings.Contains(err.Error(), "--admin-url") {
		t.Errorf("it must point at the admin flags: %v", err)
	}
}

func TestPostgresInitNeverSetsThePasswordOfAnExistingRole(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "t|t|t|t|t|versola_app|f", roleLogin: errors.New(`FATAL:  password authentication failed for user "versola_app"`)}
	var out bytes.Buffer
	err := runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, Yes: true}, initDeps(f, &out, t.TempDir(), true))
	if err == nil || !strings.Contains(err.Error(), "never sets the password of a role that exists") {
		t.Fatalf("got %v", err)
	}
	if f.mutating != 0 {
		t.Errorf("nothing may run, %d statements did", f.mutating)
	}
}

func TestPostgresInitRefusals(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{}
	var out bytes.Buffer
	deps := initDeps(f, &out, t.TempDir(), true)

	if err := runPostgresInit(st, dir, PostgresInitOptions{AdminURL: "postgres://a@h/postgres", SudoPostgres: true}, deps); err == nil {
		t.Error("both admin flags must be refused")
	}
	if err := runPostgresInit(st, dir, PostgresInitOptions{AdminPasswordFile: "x"}, deps); err == nil {
		t.Error("a password file without --admin-url must be refused")
	}
	bundled := *st
	bundled.PostgresMode = PostgresModeBundled
	if err := runPostgresInit(&bundled, dir, PostgresInitOptions{SudoPostgres: true}, deps); err == nil || !strings.Contains(err.Error(), "nothing to initialise") {
		t.Errorf("a deployment with its own Postgres: %v", err)
	}
	local := *st
	local.Target = "local"
	if err := runPostgresInit(&local, dir, PostgresInitOptions{SudoPostgres: true}, deps); err == nil || !strings.Contains(err.Error(), "vps") {
		t.Errorf("local: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a refusal must not reach Postgres: %d calls", len(f.calls))
	}
}

func TestPostgresInitChecksTheRolesLoginBeforeCreatingItsDatabase(t *testing.T) {
	st, dir := initFixture(t)
	// The role exists, the database does not, and the role cannot log in.
	f := &fakeServer{probe: "t|t|t|t|f||f", roleLogin: errors.New(`FATAL:  password authentication failed for user "versola_app"`)}
	var out bytes.Buffer
	err := runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, Yes: true}, initDeps(f, &out, t.TempDir(), true))
	if err == nil || !strings.Contains(err.Error(), "never sets the password of a role that exists") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("got %v", err)
	}
	if f.mutating != 0 {
		t.Errorf("the database must not be made before the login is known to work: %d statements ran", f.mutating)
	}
	for _, c := range f.calls {
		if strings.TrimSpace(c.sql) == "SELECT 1;" && (c.conn.db != "postgres" || c.conn.user != "versola_app") {
			t.Errorf("the check is a login as the role to the postgres database: %+v", c.conn)
		}
	}

	// The same, with a role that can log in: the database is made.
	st, dir = initFixture(t)
	f = &fakeServer{probe: "t|t|t|t|f||f", schemas: map[string]bool{}}
	out.Reset()
	if err := runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, Yes: true}, initDeps(f, &out, t.TempDir(), true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.mutating != 2 {
		t.Errorf("the database and the schemas: %d", f.mutating)
	}
}

func TestPostgresInitSudoNeedsAPostgresOnThisServer(t *testing.T) {
	st, dir := initFixture(t)
	conf, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
	if err != nil {
		t.Fatal(err)
	}
	remote := strings.ReplaceAll(string(conf), "127.0.0.1", "db.example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(remote), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{probe: "t|t|t|f|f||f", schemas: map[string]bool{}}
	var out bytes.Buffer
	err = runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, Yes: true}, initDeps(f, &out, t.TempDir(), true))
	if err == nil || !strings.Contains(err.Error(), "not this server") || !strings.Contains(err.Error(), "--admin-url") {
		t.Fatalf("a remote Postgres must be refused for --sudo-postgres, saying what to use: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing may reach a server: %d calls", len(f.calls))
	}
	// --admin-url is for exactly that.
	f = &fakeServer{probe: "t|t|t|f|f||f", schemas: map[string]bool{}}
	out.Reset()
	if err := runPostgresInit(st, dir, PostgresInitOptions{AdminURL: "postgres://admin@db.example.invalid/postgres", Yes: true}, initDeps(f, &out, t.TempDir(), true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestPostgresInitDryRunWritesNoScript(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "f|f|t|f|f||f", schemas: map[string]bool{}}
	scripts := t.TempDir()
	var out bytes.Buffer
	err := runPostgresInit(st, dir, PostgresInitOptions{AdminURL: "postgres://limited@db.example.com/postgres", DryRun: true}, initDeps(f, &out, scripts, true))
	if err == nil {
		t.Fatal("a missing right still fails the command")
	}
	if entries, _ := os.ReadDir(scripts); len(entries) != 0 {
		t.Errorf("a dry run only prints: %v", entries)
	}
	if !strings.Contains(out.String(), "was not written") {
		t.Errorf("it says so:\n%s", out.String())
	}
}

func TestPsqlErrorLine(t *testing.T) {
	stmt := `psql: error: connection to server at "10.0.0.5", port 5432 failed: FATAL:  password authentication failed for user "versola_app"`
	if got := psqlErrorLine(stmt + "\n"); got != stmt {
		t.Errorf("got %q", got)
	}
	// A statement's own error: the ERROR line, not the quoted statement or the caret under it.
	multi := "ERROR:  syntax error at or near \"x\"\nLINE 1: CREATE ROLE a PASSWORD 'SCRAM-SHA-256$4096:salt$k:s' x\n                                                          ^\n"
	got := psqlErrorLine(multi)
	if !strings.HasPrefix(got, "ERROR:") || strings.Contains(got, "SCRAM") {
		t.Errorf("got %q", got)
	}
	if got := psqlErrorLine("sudo: a password is required\n"); got != "sudo: a password is required" {
		t.Errorf("sudo: %q", got)
	}
	if got := psqlErrorLine(""); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestHostIsLocal(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "127.0.0.1", "127.1.2.3", "::1", "[::1]", "/var/run/postgresql"} {
		if !hostIsLocal(h) {
			t.Errorf("%q is this machine", h)
		}
	}
	if name, err := os.Hostname(); err == nil && !hostIsLocal(name) {
		t.Errorf("the machine's own name %q is this machine", name)
	}
	for _, h := range []string{"", "db.example.invalid", "10.255.255.254"} {
		if hostIsLocal(h) {
			t.Errorf("%q is not this machine", h)
		}
	}
}

func TestPostgresInitAdminTLSModeDoesNotCarryOverToAnotherServer(t *testing.T) {
	// The deployment connects to its own Postgres with TLS switched off.
	for _, c := range []struct {
		name, deployHost, adminURL, want string
	}{
		{"another server: verify-full, not the deployment's disable", "127.0.0.1", "postgres://admin@db.other.invalid/postgres", ""},
		{"the same server: the deployment's mode", "127.0.0.1", "postgres://admin@127.0.0.1/postgres", "disable"},
		{"the loopback by another name", "127.0.0.1", "postgres://admin@localhost/postgres", "disable"},
		{"explicit wins", "127.0.0.1", "postgres://admin@db.other.invalid/postgres?sslmode=require", "require"},
		{"the very host the deployment uses, across the network", "db.same.invalid", "postgres://admin@db.same.invalid/postgres", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, dir := initFixture(t)
			conf, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
			if err != nil {
				t.Fatal(err)
			}
			conf = []byte(strings.ReplaceAll(string(conf), "127.0.0.1", c.deployHost))
			if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(strings.ReplaceAll(string(conf), "sslmode=require", "sslmode=disable")), 0o600); err != nil {
				t.Fatal(err)
			}
			f := &fakeServer{probe: "t|t|t|t|t|versola_app|f", schemas: map[string]bool{"auth": true, "central": true, "edge": true}}
			var out bytes.Buffer
			if err := runPostgresInit(st, dir, PostgresInitOptions{AdminURL: c.adminURL, DryRun: true}, initDeps(f, &out, t.TempDir(), true)); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if got := f.calls[0].conn.sslmode; got != c.want {
				t.Errorf("the admin's sslmode = %q, want %q", got, c.want)
			}
		})
	}

}

func TestInitTargetRefusesOddNames(t *testing.T) {
	for _, c := range []struct{ user, db string }{
		{`a"b`, "auth"},
		{"versola_app", "x y"},
		{"", "auth"},
		{"versola_app", strings.Repeat("d", 64)},
	} {
		st, dir := initFixture(t)
		conf := "postgres {\n  url = \"jdbc:postgresql://127.0.0.1:5432/" + strings.ReplaceAll(c.db, "\n", "") + "?sslmode=disable\"\n  user = \"" + strings.ReplaceAll(c.user, "\"", "\\\"") + "\"\n  password = ${POSTGRES_PASSWORD}\n}\n"
		if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := initTargetOf(st, dir); err == nil {
			t.Errorf("user %q, database %q must be refused", c.user, c.db)
		}
	}
	st, dir := initFixture(t)
	if _, err := initTargetOf(st, dir); err != nil {
		t.Errorf("the fixture's plain names are fine: %v", err)
	}
}

func TestPostgresInitSudoActsOnTheDeploymentsPort(t *testing.T) {
	st, dir := initFixture(t)
	f := &fakeServer{probe: "t|t|t|t|t|versola_app|f", schemas: map[string]bool{"auth": true, "central": true, "edge": true}}
	var out bytes.Buffer
	if err := runPostgresInit(st, dir, PostgresInitOptions{SudoPostgres: true, DryRun: true}, initDeps(f, &out, t.TempDir(), true)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := f.calls[0].conn; !got.sudo || got.port != "5432" {
		t.Errorf("the admin's connection: %+v", got)
	}
}

func TestInitScriptNeverGrantsAMembershipOfARoleThatExisted(t *testing.T) {
	s := initScript(testTarget, testSecret)
	grant := strings.Index(s, `GRANT "versola_app" TO CURRENT_USER`)
	if grant < 0 {
		t.Fatalf("no GRANT in the script:\n%s", s)
	}
	// The GRANT sits inside \if :versola_create_role (the role made by this very script).
	before := s[:grant]
	if strings.LastIndex(before, `\if :versola_create_role`) < strings.LastIndex(before, `\endif`) {
		t.Errorf("the GRANT is not inside \\if :versola_create_role:\n%s", s)
	}
	if strings.Count(s, `\if`) != strings.Count(s, `\endif`) {
		t.Errorf("unbalanced \\if/\\endif:\n%s", s)
	}
}
