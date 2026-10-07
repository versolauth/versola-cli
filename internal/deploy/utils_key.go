package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/versolauth/versola-cli/internal/openbao"
)

const (
	// utilsService is the OpenBao path (see openbao.SecretPath) the private key of Versola's
	// `utils` client lives at. Deliberately not one of secretServices: everything under those
	// paths is loaded into that service's container, and central must never hold the key that
	// authenticates as `utils`.
	utilsService = "utils"

	// utilsPrivateKeyKey is the field holding that key, as one JWK.
	utilsPrivateKeyKey = "UTILITY_CLIENT_PRIVATE_JWK"

	// utilsPublicKeyKey is the field in central's own path carrying the matching public half,
	// which central.conf's bootstrap.utility-client.public-key-jwk resolves against.
	utilsPublicKeyKey = "UTILITY_CLIENT_PUBLIC_JWK"

	// utilsKeyFile is where versola-tools writes the freshly generated candidate and where the
	// resolved key is left for the operator: it is `loadgen provision`'s
	// provision.provisioner-private-key, or whatever else authenticates as `utils`.
	utilsKeyFile = "utils.private-key.jwk"
)

// utilsKeyPlan is what reconcileUtilsKey decided.
type utilsKeyPlan struct {
	// private is the key the deployment keeps from here on.
	private string
	// store reports that private is not in OpenBao yet and has to be written there.
	store bool
	// centralPublic is the public half to use for central if OpenBao holds none for it yet.
	centralPublic string
	// warning, when set, describes a pair OpenBao holds that can no longer be made to agree.
	warning string
}

// reconcileUtilsKey keeps the `utils` key pair stable across runs.
//
// gen-env generates a fresh pair every run, but central seeds `utils` exactly once, so a
// regenerated private key would no longer match the public key central keeps. The private key
// already stored in OpenBao therefore wins, as every other stored secret does, and the public
// half offered to central is derived from it instead of taken from this run's candidate -- the
// two can only ever disagree because of a stored pair this function did not make.
//
// candidate is the freshly generated private JWK ("" when versola-tools predates the pair),
// stored what OpenBao holds ("" when nothing), centralStored central's own stored public half.
func reconcileUtilsKey(candidate, stored, centralStored string) (utilsKeyPlan, error) {
	if candidate == "" && stored == "" {
		return utilsKeyPlan{}, nil
	}
	plan := utilsKeyPlan{private: stored}
	if stored == "" {
		plan.private, plan.store = candidate, true
	}
	public, err := publicJWK(plan.private)
	if err != nil {
		return utilsKeyPlan{}, err
	}
	plan.centralPublic = public

	switch {
	case centralStored == "":
		// Nothing seeded yet: this pair is the one central will get.
	case sameJWKPublicKey(centralStored, public):
		// Already consistent.
	case plan.store:
		plan.warning = "central's stored " + utilsPublicKeyKey + " has no private key stored beside it (it predates this " +
			"handling), so the key just stored does not match it: `utils` cannot authenticate with it until its client " +
			"is registered again (see the upgrade note in k8s/README.md of the versola repository)"
	default:
		plan.warning = "central's stored " + utilsPublicKeyKey + " does not match the private key stored under " +
			openbao.SecretPath("<target>", utilsService) + "; central keeps the former"
	}
	return plan, nil
}

// publicJWK is the key with its private half ("d") removed.
func publicJWK(private string) (string, error) {
	fields, err := parseJWK(private)
	if err != nil {
		return "", err
	}
	if _, ok := fields["d"]; !ok {
		return "", errors.New("the " + utilsPrivateKeyKey + " JWK carries no private half (\"d\")")
	}
	delete(fields, "d")
	out, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func parseJWK(document string) (map[string]any, error) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(document)), &fields); err != nil {
		return nil, fmt.Errorf("the %s is not a JWK: %w", utilsPrivateKeyKey, err)
	}
	return fields, nil
}

// sameJWKPublicKey compares the public coordinates only: central's value may be formatted or
// ordered differently from one derived here.
func sameJWKPublicKey(a, b string) bool {
	left, err := parseJWK(a)
	if err != nil {
		return false
	}
	right, err := parseJWK(b)
	if err != nil {
		return false
	}
	for _, field := range []string{"kty", "crv", "x", "y"} {
		if left[field] != right[field] {
			return false
		}
	}
	return true
}

// resolveUtilsKey applies reconcileUtilsKey against OpenBao and the bundle directory: the
// candidate versola-tools wrote is read from dir, the resolved key is stored if new and written
// back to dir at 0600, and central's candidate public half is replaced by the one that goes
// with it. Does nothing when versola-tools wrote no candidate and nothing is stored.
func resolveUtilsKey(ctx context.Context, client *openbao.Client, dir, target string, centralExisting, centralCandidates map[string]string) error {
	keyPath := filepath.Join(dir, utilsKeyFile)
	candidate := ""
	if raw, err := os.ReadFile(keyPath); err == nil {
		candidate = strings.TrimSpace(string(raw))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("couldn't read %s: %w", keyPath, err)
	}

	path := openbao.SecretPath(target, utilsService)
	existing, _, err := client.ReadSecret(ctx, path)
	if err != nil {
		return fmt.Errorf("couldn't read the %s key from OpenBao: %w", utilsService, err)
	}
	plan, err := reconcileUtilsKey(candidate, existing[utilsPrivateKeyKey], centralExisting[utilsPublicKeyKey])
	if err != nil {
		return err
	}
	if plan.private == "" {
		return nil
	}
	if plan.store {
		stored := make(map[string]string, len(existing)+1)
		for key, value := range existing {
			stored[key] = value
		}
		stored[utilsPrivateKeyKey] = plan.private
		if err := client.WriteSecret(ctx, path, stored); err != nil {
			return fmt.Errorf("couldn't store the %s key in OpenBao: %w", utilsService, err)
		}
	}
	if plan.warning != "" {
		fmt.Printf("  warning: %s\n", strings.ReplaceAll(plan.warning, "<target>", target))
	}
	// Only offered to central where it has none stored: mergeSecrets lets a stored value win.
	centralCandidates[utilsPublicKeyKey] = plan.centralPublic
	if err := os.WriteFile(keyPath, []byte(plan.private+"\n"), 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", keyPath, err)
	}
	return os.Chmod(keyPath, 0o600)
}
