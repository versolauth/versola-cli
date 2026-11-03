package deploy

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func probeConn() pgConn {
	return pgConn{host: "db.internal", port: "5432", db: "auth", user: "versola", password: "s3cr3t-value"}
}

func TestClassifyPgProbeError(t *testing.T) {
	cases := []struct {
		name        string
		msg         string
		loginTested bool
		first       bool
		kind        PgProbeKind
		want        []string
	}{
		{"host name", `psql: error: connection to server at "db.internal" (10.0.0.5), port 5432 failed: server certificate for "pg-01.internal" (and 1 other name) does not match host name "db.internal"`, true, false, PgProbeFail,
			[]string{"issued for pg-01.internal, not db.internal", "--postgres-host", "--postgres-ca"}},
		{"unknown CA", `psql: error: connection to server at "db.internal" (10.0.0.5), port 5432 failed: SSL error: certificate verify failed`, true, false, PgProbeFail,
			[]string{"--postgres-ca <file>", "certificate verify failed"}},
		{"no tls", `psql: error: connection to server at "db.internal" (10.0.0.5), port 5432 failed: server does not support SSL, but SSL was required`, true, false, PgProbeFail,
			[]string{"does not offer TLS", "verify-full"}},
		{"bad CA file", `psql: error: could not read root certificate file "/etc/versola/postgres-ca/ca.crt"`, true, false, PgProbeFail,
			[]string{"--postgres-ca"}},
		{"login first configure", `psql: error: connection to server failed: FATAL:  password authentication failed for user "versola"`, true, true, PgProbeNotYet,
			[]string{"not yet", "role SQL", "versola migrate"}},
		{"login later", `FATAL:  password authentication failed for user "versola"`, true, false, PgProbeWarn,
			[]string{"login as versola is refused", "postgres init"}},
		{"doctor login", `FATAL:  password authentication failed for user "versola"`, false, false, PgProbeOK,
			[]string{"over TLS", "not tried here"}},
		{"doctor no password", `psql: error: connection to server failed: fe_sendauth: no password supplied`, false, false, PgProbeOK,
			[]string{"not tried here"}},
		{"database missing first", `FATAL:  database "auth" does not exist`, true, true, PgProbeNotYet,
			[]string{`"auth" does not exist yet`, "postgres init"}},
		{"database missing later", `FATAL:  database "auth" does not exist`, true, false, PgProbeWarn,
			[]string{`"auth" does not exist`}},
		{"refused", `psql: error: connection to server at "db.internal" (10.0.0.5), port 5432 failed: Connection refused`, true, false, PgProbeWarn,
			[]string{"can't be reached", "firewall"}},
		{"dns", `psql: error: could not translate host name "db.internal" to address: Name or service not known`, true, false, PgProbeWarn,
			[]string{"does not resolve", "--postgres-host"}},
		{"hba", `FATAL:  no pg_hba.conf entry for host "1.2.3.4", user "versola", database "auth", SSL encryption`, true, false, PgProbeWarn,
			[]string{"hostssl"}},
		{"docker", `docker couldn't run psql (pulling postgres:18-alpine, or starting it, failed)`, true, false, PgProbeWarn,
			[]string{"could not be checked", "not a problem of the database"}},
		{"unknown", `something odd`, true, false, PgProbeWarn,
			[]string{"could not be checked", "something odd"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyPgProbeError(c.msg, probeConn(), c.loginTested, c.first)
			if got.Kind != c.kind {
				t.Errorf("kind = %v, want %v (%s)", got.Kind, c.kind, got.Detail)
			}
			for _, w := range c.want {
				if !strings.Contains(got.Detail, w) {
					t.Errorf("detail %q lacks %q", got.Detail, w)
				}
			}
			if strings.Contains(got.Detail, "s3cr3t-value") {
				t.Errorf("detail holds the password: %q", got.Detail)
			}
		})
	}
}

func TestProbePostgresReadsTheAnswer(t *testing.T) {
	ok := func(out string) psqlRunner {
		return func(pgConn, string, map[string]string) (string, error) { return out, nil }
	}
	p := ProbePostgres(probeConn(), true, false, ok("180005|auth,central,edge\n"))
	if p.Kind != PgProbeOK || !strings.Contains(p.Detail, "PostgreSQL 18.5") || !strings.Contains(p.Detail, "schemas auth, central and edge exist") {
		t.Errorf("full answer: %+v", p)
	}
	p = ProbePostgres(probeConn(), true, false, ok("180005|auth\n"))
	if p.Kind != PgProbeOK || !strings.Contains(p.Detail, "schemas missing: central, edge") {
		t.Errorf("missing schemas: %+v", p)
	}
	p = ProbePostgres(probeConn(), true, false, ok("160011|\n"))
	if p.Kind != PgProbeWarn || !strings.Contains(p.Detail, "PostgreSQL 16.11") || !strings.Contains(p.Detail, "older versions are not verified") {
		t.Errorf("old version: %+v", p)
	}
	if p = ProbePostgres(probeConn(), true, false, ok("garbage")); p.Kind != PgProbeWarn {
		t.Errorf("garbage: %+v", p)
	}
	failing := func(pgConn, string, map[string]string) (string, error) {
		return "", errors.New(`FATAL: password authentication failed for user "versola"`)
	}
	if p = ProbePostgres(probeConn(), true, true, failing); p.Kind != PgProbeNotYet {
		t.Errorf("first configure, login refused: %+v", p)
	}
}

// The probe only reads: no statement but the one SELECT is sent.
func TestProbeSQLIsReadOnly(t *testing.T) {
	var sent []string
	run := func(c pgConn, sql string, vars map[string]string) (string, error) {
		sent = append(sent, sql)
		return "180005|", nil
	}
	ProbePostgres(probeConn(), true, false, run)
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "SELECT ") || strings.ContainsAny(strings.TrimSuffix(sent[0], ";"), ";") {
		t.Errorf("sent %q", sent)
	}
}

func TestPgProbeResult(t *testing.T) {
	if r := PgProbeResult(PgProbe{Kind: PgProbeFail, Detail: "x"}); r.OK {
		t.Errorf("a failure must fail doctor: %+v", r)
	}
	if r := PgProbeResult(PgProbe{Kind: PgProbeWarn, Detail: "x"}); !r.OK || !r.Warn {
		t.Errorf("a warning must not fail doctor: %+v", r)
	}
	if r := PgProbeResult(PgProbe{Kind: PgProbeOK, Detail: "x"}); !r.OK || r.Warn {
		t.Errorf("ok: %+v", r)
	}
}

func TestBundlePgConn(t *testing.T) {
	dir := t.TempDir()
	writeProbeFile(t, dir+"/auth.conf", "postgres {\n  url = \"jdbc:postgresql://db.internal:5433/versola?sslmode=require\"\n  user = \"versola_app\"\n}\n")
	writeProbeFile(t, dir+"/auth.secrets.env", "POSTGRES_PASSWORD=hunter2\n")
	c, err := bundlePgConn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "db.internal" || c.port != "5433" || c.db != "versola" || c.user != "versola_app" || c.sslmode != "require" || c.password != "hunter2" || c.caFile != "" {
		t.Errorf("conn = %+v", c)
	}
}

func writeProbeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyPgProbeErrorLibpqSecondLine(t *testing.T) {
	got := classifyPgProbeError("\tIs the server running on that host and accepting TCP/IP connections?", probeConn(), true, false)
	if got.Kind != PgProbeWarn || !strings.Contains(got.Detail, "can't be reached") {
		t.Errorf("%+v", got)
	}
	got = classifyPgProbeError("FATAL: no pg_hba.conf entry \x1b]0;x\x07 for host", probeConn(), true, false)
	if strings.ContainsAny(got.Detail, "\x1b\x07") {
		t.Errorf("control characters in %q", got.Detail)
	}
}

func TestPsqlWithoutPasswordIsToldNotToPrompt(t *testing.T) {
	args := pgDockerArgs(pgConn{host: "h", port: "5432", db: "d", user: "u"}, nil)
	if args[len(args)-1] != "-w" {
		t.Errorf("no -w: %v", args)
	}
	args = pgDockerArgs(pgConn{host: "h", port: "5432", db: "d", user: "u", password: "x"}, nil)
	if args[len(args)-1] == "-w" {
		t.Errorf("-w with a password: %v", args)
	}
}

func TestProbeConfiguredPostgresGate(t *testing.T) {
	dir := t.TempDir()
	writeProbeFile(t, dir+"/auth.conf", "postgres {\n  url = \"jdbc:postgresql://db.internal:5432/auth?sslmode=verify-full\"\n  user = \"versola\"\n}\n")
	writeProbeFile(t, dir+"/auth.secrets.env", "POSTGRES_PASSWORD=hunter2\n")
	var got pgConn
	run := func(c pgConn, sql string, v map[string]string) (string, error) {
		got = c
		return "", errors.New(`psql: error: connection to server failed: SSL error: certificate verify failed`)
	}
	if p := probeConfiguredPostgres(dir, true, run); p.Kind != PgProbeFail || got.password != "hunter2" || got.host != "db.internal" {
		t.Errorf("%+v %+v", p, got)
	}
	run = func(c pgConn, sql string, v map[string]string) (string, error) {
		return "", errors.New(`FATAL: password authentication failed for user "versola"`)
	}
	if p := probeConfiguredPostgres(dir, true, run); p.Kind != PgProbeNotYet {
		t.Errorf("first: %+v", p)
	}
	if p := probeConfiguredPostgres(dir, false, run); p.Kind != PgProbeWarn {
		t.Errorf("later: %+v", p)
	}
}
