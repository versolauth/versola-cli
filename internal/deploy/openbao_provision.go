package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/versolauth/versola-cli/internal/openbao"
)

// approleName is both the AppRole role name and the ACL policy name
// ProvisionOpenBao provisions for target -- develop.md's manual steps use
// "versola-<target>" for both, so this keeps an auto-provisioned role
// indistinguishable from one a human set up by hand following those same
// steps.
func approleName(target string) string {
	return "versola-" + target
}

// ProvisionOpenBao does, over OpenBao's HTTP API, everything develop.md's
// "One-time setup" section otherwise asks a human to run by hand with the
// `bao` CLI (init, unseal, enable kv-v2, enable approle, write a policy,
// create a role, mint credentials) -- for whichever of "local"/"vps" is
// passed as target.
//
// Originally local-only. Georgy (reviewing this repeatedly confusing a
// teammate first running `versola bootstrap local`): a fresh machine's
// `versola configure local` used to fail with "no OpenBao credentials
// stored for 'local'" and point at seven manual `docker exec ... bao ...`
// commands before it would ever start -- fine for vps at the time, where
// a human deliberately performing a one-time admin action against a real
// production secret store was the deliberate design (develop.md's own
// words: "not something configure should ever do silently"), but exactly
// backwards for local: local's OpenBao is a throwaway container this same
// CLI already creates and destroys freely, and "try Versola locally"
// shouldn't need a crash course in AppRole administration first.
//
// Extended to vps on Georgy's own explicit call (Telegram, 23.09.2026,
// in response to the question of whether vps should keep the stricter,
// human-does-it-once design): "по умолчанию нужно разворачивать openbao
// самим... нужно путь до прода максимально сократить" -- teams that
// run on a fresh vps are usually small ones just getting started, and
// cutting the path to a first working deployment matters more than the
// extra caution a real secret store would otherwise call for. The escape
// hatch for whoever WANTS the old, careful, do-it-yourself path is
// --setup-openbao (see cmd/bootstrap.go, cmd/configure.go): passing it
// skips this function entirely for vps and requires credentials to
// already be stored via `versola secrets login vps` instead, exactly
// like every target's `configure` behaved before this function existed.
//
// Idempotent and safe to call on every `configure <target>`, not just the
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
func ProvisionOpenBao(target, address string) error {
	ctx := context.Background()
	role := approleName(target)

	existingCreds, err := openbao.LoadCredentials(target)
	haveCreds := err == nil
	if err != nil && !errors.Is(err, openbao.ErrNoCredentials) {
		return fmt.Errorf("couldn't check for existing OpenBao credentials: %w", err)
	}

	admin, err := openbao.LoadAdminCreds(target)
	haveAdmin := err == nil
	if err != nil && !errors.Is(err, openbao.ErrNoAdminCreds) {
		return fmt.Errorf("couldn't check for saved OpenBao admin credentials: %w", err)
	}

	initialized, sealed, err := openbao.Health(ctx, address)
	if err != nil {
		return fmt.Errorf("couldn't check OpenBao's health: %w", err)
	}

	// Tracked separately from `initialized` itself: the check below that
	// skips re-provisioning when credentials already exist on disk must
	// NOT apply here. A fresh Init means this is, by definition, a BRAND
	// NEW OpenBao with none of kv-v2/AppRole/policy/role set up yet --
	// any <target>.json already on disk at this point is a leftover from
	// a PREVIOUS OpenBao (e.g. the volume was recreated) and is now
	// stale: its role-id/secret-id refer to an AppRole that doesn't exist
	// on this new instance. Trusting it would skip provisioning entirely
	// and leave resolveSecrets to fail on a confusing AppRole login error
	// instead of this function ever getting a chance to explain what's
	// actually wrong.
	justInitialized := !initialized

	if !initialized {
		fmt.Printf("Initializing %s OpenBao...\n", target)
		rootToken, unsealKey, err := openbao.Init(ctx, address)
		if err != nil {
			return fmt.Errorf("couldn't initialize OpenBao: %w", err)
		}
		admin = &openbao.AdminCreds{RootToken: rootToken, UnsealKey: unsealKey}
		if err := openbao.SaveAdminCreds(target, admin); err != nil {
			// Without this file, the NEXT `configure <target>` (after
			// OpenBao's container restarts and reseals -- see
			// openbao.Unseal's own comment) has no way to unseal it
			// again, and neither value is recoverable from OpenBao
			// itself afterward.
			//
			// local: printed to the console instead, so a human can
			// save them by hand (e.g. straight into local-admin.json,
			// same shape this would have written) rather than losing
			// them the moment this process exits -- local's OpenBao is
			// a throwaway container anyway, so "stuck sealed, reset the
			// volume" is an acceptable worst case, and printing at
			// least gives a way to avoid even that.
			//
			// vps: Bekezhan's own call (23.09.2026, after already
			// deciding the REST of this function's UX -- console
			// printing of role-id/secret-id, single key-share -- should
			// match local one-to-one): a failed write here on a real
			// prod secret store shouldn't fall back to printing the
			// root token into a terminal that's presumably open on a
			// shared or logged server session -- fail loudly instead
			// and let the human decide how to recover, rather than
			// silently leaving a root token sitting in scrollback.
			if target == "vps" {
				return fmt.Errorf("OpenBao was just initialized but couldn't save its admin credentials to ~/.versola/openbao/%s-admin.json: %w -- the root token and unseal key this just generated are now lost to this process (not printed, on purpose, for vps -- see ProvisionOpenBao's own comment). Fix whatever's blocking the write (permissions, disk space) and re-run; if that's not possible, you'll need to reset this OpenBao (a fresh volume) and start over", target, err)
			}
			fmt.Printf("(couldn't save OpenBao admin credentials -- %v)\n", err)
			fmt.Println("Save these by hand or this OpenBao is stuck sealed forever once this process exits:")
			fmt.Printf("  root token: %s\n", rootToken)
			fmt.Printf("  unseal key: %s\n", unsealKey)
		}
		haveAdmin = true
		sealed = true // a freshly initialized OpenBao always starts sealed
	}

	if sealed {
		if !haveAdmin {
			return fmt.Errorf(`%s's OpenBao is initialized and sealed, but this machine has no saved unseal key -- this happens if it was set up by hand before this automation existed (see develop.md's OpenBao section for the manual "bao operator unseal" step), if ~/.versola/openbao/%s-admin.json was deleted, or if saving it failed right after a previous Init (in which case the root token and unseal key were printed to the console at the time -- check scrollback/logs from that run). Unseal it by hand once and re-run`, target, target)
		}
		fmt.Printf("Unsealing %s OpenBao...\n", target)
		if err := openbao.Unseal(ctx, address, admin.UnsealKey); err != nil {
			return fmt.Errorf("couldn't unseal OpenBao: %w", err)
		}
	}

	if haveCreds && !justInitialized {
		// Already fully set up by a previous run (or by hand, following
		// develop.md's old manual steps) -- nothing left to provision, but
		// still print the same info a fresh provisioning run would, every
		// time (see printOpenBaoInfo's own comment for why): the point is
		// that nobody has to go find and cat a JSON file to get these,
		// whether this is the first `bootstrap <target>` or the fiftieth.
		//
		// The justInitialized exception: existingCreds here can be a
		// LEFTOVER from a previous OpenBao volume that just got destroyed
		// and recreated (Init above only just ran because Health reported
		// this instance as brand new) -- its role-id/secret-id refer to
		// an AppRole that doesn't exist on THIS instance yet. Falling
		// through instead of returning here means the provisioning block
		// below re-runs for real against the new instance and overwrites
		// <target>.json with credentials that actually work.
		printOpenBaoInfo(target, existingCreds)
		return nil
	}

	// Reachable here with admin == nil: OpenBao was already initialized
	// (and, since the `sealed` branch above didn't return, currently
	// unsealed) by hand before this automation existed, but whoever did
	// that never went on to run `versola secrets login <target>` -- so
	// haveCreds is false too. Nothing this function holds can finish that
	// setup without a root token it never had a chance to save.
	if admin == nil {
		return fmt.Errorf(`%s's OpenBao is already initialized, but this machine has no saved root token to finish provisioning automatically -- this only happens if it was set up by hand before this automation existed. Finish it by hand once (see develop.md's OpenBao section, steps 3-7, using whatever root token it was first initialized with), then run "versola secrets login %s %s <role-id>"`, target, target, address)
	}

	fmt.Printf("Provisioning %s OpenBao (kv-v2, AppRole)...\n", target)
	token := admin.RootToken

	if err := openbao.EnableKV(ctx, address, token); err != nil {
		return fmt.Errorf("couldn't enable OpenBao's kv-v2 secrets engine: %w", err)
	}
	if err := openbao.EnableApprole(ctx, address, token); err != nil {
		return fmt.Errorf("couldn't enable OpenBao's AppRole auth method: %w", err)
	}

	// Scoped to target's own secrets only -- same policy a human
	// following develop.md's manual steps would write, just generated
	// here instead of typed by hand.
	policy := fmt.Sprintf("path \"secret/data/versola/%s/*\" {\n  capabilities = [\"create\", \"read\", \"update\"]\n}\n", target)
	if err := openbao.WritePolicy(ctx, address, token, role, policy); err != nil {
		return fmt.Errorf("couldn't write OpenBao policy: %w", err)
	}
	if err := openbao.CreateApproleRole(ctx, address, token, role, role); err != nil {
		return fmt.Errorf("couldn't create OpenBao AppRole role: %w", err)
	}

	roleID, err := openbao.ReadRoleID(ctx, address, token, role)
	if err != nil {
		return fmt.Errorf("couldn't read OpenBao role-id: %w", err)
	}
	secretID, err := openbao.GenerateSecretID(ctx, address, token, role)
	if err != nil {
		return fmt.Errorf("couldn't generate OpenBao secret-id: %w", err)
	}

	creds := &openbao.Credentials{Address: address, RoleID: roleID, SecretID: secretID}
	if err := openbao.SaveCredentials(target, creds); err != nil {
		return fmt.Errorf("couldn't save OpenBao credentials: %w", err)
	}

	fmt.Printf("%s's OpenBao is ready:\n", target)
	printOpenBaoInfo(target, creds)
	return nil
}

// printOpenBaoInfo prints the AppRole credentials ProvisionOpenBao either
// just created or found already stored -- Georgy's own ask, originally
// for local: someone who needs to poke at OpenBao directly (the `bao`
// CLI, a debugging session, handing it to another tool) shouldn't have to
// go find and cat a JSON file first.
//
// Printing role-id AND secret-id in plain text used to be justified here
// as "fine for local specifically -- it's a throwaway container", with
// vps deliberately excluded. That distinction is gone now that
// ProvisionOpenBao runs for vps by default too (see its own doc comment)
// -- Georgy's call, not an oversight: matching local's UX one-to-one, on
// the same reasoning that decided the rest of this function, rather than
// inventing a quieter vps-only path nobody asked for.
func printOpenBaoInfo(target string, creds *openbao.Credentials) {
	fmt.Printf("  address:   %s\n", creds.Address)
	fmt.Printf("  role-id:   %s\n", creds.RoleID)
	fmt.Printf("  secret-id: %s\n", creds.SecretID)
	fmt.Printf("(also saved to ~/.versola/openbao/%s.json)\n", target)
}
