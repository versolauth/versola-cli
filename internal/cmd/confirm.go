package cmd

import (
	"errors"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// confirmIfVpsState asks for confirmation, describing `action`, but only when
// the deployment currently recorded on this machine targets vps. Used by the
// standalone migrate/up commands, which act on whatever configure last
// prepared rather than on a target named on the command line.
//
// Returns the state it loaded (nil if nothing is configured yet) so callers
// that go on to act on it -- cmd/migrate.go, cmd/up.go -- can hand this same
// value to deploy.Migrate/deploy.Up instead of re-loading state themselves.
// Reading it twice would leave a window, between this confirmation and the
// later call actually reading state again, for a concurrent `configure` to
// finalize a DIFFERENT deployment in -- the decision the operator just
// confirmed (or the "no confirmation needed, this is local" conclusion) would
// then silently apply to whatever state.json says by the time the later call
// gets around to reading it, not to what was actually shown on screen
// (flagged in review). Callers that don't go on to act on the same
// deployment (cmd/configure.go, cmd/bootstrap.go -- both call this only to
// ask about the record being REPLACED, a different deployment than the one
// about to be acted on) are free to discard the returned state.
//
// A state that can't be read at all is deliberately NOT treated as "ask
// anyway": the command that follows loads the same state and reports the real
// problem with a far better message than a confirmation prompt could
// ("nothing has been configured yet -- run configure first"). Only
// ErrNotConfigured is silently tolerated here; any other read error is
// returned, since silently skipping a vps confirmation because state.json
// happened to be unreadable is exactly the wrong failure mode.
func confirmIfVpsState(action func(version string) string) (*state.State, error) {
	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	if st.Target != "vps" {
		return st, nil
	}
	if err := deploy.ConfirmVpsDeploy(action(st.Version)); err != nil {
		return nil, err
	}
	return st, nil
}
