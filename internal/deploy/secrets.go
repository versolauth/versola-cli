package deploy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/versolauth/versola-cli/internal/openbao"
)

// secretServices lists the deployments resolveSecrets handles, matching
// the *.generated-secrets.env files gen-env.scala writes (see its
// writeGeneratedSecrets) and the env_file: entries in
// compose.fragment.yml.template.
var secretServices = []string{"auth", "central", "edge"}

// restrictGeneratedSecretsPerms tightens every *.generated-secrets.env
// file in dir to 0600, right after versola-tools writes them and before
// resolveSecrets gets a chance to run (which can fail, or not run at all
// yet -- see Configure's own comment on why that gap matters). Whichever
// of these resolveServiceSecrets later resolves successfully, it removes
// outright; this is what protects the ones still sitting there if that
// never happens.
func restrictGeneratedSecretsPerms(dir string) error {
	for _, service := range secretServices {
		path := filepath.Join(dir, service+".generated-secrets.env")
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("couldn't restrict permissions on %s: %w", path, err)
		}
	}
	return nil
}

// resolveSecrets turns each <service>.generated-secrets.env file
// versola-tools wrote into dir into a <service>.secrets.env file Compose
// actually loads into the container -- substituting OpenBao's existing
// value for any key that's already there instead of the freshly
// generated candidate next to it, and storing the candidate in OpenBao
// for any key that isn't there yet.
//
// This has to run after versola-tools (it reads what that wrote) and
// before Up starts anything (compose.fragment.yml.template's env_file:
// entries expect these files to already exist) -- Configure is where
// both of those are true.
func resolveSecrets(dir, target string) error {
	creds, err := openbao.LoadCredentials(target)
	if err != nil {
		if errors.Is(err, openbao.ErrNoCredentials) {
			return fmt.Errorf("no OpenBao credentials stored for %q — run `versola secrets login %s <address> <role-id>` first (it prompts for the secret ID separately)", target, target)
		}
		return err
	}

	client := openbao.NewClient(creds)
	ctx := context.Background()
	if err := client.Login(ctx); err != nil {
		return err
	}

	for _, service := range secretServices {
		if err := resolveServiceSecrets(ctx, client, dir, target, service); err != nil {
			return fmt.Errorf("couldn't resolve secrets for %s: %w", service, err)
		}
	}
	return nil
}

func resolveServiceSecrets(ctx context.Context, client *openbao.Client, dir, target, service string) error {
	candidates, err := readDotenv(filepath.Join(dir, service+".generated-secrets.env"))
	if err != nil {
		return err
	}

	path := openbao.SecretPath(target, service)
	existing, found, err := client.ReadSecret(ctx, path)
	if err != nil {
		return fmt.Errorf("couldn't read existing secrets from OpenBao: %w", err)
	}

	// Starts as a copy of whatever's already stored, not empty -- the
	// write below is a full replace (OpenBao's KV v2 "put", not a merge),
	// so anything already at this path that isn't also touched by the
	// loop after this has to already be in final or it's gone for good.
	// Every key this run's candidates file could ever contain currently
	// also has an entry in existing once seeded by hand (see develop.md's
	// vps seeding section) or written by a previous run, but that's an
	// invariant of what gen-env.scala happens to generate today, not
	// something this function can rely on staying true — starting from
	// existing instead of from candidates means it doesn't have to.
	final := make(map[string]string, len(existing)+len(candidates))
	if found {
		for key, v := range existing {
			final[key] = v
		}
	}

	wroteAnyNew := false
	for key, candidate := range candidates {
		if _, has := final[key]; has {
			continue
		}
		// Not in OpenBao yet -- this run's freshly generated candidate
		// becomes the real value from here on.
		final[key] = candidate
		wroteAnyNew = true
	}

	if wroteAnyNew {
		if err := client.WriteSecret(ctx, path, final); err != nil {
			return fmt.Errorf("couldn't store new secrets in OpenBao: %w", err)
		}
	}

	if err := writeDotenv(filepath.Join(dir, service+".secrets.env"), final); err != nil {
		return err
	}

	// The generated-secrets.env candidates versola-tools wrote are secret
	// material too -- a candidate becomes the real, live value the first
	// time this ever runs against an empty OpenBao path (see the loop
	// above) -- but unlike *.secrets.env (written 0600 by writeDotenv)
	// they land in this bundle directory at whatever ordinary permissions
	// the tools container's own write left them at, readable by any other
	// local user on a shared machine (most relevant on vps, not a single-
	// user Windows dev box). Nothing reads this file again after this
	// point -- only *.secrets.env is referenced by
	// compose.fragment.yml.template's env_file: entries -- so removing it
	// outright closes that gap more simply than chmod'ing it consistently
	// across the platforms this runs on (Windows for docker-local, Linux
	// for vps).
	candidatesPath := filepath.Join(dir, service+".generated-secrets.env")
	if err := os.Remove(candidatesPath); err != nil {
		return fmt.Errorf("couldn't remove %s: %w", candidatesPath, err)
	}
	return nil
}

func readDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("couldn't read %s: %w", path, err)
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Cut at the first "=" only, not every one: values here can be
		// standard (not URL-safe) base64, which pads with trailing "="
		// characters. The key itself never contains one.
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("couldn't parse %s: malformed line %q", path, line)
		}
		result[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("couldn't read %s: %w", path, err)
	}
	return result, nil
}

func writeDotenv(path string, values map[string]string) error {
	var b strings.Builder
	for key, value := range values {
		fmt.Fprintf(&b, "%s=%s\n", key, value)
	}
	// 0o600, not the 0o644 most files this CLI writes into the bundle
	// directory use: this one holds real secret values.
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("couldn't write %s: %w", path, err)
	}
	return nil
}
