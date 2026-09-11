package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/versolauth/versola-cli/internal/openbao"
)

// localApproleName is both the AppRole role name and the ACL policy name
// this provisions for "local" -- develop.md's manual steps use
// "versola-<target>" for both, so this keeps the auto-provisioned local
// role indistinguishable from one a human set up by hand following those
// same steps.
const localApproleName = "versola-local"

// ProvisionLocal does, over OpenBao's HTTP API, everything develop.md's
// "One-time setup" section otherwise asks a human to run by hand with the
// `bao` CLI (init, unseal, enable kv-v2, enable approle, write a policy,
// create a role, mint credentials) -- for "local" only.
//
// Georgy (reviewing this repeatedly confusing a teammate first running
// `versola bootstrap local`): a fresh machine's `versola configure local`
// used to fail with "no OpenBao credentials stored for 'local'" and point
// at seven manual `docker exec ... bao ...` commands before it would ever
// start -- fine for vps, where a human deliberately performing a one-time
// admin action against a real production secret store is the right
// design (develop.md's own words: "not something configure should ever do
// silently"), but exactly backwards for local: local's OpenBao is a
// throwaway container this same CLI already creates and destroys freely,
// and "try Versola locally" (see installation.md) shouldn't need a crash
// course in AppRole administration first.
//
// Idempotent and safe to call on every `configure local`, not just the
// first: each step below only does real work the first time (see
// openbao.EnableKV/EnableApprole's own idempotency, and LoadCredentials'
// early return once a previous run already finished). What it does NOT
// do is silently take over an OpenBao a human already set up by hand
// under this same target before this function existed -- if credentials
// are already stored, this returns immediately without touching anything
// (see the LoadCredentials check below), and if OpenBao is already
// initialized but sealed with no admin key this machine ever saved (that
// same pre-existing case), it fails with a clear message rather than
// guessing.
func ProvisionLocal(address string) error {
	ctx := context.Background()

	existingCreds, err := openbao.LoadCredentials("local")
	haveCreds := err == nil
	if err != nil && !errors.Is(err, openbao.ErrNoCredentials) {
		return fmt.Errorf("couldn't check for existing OpenBao credentials: %w", err)
	}

	admin, err := openbao.LoadLocalAdmin()
	haveAdmin := err == nil
	if err != nil && !errors.Is(err, openbao.ErrNoLocalAdmin) {
		return fmt.Errorf("couldn't check for saved OpenBao admin credentials: %w", err)
	}

	initialized, sealed, err := openbao.Health(ctx, address)
	if err != nil {
		return fmt.Errorf("couldn't check OpenBao's health: %w", err)
	}

	if !initialized {
		fmt.Println("Initializing local OpenBao...")
		rootToken, unsealKey, err := openbao.Init(ctx, address)
		if err != nil {
			return fmt.Errorf("couldn't initialize OpenBao: %w", err)
		}
		admin = &openbao.LocalAdmin{RootToken: rootToken, UnsealKey: unsealKey}
		if err := openbao.SaveLocalAdmin(admin); err != nil {
			// Saved nowhere, but the run can still finish this once --
			// admin is still in hand for the rest of this call. Surfaced
			// as a loud warning, not a silent one: without this file, the
			// NEXT `configure local` (after OpenBao's container restarts
			// and reseals -- see openbao.Unseal's own comment) has no way
			// to unseal it again and will fail with the same message
			// ProvisionLocal itself gives below for that case.
			fmt.Printf("(couldn't save OpenBao admin credentials -- %v -- the next `configure local` after this OpenBao container restarts may need this fixed by hand first)\n", err)
		}
		haveAdmin = true
		sealed = true // a freshly initialized OpenBao always starts sealed
	}

	if sealed {
		if !haveAdmin {
			return fmt.Errorf(`local OpenBao is initialized and sealed, but this machine has no saved unseal key -- this only happens if it was set up by hand before this automation existed (see develop.md's OpenBao section for the manual "bao operator unseal" step), or ~/.versola/openbao/local-admin.json was deleted. Unseal it by hand once and re-run`)
		}
		fmt.Println("Unsealing local OpenBao...")
		if err := openbao.Unseal(ctx, address, admin.UnsealKey); err != nil {
			return fmt.Errorf("couldn't unseal OpenBao: %w", err)
		}
	}

	if haveCreds {
		// Already fully set up by a previous run (or by hand, following
		// develop.md's old manual steps) -- nothing left to provision, but
		// still print the same info a fresh provisioning run would, every
		// time (see the printing block below for why): the point is that
		// nobody has to go find and cat a JSON file to get these, whether
		// this is the first `bootstrap local` or the fiftieth.
		printLocalOpenBaoInfo(existingCreds)
		return nil
	}

	// Reachable here with admin == nil: OpenBao was already initialized
	// (and, since the `sealed` branch above didn't return, currently
	// unsealed) by hand before this automation existed, but whoever did
	// that never went on to run `versola secrets login local` -- so
	// haveCreds is false too. Nothing this function holds can finish that
	// setup without a root token it never had a chance to save.
	if admin == nil {
		return fmt.Errorf(`local OpenBao is already initialized, but this machine has no saved root token to finish provisioning automatically -- this only happens if it was set up by hand before this automation existed. Finish it by hand once (see develop.md's OpenBao section, steps 3-7, using whatever root token it was first initialized with), then run "versola secrets login local %s <role-id>"`, address)
	}

	fmt.Println("Provisioning local OpenBao (kv-v2, AppRole)...")
	token := admin.RootToken

	if err := openbao.EnableKV(ctx, address, token); err != nil {
		return fmt.Errorf("couldn't enable OpenBao's kv-v2 secrets engine: %w", err)
	}
	if err := openbao.EnableApprole(ctx, address, token); err != nil {
		return fmt.Errorf("couldn't enable OpenBao's AppRole auth method: %w", err)
	}

	// Scoped to local's own secrets only -- same policy a human following
	// develop.md's manual steps would write, just generated here instead
	// of typed by hand.
	policy := "path \"secret/data/versola/local/*\" {\n  capabilities = [\"create\", \"read\", \"update\"]\n}\n"
	if err := openbao.WritePolicy(ctx, address, token, localApproleName, policy); err != nil {
		return fmt.Errorf("couldn't write OpenBao policy: %w", err)
	}
	if err := openbao.CreateApproleRole(ctx, address, token, localApproleName, localApproleName); err != nil {
		return fmt.Errorf("couldn't create OpenBao AppRole role: %w", err)
	}

	roleID, err := openbao.ReadRoleID(ctx, address, token, localApproleName)
	if err != nil {
		return fmt.Errorf("couldn't read OpenBao role-id: %w", err)
	}
	secretID, err := openbao.GenerateSecretID(ctx, address, token, localApproleName)
	if err != nil {
		return fmt.Errorf("couldn't generate OpenBao secret-id: %w", err)
	}

	creds := &openbao.Credentials{Address: address, RoleID: roleID, SecretID: secretID}
	if err := openbao.SaveCredentials("local", creds); err != nil {
		return fmt.Errorf("couldn't save OpenBao credentials: %w", err)
	}

	fmt.Println("Local OpenBao is ready:")
	printLocalOpenBaoInfo(creds)
	return nil
}

// printLocalOpenBaoInfo prints the AppRole credentials ProvisionLocal
// either just created or found already stored -- Georgy's own ask:
// someone who needs to poke at local's OpenBao directly (the `bao` CLI, a
// debugging session, handing it to another tool) shouldn't have to go
// find and cat a JSON file first. Fine to print role-id AND secret-id in
// plain text for local specifically -- it's a throwaway container this
// same CLI already creates and destroys freely (see ProvisionLocal's own
// doc comment); the same wouldn't be appropriate for vps's real
// production secret store, and nothing here touches that path.
func printLocalOpenBaoInfo(creds *openbao.Credentials) {
	fmt.Printf("  address:   %s\n", creds.Address)
	fmt.Printf("  role-id:   %s\n", creds.RoleID)
	fmt.Printf("  secret-id: %s\n", creds.SecretID)
	fmt.Println("(also saved to ~/.versola/openbao/local.json)")
}
