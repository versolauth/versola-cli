package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ConfigureResult is what Configure hands back to its callers.
type ConfigureResult struct {
	Dir string
	// PostgresRoleSetupNeeded: this configure stored a brand-new Postgres
	// password in OpenBao (first configure against a fresh OpenBao), and
	// the Postgres server's role doesn't have it yet. Configure has
	// already printed the SQL to run; migrate/up can't connect until
	// someone does.
	PostgresRoleSetupNeeded bool
}

var postgresUserRe = regexp.MustCompile(`postgres\s*\{[^{}]*?\buser\s*=\s*"([^"]*)"`)

// postgresUser reads the Postgres user out of the auth.conf versola-tools
// wrote into dir -- the role name is Versola's to choose (gen-env.scala),
// not this CLI's, so it's read rather than hardcoded. Falls back to a
// placeholder the operator can't mistake for a real name.
func postgresUser(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
	if err != nil {
		return "<postgres user from auth.conf>"
	}
	return parsePostgresUser(string(b))
}

func parsePostgresUser(conf string) string {
	if m := postgresUserRe.FindStringSubmatch(conf); m != nil && m[1] != "" {
		return m[1]
	}
	return "<postgres user from auth.conf>"
}

// postgresRoleSQL is the SQL that gives role the password versola-cli just
// stored in OpenBao: CREATE for a fresh server, ALTER for a role that
// already exists (e.g. created earlier by following deploy.md with a
// password of its own). Identifier and literal are quoted by Postgres'
// own rules (double the quote character).
func postgresRoleSQL(role, password string) (create, alter string) {
	ident := `"` + strings.ReplaceAll(role, `"`, `""`) + `"`
	lit := `'` + strings.ReplaceAll(password, `'`, `''`) + `'`
	return fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s;", ident, lit),
		fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s;", ident, lit)
}

func printPostgresRoleSetup(dir, password string) {
	create, alter := postgresRoleSQL(postgresUser(dir), password)
	fmt.Println()
	fmt.Println("Postgres: this was the first configure against this OpenBao, so a new password was")
	fmt.Println("generated for Versola's Postgres role and stored there. Set it on the Postgres server")
	fmt.Println("before `versola migrate` -- as the postgres superuser (e.g. sudo -u postgres psql):")
	fmt.Println()
	fmt.Printf("  %s   -- if the role doesn't exist yet\n", create)
	fmt.Printf("  %s   -- if it already exists\n\n", alter)
	fmt.Println("If the database and its schemas don't exist yet either, create them as deploy.md describes")
	fmt.Println(`("Database role and schemas"). Later configures reuse this password; it's also kept in`)
	fmt.Printf("%s (readable only by you).\n", filepath.Join(dir, "auth.secrets.env"))
}
