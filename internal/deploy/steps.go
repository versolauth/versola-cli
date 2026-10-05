package deploy

import "sort"

// startStep is one `docker compose` call of `up` and the services to wait
// for after it.
type startStep struct {
	Message string   // printed before the call
	Name    string   // what the call starts, for its error
	Args    []string // arguments after `docker compose -f ...`
	Ready   []string // services whose readiness is awaited afterwards
}

// appStartSteps orders how auth and edge replicas are started.
//
// Slot 1 of each service goes first, in one call and with depends_on (it
// is what a deployment of a single replica has always done). Every other
// replica follows on its own, lowest slot first, started with --no-deps
// (central is already up, and without it Compose would also try to
// recreate whatever the replica depends on) and awaited before the next,
// so a replica that does not come up stops the rollout there.
//
// legacyGateway adds versola-tools' own nginx to the first call -- local
// deployments from before this CLI generated the proxy.
func appStartSteps(auth, edge []replica, legacyGateway bool) []startStep {
	var first []replica
	var rest []replica
	for _, group := range [][]replica{auth, edge} {
		for _, r := range group {
			if r.Slot == 1 {
				first = append(first, r)
			} else {
				rest = append(rest, r)
			}
		}
	}
	// Stable: within a slot, auth before edge (the order of the groups).
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Slot < rest[j].Slot })

	var steps []startStep
	if len(first) > 0 || legacyGateway {
		names := serviceNames(first)
		msg := "Starting auth and edge..."
		if legacyGateway {
			msg = "Starting auth, edge, and the gateway..."
		}
		steps = append(steps, startStep{
			Message: msg,
			Name:    "auth/edge",
			Args:    appUpArgs(names, legacyGateway),
			Ready:   names,
		})
	}
	for _, r := range rest {
		steps = append(steps, startStep{
			Message: "Starting " + r.Service + "...",
			Name:    r.Service,
			Args:    []string{"up", "-d", "--no-deps", r.Service},
			Ready:   []string{r.Service},
		})
	}
	return steps
}
