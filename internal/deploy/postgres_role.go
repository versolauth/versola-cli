package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/versolauth/versola-cli/internal/fsutil"
	"github.com/versolauth/versola-cli/internal/openbao"
)

// ConfigureResult is what Configure hands back to its callers.
type ConfigureResult struct {
	Dir string
	// PostgresRoleSetupNeeded: this configure stored a brand-new Postgres
	// password in OpenBao (first configure against a fresh OpenBao), and
	// the Postgres server's role doesn't have it yet. Configure has
	// already written the SQL that sets it (announcePostgresRole) and said
	// how to run it; migrate/up can't connect until someone does.
	PostgresRoleSetupNeeded bool
	// PostgresRoleScript is where that SQL is ("" if none was written).
	PostgresRoleScript string
}

// unknownPostgresUser is what parsePostgresUser returns when auth.conf names
// no Postgres user.
const unknownPostgresUser = "<postgres user from auth.conf>"

var postgresUserRe = regexp.MustCompile(`postgres\s*\{[^{}]*?\buser\s*=\s*"([^"]*)"`)

// postgresUser reads the Postgres user out of the auth.conf versola-tools
// wrote into dir -- the role name is Versola's to choose (gen-env.scala),
// not this CLI's, so it's read rather than hardcoded. Falls back to a
// placeholder the operator can't mistake for a real name.
func postgresUser(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
	if err != nil {
		return unknownPostgresUser
	}
	return parsePostgresUser(string(b))
}

func parsePostgresUser(conf string) string {
	if m := postgresUserRe.FindStringSubmatch(conf); m != nil && m[1] != "" {
		return m[1]
	}
	return unknownPostgresUser
}

// postgresRoleSQL is the SQL that gives role the password versola-cli just
// stored in OpenBao: CREATE for a fresh server, ALTER for a role that
// already exists (e.g. created earlier by following deploy.md with a
// password of its own). Identifier and literal are quoted by Postgres'
// own rules (double the quote character).
func postgresRoleSQL(role, password string) (create, alter string) {
	ident := `"` + strings.ReplaceAll(role, `"`, `""`) + `"`
	lit := sqlLiteral(password)
	return fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s;", ident, lit),
		fmt.Sprintf("ALTER ROLE %s WITH LOGIN PASSWORD %s;", ident, lit)
}

func sqlLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// postgresRoleScript is a psql script that sets role's password whether or
// not the role exists yet: it asks pg_roles and picks CREATE or ALTER with
// psql's own \if, so the operator runs one file and never has to choose.
// secret is what goes into PASSWORD '...': the password's SCRAM verifier
// (scramVerifier), or the password itself when it has none. psql does not
// expand its :variables inside quoted literals, and standard_conforming_strings
// is switched on first so a backslash in a literal stays a backslash even on
// a server configured otherwise (psql's own lexer follows the setting too).
func postgresRoleScript(role, secret string, verifier bool) string {
	create, alter := postgresRoleSQL(role, secret)
	var b strings.Builder
	b.WriteString("-- Written by `versola configure`: gives Versola's Postgres role the password\n")
	b.WriteString("-- stored in OpenBao. It creates the role if it doesn't exist and sets the\n")
	b.WriteString("-- password (and allows login) if it does. Run it as the Postgres superuser, e.g.\n")
	b.WriteString("--   sudo -u postgres psql < <this file>\n")
	if verifier {
		b.WriteString("-- It holds the password's SCRAM-SHA-256 verifier, not the password; still keep it private.\n")
	} else {
		b.WriteString("-- It holds the password itself: keep it private, don't paste it anywhere.\n")
	}
	b.WriteString("\\set ON_ERROR_STOP on\n")
	b.WriteString("SET standard_conforming_strings = on;\n")
	fmt.Fprintf(&b, "SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = %s) AS versola_create_role \\gset\n", sqlLiteral(role))
	b.WriteString("\\if :versola_create_role\n")
	b.WriteString(create + "\n")
	b.WriteString("\\else\n")
	b.WriteString(alter + "\n")
	b.WriteString("\\endif\n")
	return b.String()
}

// announcePostgresRole writes the SQL that gives Versola's Postgres role
// (named in dir's auth.conf) the new password into
// ~/.versola/openbao/<target>-postgres-role.sql (0600, see
// openbao.PostgresRoleScriptPath), tells out where it is and how to run it,
// and returns its path ("" when none was written).
//
// The password itself is never written to out or put in an error: a terminal
// ends up in scrollback, SSH session logs, CI logs and chats. It is not in
// the file either, as long as it is ASCII (generated ones are): the file
// carries its SCRAM verifier, so a server log of the statement (a failed one
// is logged in full by default) holds no password.
//
// Not in the bundle directory dir: a later configure removes it, and a second
// configure before the SQL was run does not announce the password again (it
// is stored by then), so the file would be gone with nothing to replace it.
// A newer password (a new OpenBao) replaces the file; the rename never
// follows a link put in its place.
//
// The file is fed to psql through the shell (`psql < file`), not `psql -f`:
// psql runs as the postgres user, who can't open a file only this user can
// read, but the redirect is opened by the operator's own shell.
//
// An error only when the file couldn't be written; the password is stored in
// OpenBao by then, so the error says where it is.
func announcePostgresRole(out io.Writer, dir, target, password string) (string, error) {
	secretsEnv := filepath.Join(dir, "auth.secrets.env")
	byHand := fmt.Sprintf("Set the role's password by hand to the POSTGRES_PASSWORD in %s (also in OpenBao, secret/versola/%s/auth) before `versola migrate`", secretsEnv, target)
	role := postgresUser(dir)
	path, pathErr := openbao.PostgresRoleScriptPath(target)

	// Whatever happens next, a script left from an earlier OpenBao sets an
	// older password: remove it up front, so no failure path below leaves it
	// there to be run.
	staleNote := ""
	if pathErr == nil {
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			staleNote = fmt.Sprintf(" The older file at %s couldn't be removed (%v): do NOT run it, it sets an older password.", path, rerr)
		}
	}

	if role == unknownPostgresUser {
		// Not an error: the password is stored already, and stopping here
		// would not make the next configure say anything more.
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Postgres: a new password was generated for Versola's Postgres role and stored in\n")
		fmt.Fprintf(out, "OpenBao, but %s names no Postgres user, so no SQL was written for it.\n", filepath.Join(dir, "auth.conf"))
		fmt.Fprintf(out, "%s.%s\n", byHand, staleNote)
		return "", nil
	}

	secret, verifier, err := scramVerifier(password)
	if err != nil {
		return "", fmt.Errorf("a new password was stored in OpenBao for Versola's Postgres role %q, but the SQL that sets it couldn't be prepared: %v.%s %s", role, err, staleNote, byHand)
	}
	if !verifier {
		secret = password
	}
	if pathErr != nil {
		return "", fmt.Errorf("a new password was stored in OpenBao for Versola's Postgres role %q, but there is nowhere to write the SQL that sets it: %v. %s", role, pathErr, byHand)
	}
	if err := fsutil.WriteFileAtomic(path, []byte(postgresRoleScript(role, secret, verifier)), 0o600); err != nil {
		return "", fmt.Errorf("a new password was stored in OpenBao for Versola's Postgres role %q, but the SQL that sets it couldn't be written to %s: %v.%s %s", role, path, err, staleNote, byHand)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Postgres: this was the first configure against this OpenBao, so a new password was\n")
	fmt.Fprintf(out, "generated for Versola's Postgres role %q and stored there. Set it on the Postgres\n", role)
	fmt.Fprintf(out, "server before `versola migrate`. The SQL that does it (creates the role if it doesn't\n")
	fmt.Fprintf(out, "exist, sets the password and allows login if it does) is in a file only you can read:\n\n")
	fmt.Fprintf(out, "  %s\n\n", path)
	fmt.Fprintf(out, "Run it as the Postgres superuser -- Postgres on this server:\n\n")
	fmt.Fprintf(out, "  %s\n\n", postgresRoleCommand(path))
	fmt.Fprintf(out, "managed or remote Postgres: psql \"host=<host> user=<admin> dbname=postgres\" < %s\n\n", shellQuote(path))
	fmt.Fprintf(out, "Use `<`, not `psql -f`: the postgres user can't open your file, your shell can. The\n")
	fmt.Fprintf(out, "password is not printed; you don't need to know it (it is also kept in %s).\n", secretsEnv)
	fmt.Fprintf(out, "If the database and its schemas don't exist yet either, create them as deploy.md describes\n")
	fmt.Fprintf(out, "(\"Database role and schemas\"). Later configures reuse this password.\n")
	return path, nil
}

// postgresRoleCommand is the command that runs the role script at path
// against a Postgres on this server.
func postgresRoleCommand(path string) string {
	return "sudo -u postgres psql < " + shellQuote(path)
}

// shellQuote quotes s for a POSIX shell when it needs it: single quotes, in
// which nothing is special; a single quote inside is closed, escaped and
// reopened (see the test for an example).
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'$`\\&;|<>()*?[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
