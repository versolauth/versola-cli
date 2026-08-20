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
// A state that can't be read at all is deliberately NOT treated as "ask
// anyway": the command that follows loads the same state and reports the real
// problem with a far better message than a confirmation prompt could
// ("nothing has been configured yet -- run configure first"). Only
// ErrNotConfigured is silently tolerated here; any other read error is
// returned, since silently skipping a vps confirmation because state.json
// happened to be unreadable is exactly the wrong failure mode.
func confirmIfVpsState(action func(version string) string) error {
	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return nil
		}
		return err
	}
	if st.Target != "vps" {
		return nil
	}
	return deploy.ConfirmVpsDeploy(action(st.Version))
}
