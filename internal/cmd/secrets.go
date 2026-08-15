package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/openbao"
)

// secretsCmd is a parent for subcommands, not runnable itself — cobra
// prints its own help (the subcommand list) when invoked with no
// subcommand, which is what we want here.
var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage this machine's access to OpenBao",
}

var secretsLoginCmd = &cobra.Command{
	Use:   "login <target> <address> <role-id> <secret-id>",
	Short: "Store this machine's AppRole credentials for a target's OpenBao",
	Long: `login stores the AppRole credentials whoever administers OpenBao for
<target> handed you, so later commands (configure, migrate, "secrets
test") can authenticate without asking again.

<address> is where OpenBao listens, e.g. http://localhost:8200 for
local. <role-id> and <secret-id> come from the AppRole your OpenBao
administrator created — see develop.md's OpenBao section for how
that's set up.

Credentials are stored per target in ~/.versola/openbao/<target>.json
(not under ~/.versola/active, which only ever describes the single
deployment currently configured) — logging into a different target
later doesn't discard this one's access.`,
	Args: cobra.ExactArgs(4),
	RunE: runSecretsLogin,
}

func runSecretsLogin(cmd *cobra.Command, args []string) error {
	target, address, roleID, secretID := args[0], args[1], args[2], args[3]

	creds := &openbao.Credentials{
		Address:  address,
		RoleID:   roleID,
		SecretID: secretID,
	}

	// Fail before saving anything that doesn't actually work — a stored
	// credential that turns out to be wrong is a more confusing failure
	// mode (surfaces later, inside configure/migrate) than rejecting it
	// here, at the moment it was typed in.
	fmt.Printf("Verifying credentials against %s...\n", address)
	if err := openbao.NewClient(creds).Login(context.Background()); err != nil {
		return fmt.Errorf("couldn't log in with these credentials: %w", err)
	}

	if err := openbao.SaveCredentials(target, creds); err != nil {
		return err
	}
	fmt.Printf("Stored OpenBao credentials for %q.\n", target)
	return nil
}

var secretsTestCmd = &cobra.Command{
	Use:   "test <target>",
	Short: "Verify this machine's stored OpenBao credentials for a target",
	Long: `test logs into OpenBao using the AppRole credentials stored for
<target> (see "versola secrets login"), then writes and reads back a
scratch value to confirm both the credentials and the KV v2 engine
are set up correctly.

This doesn't touch any real service's secrets — it writes to
versola/<target>/_secrets-test, a path nothing else reads.`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretsTest,
}

func runSecretsTest(cmd *cobra.Command, args []string) error {
	target := args[0]

	creds, err := openbao.LoadCredentials(target)
	if err != nil {
		if errors.Is(err, openbao.ErrNoCredentials) {
			return fmt.Errorf("no OpenBao credentials stored for %q — run `versola secrets login %s <address> <role-id> <secret-id>` first", target, target)
		}
		return err
	}

	client := openbao.NewClient(creds)
	ctx := context.Background()

	fmt.Printf("Logging into OpenBao at %s...\n", creds.Address)
	if err := client.Login(ctx); err != nil {
		return err
	}
	fmt.Println("  ok")

	path := openbao.SecretPath(target, "_secrets-test")
	want := map[string]string{"probe": fmt.Sprintf("versola-cli round-trip at %s", time.Now().UTC().Format(time.RFC3339))}

	fmt.Println("Writing a scratch value...")
	if err := client.WriteSecret(ctx, path, want); err != nil {
		return err
	}
	fmt.Println("  ok")

	fmt.Println("Reading it back...")
	got, ok, err := client.ReadSecret(ctx, path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("wrote a value but couldn't read it back")
	}
	if got["probe"] != want["probe"] {
		return fmt.Errorf("read back a different value than was written")
	}
	fmt.Println("  ok")

	fmt.Printf("\nOpenBao credentials for %q are working.\n", target)
	return nil
}

func init() {
	secretsCmd.AddCommand(secretsLoginCmd)
	secretsCmd.AddCommand(secretsTestCmd)
}
