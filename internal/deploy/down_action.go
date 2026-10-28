package deploy

import (
	"fmt"

	"github.com/versolauth/versola-cli/internal/state"
)

// DownAction is the sentence `versola down` shows before stopping a vps
// deployment (ConfirmVpsDeployYes prints "This will <it>."). It names the
// services that stop and what survives: the volumes of Postgres, OpenBao and
// the proxy's certificates are `external` in the compose files, so
// `down --volumes` never removes them. The volumes it does remove are the
// compose-managed ones -- the mutual-TLS CA and certificates, and the
// diagnostics volumes -- described by kind, not by name: the names belong to
// versola-tools' compose file, not to this CLI.
func DownAction(st *state.State, removeVolumes bool) string {
	services := "central, auth, edge, the reverse proxy and OpenBao"
	kept := "The OpenBao and TLS certificate volumes are kept"
	if st.PostgresMode == PostgresModeBundled {
		services = "central, auth, edge, Postgres, the reverse proxy and OpenBao"
		kept = "The Postgres, OpenBao and TLS certificate volumes are kept"
	}
	var what string
	if st.RolloutUnderway() {
		// Version is where the upgrade is going, not what runs (see
		// State.Version), and only `rollout` starts the stack again.
		what = fmt.Sprintf("stop Versola on the VPS in the middle of a rolling upgrade (%s -> %s) -- %s. Afterwards only `versola rollout` (or `rollout --abort`) starts it again, not `versola up`", st.PreviousVersion, st.Version, services)
	} else {
		what = fmt.Sprintf("stop Versola %s on the VPS -- %s", st.Version, services)
		if st.AuthURL != "" {
			what += fmt.Sprintf(". %s stops answering until `versola up`", st.AuthURL)
		}
	}
	if removeVolumes {
		what += ". It also deletes the stack's own volumes: the mutual-TLS CA and certificates, and the diagnostics volumes (heap dumps, GC logs)"
	}
	return what + ". " + kept
}
