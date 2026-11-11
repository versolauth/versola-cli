package deploy

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The restore script against a real Postgres, in Docker: what the unit tests
// can only match as text. Runs only with VERSOLA_PG_IT=1 (it starts a
// postgres:18-alpine container and removes it): a good dump over a database
// that has since changed, a truncated dump, and garbage must leave the
// database as the script promises.
func TestPgRestoreScriptAgainstRealPostgres(t *testing.T) {
	if os.Getenv("VERSOLA_PG_IT") != "1" {
		t.Skip("set VERSOLA_PG_IT=1 to run against a real Postgres in Docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	name := fmt.Sprintf("versola-restore-it-%d", os.Getpid())
	run := func(stdin []byte, args ...string) ([]byte, string, error) {
		c := exec.Command("docker", args...)
		if stdin != nil {
			c.Stdin = bytes.NewReader(stdin)
		}
		var out, errb bytes.Buffer
		c.Stdout, c.Stderr = &out, &errb
		err := c.Run()
		return out.Bytes(), errb.String(), err
	}
	if _, stderr, err := run(nil, "run", "-d", "--name", name, "-e", "POSTGRES_USER=versola_app", "-e", "POSTGRES_PASSWORD=it-only-"+fmt.Sprint(time.Now().UnixNano()), "-e", "POSTGRES_DB=auth", pgCheckImage); err != nil {
		t.Fatalf("couldn't start Postgres: %v %s", err, stderr)
	}
	defer run(nil, "rm", "-f", name)
	ready := false
	for i := 0; i < 60 && !ready; i++ {
		// The image restarts once after its init: a ready server answers twice.
		_, _, e1 := run(nil, "exec", name, "pg_isready", "-U", "versola_app", "-d", "auth")
		time.Sleep(500 * time.Millisecond)
		_, _, e2 := run(nil, "exec", name, "pg_isready", "-U", "versola_app", "-d", "auth")
		ready = e1 == nil && e2 == nil
	}
	if !ready {
		t.Fatal("Postgres did not become ready")
	}
	sql := func(q string) string {
		t.Helper()
		out, stderr, err := run(nil, "exec", name, "psql", "-U", "versola_app", "-d", "auth", "-Atq", "-c", q)
		if err != nil {
			t.Fatalf("%s: %v %s", q, err, stderr)
		}
		return strings.TrimSpace(string(out))
	}
	restore := func(dump []byte) ([]byte, string, error) {
		return run(dump, pgRestoreArgs(name, pgTarget{db: "auth", user: "versola_app"})...)
	}

	sql("create schema auth; create table auth.users(id int primary key, n text); insert into auth.users select g, 'u'||g from generate_series(1,50) g; create table t(i int); insert into t select generate_series(1,100);")
	dump, stderr, err := run(nil, "exec", name, "pg_dump", "-Fc", "-U", "versola_app", "auth")
	if err != nil || len(dump) < 100 {
		t.Fatalf("pg_dump: %v %s", err, stderr)
	}
	// What a newer version leaves behind, and damage.
	sql("delete from auth.users where id > 5; create table extra(x int); create schema newer; create table newer.x(i int);")

	// A truncated dump and garbage: nothing is committed, the database is as it was.
	for what, bad := range map[string][]byte{"truncated": dump[:len(dump)/2], "garbage": []byte("this is not a pg dump")} {
		if _, stderr, err := restore(bad); err == nil {
			t.Errorf("%s: the restore succeeded", what)
		} else if what == "garbage" && !strings.Contains(stderr, "error") {
			t.Errorf("%s: no error in %q", what, stderr)
		}
		if sql("select count(*) from auth.users") != "5" || sql("select count(*) from pg_namespace where nspname='newer'") != "1" {
			t.Fatalf("%s: a failed restore changed the database", what)
		}
	}

	// A good dump: everything the dump has is back, and what it lacks is gone.
	if _, stderr, err := restore(dump); err != nil {
		t.Fatalf("restore: %v %s", err, stderr)
	}
	if sql("select count(*) from auth.users") != "50" || sql("select count(*) from t") != "100" {
		t.Errorf("the data is not back")
	}
	if sql("select count(*) from information_schema.tables where table_name='extra'") != "0" || sql("select count(*) from pg_namespace where nspname='newer'") != "0" {
		t.Errorf("what a newer version made survived the restore")
	}
}
