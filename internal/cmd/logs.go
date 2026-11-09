package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var logsCmd = &cobra.Command{
	Use:     "logs [service...]",
	GroupID: groupOperate,
	Short:   "Show the deployment's service logs",
	Long: `logs shows what the deployment's services print, local or vps,
without docker commands or container names: "docker compose logs" for the
compose project this machine tracks, replicas, the reverse proxy and OpenBao
included.

A service is one of the deployment's compose services (central, auth, edge,
proxy, openbao, step-ca, postgres when the deployment runs its own, ...) or
a replica as status shows it: "auth" is every replica of auth, "auth-3"
only slot 3 ("auth-1" is slot 1). No service means all of them.

The last 200 lines of each are shown (--tail, also with --follow; "--tail
all" for the whole history). --follow keeps printing until Ctrl-C.

The output is the services' own, unchanged -- mind where you paste it: on
its first start step-ca prints its administrative password.`,
	Example: `  versola logs
  versola logs central --tail 100
  versola logs auth -f
  versola logs proxy --since 10m`,
	RunE: runLogs,
}

var (
	logsFollow     bool
	logsSince      string
	logsTail       string
	logsTimestamps bool

	// Stand-ins for tests: what lists the deployment's services, and what
	// runs docker compose logs.
	logsServices       = deploy.DeploymentServices
	logsRun            = deploy.RunLogs
	logsOpenbaoProject = deploy.OpenbaoLogProject
)

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "keep printing new lines until Ctrl-C")
	logsCmd.Flags().StringVar(&logsSince, "since", "", "only lines since this long ago (10m, 2h) or this time (2026-11-02T10:00:00)")
	logsCmd.Flags().StringVar(&logsTail, "tail", deploy.DefaultLogsTail, `lines to show from the end of each service's log, or "all"`)
	logsCmd.Flags().BoolVarP(&logsTimestamps, "timestamps", "t", false, "show when each line was written")
}

// errNothingDeployed: logs has nothing to show, and unlike status (which
// answers "nothing" as a fact) the user asked for something that is not
// there, so it is an error.
var errNothingDeployed = errors.New("nothing is deployed on this machine -- run `versola bootstrap <local|vps> <version>` first")

func runLogs(cmd *cobra.Command, args []string) error {
	st, err := state.Load()
	if err != nil {
		if errors.Is(err, state.ErrNotConfigured) {
			return errNothingDeployed
		}
		return err
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return errNothingDeployed
	}
	// Validate Compose even when no services were named. Its env-file parse
	// errors can repeat secret values; DeploymentServices captures stderr and
	// returns the safe composeConfigError before logs streams either output.
	services, err := logsServices(composePath)
	if err != nil {
		return err
	}
	var targets []string
	if len(args) > 0 {
		if targets, err = deploy.ResolveLogTargets(args, services, st); err != nil {
			return err
		}
	}
	dockerArgs, err := deploy.LogsArgs(composePath, deploy.LogsOptions{
		Services:   targets,
		Follow:     logsFollow,
		Since:      logsSince,
		Tail:       logsTail,
		Timestamps: logsTimestamps,
	})
	if err != nil {
		return err
	}
	if len(targets) == 1 && targets[0] == "openbao" {
		project, err := logsOpenbaoProject(st.Target)
		if err != nil {
			return err
		}
		if project != "" {
			// Configure may have kept this container in an older project.
			// Override only for an explicit request for its own logs.
			dockerArgs = append([]string{"compose", "--project-name", project}, dockerArgs[1:]...)
		}
	}
	return logsRun(dockerArgs)
}
