package deploy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/secrets"
	"github.com/versolauth/versola-cli/internal/state"
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
//
// Best-effort, not fatal. pullAndRunTools now runs the tools container
// with -u matching this process's own UID/GID on a normal (rootful)
// Docker daemon (see its own comment, and docker.IsRootless for why
// rootless daemons are deliberately left alone), so on a native Linux
// host these files are usually already owned by the CLI by the time
// this runs, and the chmod below is expected to actually succeed there
// -- shrinking the "operation not permitted" gap that used to leave
// *.generated-secrets.env world-readable indefinitely (KNOWN-ISSUES.md,
// confirmed on the real vps 09.09.2026, before that fix).
//
// "Shrinking", not closing: -u fixes who owns the file, not the
// container's umask. entrypoint.sh's cp/java calls still create these
// files at whatever default mode the image's umask gives them (usually
// 644) before this function ever runs -- so there's still a brief
// window, between the tools container exiting and this chmod actually
// running, where the file is world-readable on a genuinely multi-user
// host. What -u closes is the *indefinite* version of that window (the
// file no longer stays 644 forever, only for a moment), not the window
// itself. A fully airtight fix would need versola-tools' own
// entrypoint.sh to `umask 077` before writing these files -- a
// cross-repo change, out of scope here, noted as a possible follow-up
// in KNOWN-ISSUES.md.
//
// This is still treated as best-effort rather than fatal: -u is skipped
// on Windows and on a rootless daemon (see pullAndRunTools), and
// nothing here guarantees every future environment this runs in maps
// UIDs the same way a plain rootful native-Linux Docker install does.
// Treating a chmod failure as fatal would abort `configure` before
// secret resolution even starts, on exactly the kind of unusual setup
// this can't anticipate -- staying best-effort means an environment
// where this still doesn't line up degrades back to the old (logged)
// exposure window instead of losing the ability to deploy at all.
// Deletion doesn't have this problem -- removing a file only needs
// write access to its directory, not ownership of the file itself --
// so resolveServiceSecrets' cleanup on the happy path is unaffected
// either way.
func restrictGeneratedSecretsPerms(dir string) {
	for _, service := range secretServices {
		path := filepath.Join(dir, service+".generated-secrets.env")
		if err := os.Chmod(path, 0o600); err != nil {
			fmt.Printf("  (couldn't restrict permissions on %s: %v)\n", path, err)
		}
	}
}

// SecretOptions are the operator's choices about secrets for one configure.
type SecretOptions struct {
	// GenerateMissing is --generate-missing: names of secrets the operator
	// confirms may be generated although the plan would stop (a secret that
	// looks lost, or an install of unknown origin). A confirmation about names;
	// a group is only generated if every one of its members is named.
	GenerateMissing []string
	// AllowLegacy is --allow-legacy-secrets: proceed with a release that has no
	// secrets.schema.json although OpenBao already holds secrets or this machine
	// has a record of a deployment, i.e. with the old rule "anything missing is
	// generated", which cannot tell a new secret from a lost one.
	AllowLegacy bool
}

func (o SecretOptions) generateSet() map[string]bool {
	set := make(map[string]bool, len(o.GenerateMissing))
	for _, name := range o.GenerateMissing {
		set[name] = true
	}
	return set
}

// secretsOutcome is what resolveSecrets reports back to Configure.
type secretsOutcome struct {
	// NewPostgresPassword is the Postgres password this run stored for the first
	// time ("" if none); Configure prints the SQL that sets it on the role.
	NewPostgresPassword string
	// Revision is the revision of the schema the secrets were settled against;
	// 0 when the release has no schema.
	Revision int
}

// resolveSecrets turns each <service>.generated-secrets.env file
// versola-tools wrote into dir into a <service>.secrets.env file Compose
// actually loads into the container -- substituting OpenBao's existing
// value for any key that's already there instead of the freshly
// generated candidate next to it, and storing the candidate in OpenBao
// for any key that isn't there yet.
//
// Before it writes anything it settles what is allowed (see secrets.Plan):
// a secret that looks lost, a half-stored key pair, shared copies that
// disagree, an install of unknown origin all stop the run with nothing
// written, because a new value generated in their place would make stored
// data unreadable. A release that has no secrets.schema.json cannot be
// planned and is handled by the old rule (see legacyGuard).
//
// This has to run after versola-tools (it reads what that wrote) and
// before Up starts anything (compose.fragment.yml.template's env_file:
// entries expect these files to already exist) -- Configure is where
// both of those are true.
func resolveSecrets(dir, target, version string, opts SecretOptions) (secretsOutcome, error) {
	creds, err := openbao.LoadCredentials(target)
	if err != nil {
		if errors.Is(err, openbao.ErrNoCredentials) {
			return secretsOutcome{}, fmt.Errorf("no OpenBao credentials stored for %q — run `versola secrets login %s <address> <role-id>` first (it prompts for the secret ID separately)", target, target)
		}
		return secretsOutcome{}, err
	}

	client := openbao.NewClient(creds)
	ctx := context.Background()
	if err := client.Login(ctx); err != nil {
		return secretsOutcome{}, err
	}
	return resolveSecretsWith(ctx, client, dir, target, version, opts)
}

// resolveSecretsWith is resolveSecrets with the OpenBao client already logged in.
func resolveSecretsWith(ctx context.Context, client *openbao.Client, dir, target, version string, opts SecretOptions) (secretsOutcome, error) {
	// Everything is read before anything is written, so the plan, and the
	// Postgres password, can be settled across all three services first (see
	// pickPostgresPassword).
	candidates := make(map[string]map[string]string, len(secretServices))
	existing := make(map[string]map[string]string, len(secretServices))
	for _, service := range secretServices {
		c, err := readDotenv(filepath.Join(dir, service+".generated-secrets.env"))
		if err != nil {
			return secretsOutcome{}, err
		}
		e, _, err := client.ReadSecret(ctx, openbao.SecretPath(target, service))
		if err != nil {
			return secretsOutcome{}, fmt.Errorf("couldn't read existing %s secrets from OpenBao: %w", service, err)
		}
		candidates[service], existing[service] = c, e
	}

	prev, installing, err := loadPrevious(target)
	if err != nil {
		return secretsOutcome{}, err
	}
	hasRecord := prev != nil && prev.Target == target

	var out secretsOutcome
	var shared map[string]map[string]string // service -> name -> value, for secrets listed for several services
	replace := map[string]map[string]bool{} // service -> names the plan overwrites with a candidate
	writeNote := false                      // whether this run is a first install proper (see state.Pending)
	schema, err := secrets.Load(filepath.Join(dir, secrets.FileName), target)
	switch {
	case err == nil:
		out.Revision = schema.RevisionNumber()
		result := secrets.Plan(schema, candidates, existing, prev, secrets.Options{
			Target:            target,
			GenerateMissing:   opts.generateSet(),
			InstallInProgress: installing,
		})
		if !result.OK() {
			// Names and actions only: a Result cannot hold a value.
			if err := secrets.RenderText(os.Stdout, version, result); err != nil {
				return secretsOutcome{}, err
			}
			return secretsOutcome{}, fmt.Errorf("the secrets plan has %d problem(s), listed above: nothing was written to OpenBao", len(result.Problems))
		}
		fmt.Printf("  secrets plan (%s, schema revision %d): OK\n", result.State, result.SchemaRevision)
		shared = sharedValues(schema, existing, candidates)
		for _, a := range result.Actions {
			if a.Reason != secrets.ReasonRestartInstall {
				continue
			}
			// A key group an interrupted run left half-written is generated again
			// whole, from this run's candidates: not from what is stored for any
			// of its services (see secrets.Plan).
			delete(shared[a.Service], a.Name)
			if a.Kind == secrets.ActionReplace {
				if replace[a.Service] == nil {
					replace[a.Service] = map[string]bool{}
				}
				replace[a.Service][a.Name] = true
				fmt.Printf("  replace %s/%s: an interrupted run left its key group half-written, so the group is generated again whole\n", a.Service, a.Name)
			}
		}
		if names := unlistedCandidates(schema, candidates); len(names) > 0 {
			fmt.Printf("  warning: versola-tools wrote secrets the schema does not list: %s -- stored as before, but not covered by the plan\n", strings.Join(names, ", "))
		}
		// Only a first install proper gets the note. NOT an install of unknown
		// origin (OpenBao holds secrets, this machine has no record): that may be a
		// live deployment, and a note left by its configure would let the next run
		// regenerate what looks lost.
		writeNote = target != "local" && (result.State == secrets.StateFirstInstall || result.State == secrets.StateInstalling)
	case errors.Is(err, secrets.ErrNoSchema):
		if len(opts.GenerateMissing) > 0 {
			fmt.Printf("  warning: --generate-missing is ignored: Versola %s has no %s, so there is no plan to confirm\n", version, secrets.FileName)
		}
		fresh, err := legacyGuard(target, version, existing, hasRecord, installing, opts)
		if err != nil {
			return secretsOutcome{}, err
		}
		writeNote = fresh
	default:
		return secretsOutcome{}, err
	}

	if services := conflictingPostgresPasswords(existing); services != nil {
		return secretsOutcome{}, fmt.Errorf("OpenBao holds different Postgres passwords for %s, but they all log in as the same Postgres role, so at least one of them can't connect. "+
			"This CLI can't tell which one the role actually has, so it won't pick one: set the same, correct %s under secret/versola/%s/<service> for each of them, then re-run configure",
			strings.Join(services, ", "), postgresPasswordKey, target)
	}
	pgPassword, pgIsNew := pickPostgresPassword(existing, candidates)
	if pgPassword != "" {
		if shared == nil {
			shared = map[string]map[string]string{}
		}
		for _, service := range secretServices {
			// Only where the service has the key at all, as before.
			if _, ok := candidates[service][postgresPasswordKey]; !ok {
				if _, ok := existing[service][postgresPasswordKey]; !ok {
					continue
				}
			}
			if shared[service] == nil {
				shared[service] = map[string]string{}
			}
			shared[service][postgresPasswordKey] = pgPassword
		}
	}
	// An install that was interrupted may have stored the new Postgres password
	// and died before telling the operator (a killed process cannot print): the
	// retry finds it stored and would call it old. While an unfinished install is
	// being continued the role-setup instructions are printed again; saying them
	// twice is harmless, never saying them leaves the role without its password.
	if installing && !hasRecord && !pgIsNew && pgPassword != "" {
		out.NewPostgresPassword = pgPassword
	}

	// An install that has no record yet is about to start writing secrets: note
	// it first, so that if it fails partway the next run knows what the store
	// holds (see state.Pending). Before resolveUtilsKey, which writes too.
	if writeNote {
		if err := state.SavePending(state.Pending{Target: target, SecretsRevision: out.Revision, StartedAt: time.Now().UTC()}); err != nil {
			return out, err
		}
	}

	// The `utils` key pair is settled before any service's secrets are merged, because it
	// decides which public half central is offered. Its private half is stored apart from the
	// service paths (see utilsService) and left in the bundle directory for the operator.
	if err := resolveUtilsKey(ctx, client, dir, target, existing["central"], candidates["central"]); err != nil {
		return out, err
	}

	// NewPostgresPassword is set as soon as the new password is
	// actually stored in OpenBao for at least one service -- even if a
	// later service fails. From then on the next configure finds it
	// stored and treats it as old, so this run is the only one that can
	// tell the operator about it (Configure prints it on error too).
	for _, service := range secretServices {
		wrotePg, err := resolveServiceSecrets(ctx, client, dir, target, service, existing[service], candidates[service], shared[service], replace[service])
		if wrotePg && pgIsNew {
			out.NewPostgresPassword = pgPassword
		}
		if err != nil {
			return out, fmt.Errorf("couldn't resolve secrets for %s: %w", service, err)
		}
	}
	return out, nil
}

// unlistedCandidates names (service/name, sorted) the candidates versola-tools
// wrote that the schema does not list for that service. The utils entries are
// listed by the schema, so they are never among them.
func unlistedCandidates(schema *secrets.Schema, candidates map[string]map[string]string) []string {
	listed := map[string]bool{}
	for _, spec := range schema.Secrets {
		for _, service := range spec.Services {
			listed[service+"/"+spec.Name] = true
		}
	}
	var out []string
	for service, m := range candidates {
		for name := range m {
			if !listed[service+"/"+name] {
				out = append(out, service+"/"+name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// legacyGuard decides whether a release that wrote no secrets.schema.json may be
// configured by the old rule (an existing value wins, anything missing is
// generated). Without a schema there is nothing to tell a new secret from a lost
// one, so the old rule would silently replace a lost key with a new one. It is
// safe only when there is nothing to lose -- an empty OpenBao and no record of a
// deployment, or the retry of a first install of this machine's own that did not
// finish -- and on local, a throwaway; otherwise the operator has to ask for it
// with --allow-legacy-secrets.
//
// fresh reports that this run is a first install proper, which gets the note of
// an unfinished install (see state.Pending) so that its own retry is recognised.
func legacyGuard(target, version string, existing map[string]map[string]string, hasRecord, installing bool, opts SecretOptions) (fresh bool, err error) {
	if target == "local" {
		return false, nil
	}
	if installing && !hasRecord {
		fmt.Printf("  note: continuing this machine's unfinished first install of Versola %s (no %s: settled by the old rule).\n", version, secrets.FileName)
		return true, nil
	}
	holds := hasRecord
	for _, service := range secretServices {
		if len(existing[service]) > 0 {
			holds = true
		}
	}
	if !holds {
		fmt.Printf("  note: Versola %s has no %s, so its secrets are settled by the old rule (anything missing is generated). Nothing is stored yet, so there is nothing to lose.\n", version, secrets.FileName)
		return true, nil
	}
	if !opts.AllowLegacy {
		return false, fmt.Errorf("Versola %s writes no %s, so this CLI cannot tell a secret that is new from one that was lost, "+
			"and the old rule would generate a new value for anything missing from OpenBao -- which makes data encrypted with the lost one unreadable. "+
			"Nothing was written to OpenBao. Deploy a release that has the schema, or, if you know that nothing stored depends on what could be missing, "+
			"run again with --allow-legacy-secrets", version, secrets.FileName)
	}
	fmt.Printf("  warning: Versola %s has no %s; --allow-legacy-secrets: anything missing from OpenBao is generated, lost or not.\n", version, secrets.FileName)
	return false, nil
}

// withoutKeys is m without the given keys (m itself when there are none), so that
// merging gives each of them its candidate again.
func withoutKeys(m map[string]string, drop map[string]bool) map[string]string {
	if len(drop) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !drop[k] {
			out[k] = v
		}
	}
	return out
}

// sharedValues gives, for each secret the schema lists for several services
// (which is ONE value, see secrets.Plan), the value every one of them has to
// get: what the store already holds for any of them, else this run's candidate.
// Keyed by service, then name. A secret listed for one service is not in it.
func sharedValues(schema *secrets.Schema, existing, candidates map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, spec := range schema.Secrets {
		if len(spec.Services) < 2 {
			continue
		}
		value, found := "", false
		for _, service := range spec.Services {
			if v, ok := existing[service][spec.Name]; ok {
				value, found = v, true
				break
			}
		}
		if !found {
			for _, service := range spec.Services {
				if v, ok := candidates[service][spec.Name]; ok {
					value, found = v, true
					break
				}
			}
		}
		if !found {
			continue
		}
		for _, service := range spec.Services {
			if out[service] == nil {
				out[service] = map[string]string{}
			}
			out[service][spec.Name] = value
		}
	}
	return out
}

// postgresPasswordKey is the one secret that has to match something
// outside Versola: the password of the Postgres role the services log in
// as. gen-env.scala only emits it for vps (docker-local's Postgres is a
// container created with a fixed password).
const postgresPasswordKey = "POSTGRES_PASSWORD"

// pickPostgresPassword settles the Postgres password once for all three
// services, which share one Postgres role: a value already stored in
// OpenBao wins (resolveSecrets has already refused stored values that
// disagree, see conflictingPostgresPasswords), so a service missing it
// gets the same one; otherwise auth's (or the first service's) freshly
// generated candidate is used everywhere, and isNew reports that -- the
// role on the Postgres server doesn't have it yet, and someone has to set
// it there (see Configure). Returns "" when no service has the key at all
// (local).
func pickPostgresPassword(existing, candidates map[string]map[string]string) (password string, isNew bool) {
	for _, service := range secretServices {
		if v, ok := existing[service][postgresPasswordKey]; ok {
			return v, false
		}
	}
	for _, service := range secretServices {
		if v, ok := candidates[service][postgresPasswordKey]; ok {
			return v, true
		}
	}
	return "", false
}

// conflictingPostgresPasswords returns the services that have a Postgres
// password stored in OpenBao, if those stored values don't all agree; nil
// when they do (or fewer than two are stored). Overwriting them with one
// of the values would be a guess -- whichever one the Postgres role
// really has is the one that works, and that's not knowable from here.
func conflictingPostgresPasswords(existing map[string]map[string]string) []string {
	var services []string
	values := map[string]bool{}
	for _, service := range secretServices {
		if v, ok := existing[service][postgresPasswordKey]; ok {
			services = append(services, service)
			values[v] = true
		}
	}
	if len(values) > 1 {
		return services
	}
	return nil
}

// mergeSecrets computes what's stored for one service: whatever OpenBao
// already has, plus this run's candidate for every key it doesn't -- except
// the Postgres password, which takes pgPassword so all three services
// agree on it. wroteNew reports whether anything was added.
func mergeSecrets(existing, candidates map[string]string, pgPassword string) (final map[string]string, wroteNew bool) {
	var shared map[string]string
	if pgPassword != "" {
		shared = map[string]string{postgresPasswordKey: pgPassword}
	}
	return mergeSecretsShared(existing, candidates, shared)
}

// mergeSecretsShared is mergeSecrets for any number of secrets that several
// services share: for a key in shared, a service that lacks it gets that value
// and not its own candidate, so the services agree. (Only the Postgres password
// used to be handled so: a shared key such as CLIENT_SECRETS_SECRET stored for
// auth but missing for central used to get central a fresh random value that
// differed from auth's.)
func mergeSecretsShared(existing, candidates, shared map[string]string) (final map[string]string, wroteNew bool) {
	// Starts as a copy of whatever's already stored, not empty -- the
	// write in resolveServiceSecrets is a full replace (OpenBao's KV v2
	// "put", not a merge), so anything already at this path that isn't
	// also touched by the loop after this has to already be in final or
	// it's gone for good.
	final = make(map[string]string, len(existing)+len(candidates))
	for key, v := range existing {
		final[key] = v
	}
	for key, candidate := range candidates {
		if _, has := final[key]; has {
			continue
		}
		// Not in OpenBao yet -- this run's freshly generated candidate
		// becomes the real value from here on, unless the secret is shared
		// and its value is already settled.
		if v, ok := shared[key]; ok {
			candidate = v
		}
		final[key] = candidate
		wroteNew = true
	}
	// A shared value the plan settled for a key this service has no candidate for
	// (a fill-shared copy) is still stored: the plan said "create".
	for key, v := range shared {
		if _, has := final[key]; !has {
			final[key] = v
			wroteNew = true
		}
	}
	return final, wroteNew
}

// resolveServiceSecrets stores and writes out one service's secrets.
// wrotePg reports whether this call stored a Postgres password in OpenBao
// that wasn't there before.
func resolveServiceSecrets(ctx context.Context, client *openbao.Client, dir, target, service string, existing, candidates, shared map[string]string, replace map[string]bool) (wrotePg bool, err error) {
	final, wroteNew := mergeSecretsShared(withoutKeys(existing, replace), candidates, shared)
	if wroteNew {
		if err := client.WriteSecret(ctx, openbao.SecretPath(target, service), final); err != nil {
			return false, fmt.Errorf("couldn't store new secrets in OpenBao: %w", err)
		}
		_, hadPg := existing[postgresPasswordKey]
		_, hasPg := final[postgresPasswordKey]
		wrotePg = hasPg && !hadPg
	}

	if err := writeDotenv(filepath.Join(dir, service+".secrets.env"), final); err != nil {
		return wrotePg, err
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
		return wrotePg, fmt.Errorf("couldn't remove %s: %w", candidatesPath, err)
	}
	return wrotePg, nil
}

func readDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("couldn't read %s: %w", path, err)
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Cut at the first "=" only, not every one: values here can be
		// standard (not URL-safe) base64, which pads with trailing "="
		// characters. The key itself never contains one.
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// The line number, not the line: it holds (part of) a secret value.
			return nil, fmt.Errorf("couldn't parse %s: malformed line %d", path, lineNo)
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
