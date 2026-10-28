package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// postgresCmd is a parent for subcommands, like secretsCmd.
var postgresCmd = &cobra.Command{
	Use:   "postgres",
	Short: "Prepare the Postgres a vps deployment uses",
}

var (
	postgresInitAdminURL          string
	postgresInitSudo              bool
	postgresInitAdminPasswordFile string
	postgresInitDryRun            bool
	postgresInitYes               bool
)

var postgresInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create the role, the database and the schemas on a Postgres of your own",
	Long: `init makes the Postgres a vps deployment was configured against (see
"versola configure --postgres-host") ready for Versola: the role, the database
and the schemas auth, central and edge that the generated configs name. It is
for a Postgres of your own; the one that runs in a container of the deployment
creates all of this by itself.

What it does depends on the rights it finds:

  - an admin account that can create roles and databases (--admin-url, or
    --sudo-postgres on this server): the role (with the password stored in
    OpenBao), the database owned by it, then, logged in as that role, the schemas;
  - no admin, but a role that already owns the database: only the schemas that
    are missing, as that role;
  - not enough rights: nothing is run. It says which rights are missing and
    writes the SQL for an admin to ~/.versola/openbao/vps-postgres-init.sql
    (readable only by you), the way "versola configure" does for the role.

It only creates what is missing: no DROP, no ALTER of a role, database or
schema that exists. The password of a role that exists is not set; a role
created here gets the deployment's POSTGRES_PASSWORD.

The SQL is printed before it runs, with the password hidden, and you are asked
to confirm (--yes skips the question, --dry-run only prints). The admin's
password is read from --admin-password-file, else from PGPASSWORD, else asked
for with no echo. It is never taken from the URL (a URL with a password is
refused) and never put on a command line: psql runs in a throwaway container
(postgres:18-alpine) on the host network, with the password in its environment.

  versola postgres init --sudo-postgres
  versola postgres init --admin-url 'postgres://admin@db.example.com:5432/postgres?sslmode=verify-full'
  versola postgres init --dry-run --sudo-postgres`,
	Args: cobra.NoArgs,
	RunE: runPostgresInit,
}

func init() {
	postgresInitCmd.Flags().StringVar(&postgresInitAdminURL, "admin-url", "", "an admin account to create the role and the database with, as postgres://ADMIN@host:5432/postgres?sslmode=... (no password in it)")
	postgresInitCmd.Flags().BoolVar(&postgresInitSudo, "sudo-postgres", false, "use the postgres user of this server (sudo -u postgres psql) as the admin")
	postgresInitCmd.Flags().StringVar(&postgresInitAdminPasswordFile, "admin-password-file", "", "a file (mode 600) whose first line is the admin's password, instead of PGPASSWORD or the prompt")
	postgresInitCmd.Flags().BoolVar(&postgresInitDryRun, "dry-run", false, "look at what exists and print the SQL that would run, change nothing")
	postgresInitCmd.Flags().BoolVarP(&postgresInitYes, "yes", "y", false, "don't ask for confirmation")
	postgresCmd.AddCommand(postgresInitCmd)
}

func runPostgresInit(cmd *cobra.Command, args []string) error {
	// Held for the whole run, as migrate holds it: a concurrent configure could
	// otherwise replace the deployment this is about to act on.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := state.Load(); err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return fmt.Errorf("nothing has been configured yet — run `versola configure vps <version> --auth-url ... --postgres-host ...` first")
		}
		return err
	}
	return deploy.PostgresInit(deploy.PostgresInitOptions{
		AdminURL:          postgresInitAdminURL,
		SudoPostgres:      postgresInitSudo,
		AdminPasswordFile: postgresInitAdminPasswordFile,
		DryRun:            postgresInitDryRun,
		Yes:               postgresInitYes,
	}, deploy.PostgresInitDeps{
		Out:        os.Stdout,
		PGPassword: os.Getenv("PGPASSWORD"),
		Prompt:     readAdminPassword,
		Confirm:    confirm,
	})
}

// readAdminPassword asks for the admin's password. Masked whenever stdin is a
// terminal; a script that pipes it in gets a plain read. An empty answer is an
// error: a cut or missing password would only fail later as an authentication
// error.
func readAdminPassword() (string, error) {
	fmt.Print("Admin password: ")
	if !stdinIsInteractive() {
		line, err := deploy.ReadLine(os.Stdin)
		if (err != nil && !errors.Is(err, io.EOF)) || strings.TrimSpace(line) == "" {
			if err == nil {
				err = errors.New("empty input")
			}
			return "", fmt.Errorf("couldn't read the admin password (give it with PGPASSWORD or --admin-password-file): %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("couldn't read the admin password: %w", err)
	}
	if len(b) == 0 {
		return "", errors.New("the admin password is empty")
	}
	return string(b), nil
}
