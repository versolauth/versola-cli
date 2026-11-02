package deploy

import (
	"fmt"

	"github.com/versolauth/versola-cli/internal/state"
)

// RecordedVps is what the vps deployment this machine tracks was configured
// with, as far as a configure run again without its flags keeps it:
// --auth-url, --proxy, --acme-staging, --setup-openbao and which Postgres.
//
// Only AuthURL, ProxyMode and the Postgres fields are fields of state.json.
// The other two are read the way a rolling upgrade already reads them
// (prepareStaged), so configure and rollout cannot disagree and a deployment
// made before this existed is covered too, with no change to the state
// layout:
//
//   - ACMEStaging from the current bundle's proxy configuration
//     (usesACMEStaging): the certificate directory it was generated with;
//   - OpenBaoByHand from the absence of saved OpenBao admin credentials
//     (openbaoSetUpByHand): an OpenBao this CLI provisioned always has them,
//     one set up with --setup-openbao never does.
type RecordedVps struct {
	AuthURL       string
	ProxyMode     string
	ACMEStaging   bool
	OpenBaoByHand bool
	// PostgresMode and PostgresHost: see state.State. resolvePostgres is what
	// actually applies them; they are here so that they can be shown.
	PostgresMode string
	PostgresHost string
}

// StoredVps returns the settings of the vps deployment this machine tracks,
// nil when there is none: nothing configured, a record of another target, or
// one that cannot be read (configure then asks for the flags, as on a first
// deployment, and stops on the unreadable record by itself).
func StoredVps() *RecordedVps {
	st, err := state.Load()
	if err != nil || st.Target != "vps" {
		return nil
	}
	return recordedVpsOf(st, openbaoSetUpByHand("vps"))
}

func recordedVpsOf(st *state.State, openbaoByHand bool) *RecordedVps {
	r := &RecordedVps{
		AuthURL:       st.AuthURL,
		ProxyMode:     st.ProxyMode,
		OpenBaoByHand: openbaoByHand,
		PostgresMode:  st.PostgresMode,
		PostgresHost:  st.PostgresHost,
	}
	if dir, err := bundleDirOf(st); err == nil {
		r.ACMEStaging = usesACMEStaging(dir)
	}
	return r
}

// PostgresText describes the recorded Postgres in a few words, for the
// settings configure prints before it asks.
func (r *RecordedVps) PostgresText() string {
	switch {
	case r.PostgresMode == PostgresModeBundled:
		return fmt.Sprintf("in a container of this deployment, on %s", BundledPostgresHost)
	case r.PostgresHost != "":
		return "your own, at " + r.PostgresHost
	}
	// A record from before layout 3: resolvePostgres asks for --postgres-host.
	return "not recorded (give --postgres-host)"
}

// VpsSettingsLines are the recorded settings `versola status` prints, one per
// line, for a vps record (nothing for another target). Postgres has its own
// line there (PostgresSummary). OpenBao is asked about only here, not in
// state.Load's callers, because it reads the credentials directory.
func VpsSettingsLines(st *state.State) []string {
	if st == nil || st.Target != "vps" {
		return nil
	}
	r := recordedVpsOf(st, openbaoSetUpByHand("vps"))
	lines := []string{
		"Auth URL: " + r.AuthURL,
		"Proxy: " + r.ProxyMode,
	}
	if r.ACMEStaging {
		lines[1] += " (certificate from Let's Encrypt staging, --acme-staging)"
	}
	if r.OpenBaoByHand {
		lines = append(lines, `OpenBao: set up by hand (--setup-openbao; credentials from "versola secrets login vps")`)
	}
	return lines
}
