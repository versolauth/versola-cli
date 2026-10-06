package cmd

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var replicaCmd = &cobra.Command{
	Use:   "replica",
	Short: "Add or remove replicas of auth or edge",
	Long: `replica runs more than one copy of auth or edge behind the reverse
proxy, which balances requests between them. central is not replicated.

The replicas a deployment has are recorded, so "versola up" brings back
the same ones, and "versola status" lists them.`,
}

var replicaAddCmd = &cobra.Command{
	Use:   "add <auth|edge> [count]",
	Short: "Start more replicas of auth or edge",
	Long: `add starts count more replicas (1 if omitted) of auth or edge, one at a
time, in the lowest free slots (up to 9 in all). Each one is waited for
until it is ready, and only then does the reverse proxy start sending it
requests. A replica that does not come up is removed again; those started
before it stay.

Needs a running deployment ("versola up").`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReplica(args, "add", deploy.AddReplicas, func(service string, n int, version string) string {
			return fmt.Sprintf("start %d more %s replica(s) of Versola %s on the VPS", n, service, version)
		})
	},
}

var replicaRemoveCmd = &cobra.Command{
	Use:   "remove <auth|edge> [count]",
	Short: "Stop replicas of auth or edge",
	Long: `remove stops count replicas (1 if omitted) of auth or edge, the highest
slots first, one at a time. The reverse proxy stops sending a replica
requests first and waits for the ones it is answering to finish; only then
is the replica stopped. The last replica of a service cannot be removed.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReplica(args, "remove", deploy.RemoveReplicas, func(service string, n int, version string) string {
			return fmt.Sprintf("stop %d %s replica(s) of Versola %s on the VPS", n, service, version)
		})
	},
}

func init() {
	replicaCmd.AddCommand(replicaAddCmd)
	replicaCmd.AddCommand(replicaRemoveCmd)
}

// parseReplicaArgs reads "<service> [count]".
func parseReplicaArgs(args []string) (service string, count int, err error) {
	service, count = args[0], 1
	if !deploy.Scalable(service) {
		return "", 0, fmt.Errorf("%q can't have replicas: only %s and %s can", service, deploy.AuthService, deploy.EdgeService)
	}
	if len(args) == 2 {
		if count, err = strconv.Atoi(args[1]); err != nil || count < 1 {
			return "", 0, fmt.Errorf("the number of replicas must be a whole number of at least 1, got %q", args[1])
		}
	}
	return service, count, nil
}

func runReplica(args []string, verb string, run func(*state.State, string, int) error, action func(service string, n int, version string) string) error {
	service, count, err := parseReplicaArgs(args)
	if err != nil {
		return err
	}
	// Held for the whole run, like up and migrate (see state.Lock): a
	// concurrent configure would swap the bundle this works in.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := state.Load()
	if err != nil {
		if !errors.Is(err, state.ErrNotConfigured) {
			return err
		}
		st = nil // CheckReplicaRequest says what to run first
	}
	// Refused before anyone is asked to confirm something that was never
	// going to run.
	if err := deploy.CheckReplicaRequest(st, service, count, verb == "add"); err != nil {
		return err
	}
	if st.Target == "vps" {
		if err := deploy.ConfirmVpsDeploy(action(service, count, st.Version)); err != nil {
			return err
		}
	}
	if err := run(st, service, count); err != nil {
		return err
	}
	fmt.Printf("\nDone: %s %d %s replica(s). `versola status` shows them.\n", verb, count, service)
	return nil
}
