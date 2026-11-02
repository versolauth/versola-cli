package deploy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// DeploymentServices lists the compose services of the deployment whose
// compose.yml is at composePath: every file ComposeArgs passes (compose.yml,
// overrides.yml, replicas.yml, proxy.yml), so the replicas beyond slot 1, the
// reverse proxy, OpenBao, step-ca and, when the deployment has one, Postgres
// are all in it. The names are what Compose itself knows, not a list kept
// here, so a release that adds or renames a service needs no new CLI.
//
// `config --services` prints names only, unlike `config --format json`
// (loadTopology), which inlines env_file values -- and still, on failure, its
// stderr is never quoted: Compose's parse errors for env files repeat the
// offending line (see composeConfigError, which explains the failure
// instead).
func DeploymentServices(composePath string) ([]string, error) {
	out, err := docker.Output(state.ComposeArgs(composePath, "config", "--services")...)
	if err != nil {
		return nil, composeConfigError(composePath, err)
	}
	var services []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			services = append(services, name)
		}
	}
	sort.Strings(services)
	return services, nil
}

// ResolveLogTargets maps the names a user gives `versola logs` to compose
// services of the deployment (services, from DeploymentServices), in the
// order given and without repeats. nil names means every service, and so
// resolves to nil (compose logs with no service shows them all).
//
// A name is either a compose service as it is ("central", "proxy", "auth-3",
// "step-ca"), or the base of a service with replicas: "auth" is every slot of
// auth -- "auth" itself (slot 1, see topology.SlotService) and each "auth-N"
// -- because "what is auth doing" is the question when auth misbehaves, not
// which replica. "auth-1" is accepted for slot 1, which `status` prints as
// "slots 1, 3" but whose compose service has no number.
//
// "postgres" on a vps deployment whose Postgres is not a container of its own
// gets a sentence of its own rather than the unknown-name list: the customer
// asked a fair question whose answer is "not here".
func ResolveLogTargets(names, services []string, st *state.State) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	known := make(map[string]bool, len(services))
	for _, s := range services {
		known[s] = true
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, name := range names {
		if base, ok := slotOneAlias(name); ok && known[base] && !known[name] {
			add(base)
			continue
		}
		if !known[name] {
			if name == "postgres" && st != nil && st.Target == "vps" && st.PostgresMode != PostgresModeBundled {
				where := ""
				if st.PostgresHost != "" {
					where = " (" + st.PostgresHost + ")"
				}
				return nil, fmt.Errorf("this deployment uses your own Postgres%s, not a container of its own -- its logs are on the server that runs it, not something versola can show", where)
			}
			return nil, fmt.Errorf("no service %q in this deployment -- it has: %s", name, strings.Join(services, ", "))
		}
		add(name)
		// The other slots of a base name: "auth-2", "auth-3", ... ("auth"
		// itself was added just above). A name that is already a slot
		// ("auth-3") has no replicas of its own: "auth-3-2" is not a thing
		// SlotService makes, and the prefix test below never matches it.
		for _, s := range services {
			if rest, ok := strings.CutPrefix(s, name+"-"); ok && isSlotNumber(rest) {
				add(s)
			}
		}
	}
	return out, nil
}

// slotOneAlias: name is "<base>-1", the spelling of slot 1 that
// topology.SlotService never produces.
func slotOneAlias(name string) (base string, ok bool) {
	base, ok = strings.CutSuffix(name, "-1")
	return base, ok && base != ""
}

// isSlotNumber: s is a slot suffix SlotService writes -- 2 and up, digits only
// (so "step-ca" is not a replica of a service "step", nor "mtls-init" one of
// "mtls").
func isSlotNumber(s string) bool {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 2
}
