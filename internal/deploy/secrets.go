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
			return fmt.Errorf("no OpenBao credentials stored for %q — run `versola secrets login %s <address> <role-id> <secret-id>` first", target, target)
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

	final := make(map[string]string, len(candidates))
	wroteAnyNew := false
	for key, candidate := range candidates {
		if found {
			if v, has := existing[key]; has {
				final[key] = v
				continue
			}
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

	return writeDotenv(filepath.Join(dir, service+".secrets.env"), final)
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
