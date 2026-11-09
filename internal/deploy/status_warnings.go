package deploy

import (
	"fmt"
	"sort"

	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

// ServiceWarnings compares the services configured here with Docker's snapshot.
// A missing container is not in `ps -a`, so looking at containers alone would
// still report no problem after `down`. Only services the CLI starts as daemons
// are checked: successful one-shot jobs (notably mtls-init) are not outages.
// The compose inventory decides whether optional services exist; an external
// Postgres or OpenBao must not be reported as a missing local container.
func ServiceWarnings(st *state.State, services []string, containers []ContainerStatus) []string {
	expected := map[string]bool{}
	for _, service := range services {
		base, slot := splitSlotService(service)
		if slot != 0 {
			if st == nil {
				expected[service] = true
				continue
			}
			// Base services can remain in Compose while slot 1 has been
			// retired during a rollout. Check the recorded active slots,
			// including the stand-in, rather than resurrecting slot 1.
			for _, active := range st.ActiveSlots(base) {
				expected[topology.SlotService(base, active.N)] = true
			}
		} else if service == "proxy" || service == "openbao" || service == "postgres" {
			expected[service] = true
		}
	}
	byService := map[string][]ContainerStatus{}
	for _, c := range containers {
		service := c.composeService
		if service == "" {
			service = c.Service
			if c.Slot > 0 {
				service = topology.SlotService(service, c.Slot)
			}
		}
		byService[service] = append(byService[service], c)
	}
	names := make([]string, 0, len(expected))
	for service := range expected {
		names = append(names, service)
	}
	sort.Strings(names)
	warnings := []string{}
	for _, service := range names {
		list := byService[service]
		if len(list) == 0 {
			warnings = append(warnings, fmt.Sprintf("service %s has no container; %s", service, LogsHint(service)))
			continue
		}
		for _, c := range list {
			problem := ""
			switch {
			case c.State == "restarting":
				problem = "is restarting"
			case c.State != "running":
				problem = fmt.Sprintf("is not running (state: %s)", c.State)
			case c.Health == "unhealthy":
				problem = "is unhealthy"
			}
			if problem != "" {
				warnings = append(warnings, fmt.Sprintf("service %s (%s) %s; %s", service, c.Name, problem, LogsHint(service)))
			}
		}
	}
	return warnings
}
