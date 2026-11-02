package deploy

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
)

// DefaultLogsTail is how many lines of each service `versola logs` shows
// when --tail is not given -- with --follow too: a bare `versola logs` fits
// in a terminal, and `logs -f` on a deployment that has run for weeks starts
// with the recent past rather than replaying all of it before the new lines.
// `--tail all` asks for the whole history.
const DefaultLogsTail = "200"

// LogsOptions are `versola logs`' flags, before they become arguments of
// `docker compose logs`.
type LogsOptions struct {
	// Services are compose services (ResolveLogTargets); nil is all of them.
	Services   []string
	Follow     bool
	Since      string // passed to compose as it is: "10m", "2h", an RFC 3339 time
	Tail       string // a number of lines or "all"; "" is DefaultLogsTail
	Timestamps bool
}

// LogsArgs builds the docker arguments of `docker compose logs` for the
// deployment whose compose.yml is at composePath: through ComposeArgs, so
// replicas.yml and proxy.yml are part of it and the user never needs a path.
//
// With exactly one service the lines go out without compose's "service | "
// prefix, so `versola logs auth-3 | jq` reads the service's own output as it
// wrote it. With several (or all) the prefix is what tells them apart.
//
// Nothing is filtered or added: the output is the services' own, unchanged.
// (A redacting filter would be a promise this command cannot keep -- it does
// not know every service's secrets -- and the README says what that means for
// pasting the output somewhere.)
func LogsArgs(composePath string, o LogsOptions) ([]string, error) {
	tail := o.Tail
	if tail == "" {
		tail = DefaultLogsTail
	}
	if tail != "all" {
		if n, err := strconv.Atoi(tail); err != nil || n < 0 {
			return nil, fmt.Errorf("--tail takes a number of lines or \"all\", not %q", o.Tail)
		}
	}
	rest := []string{"logs", "--tail", tail}
	if o.Follow {
		rest = append(rest, "--follow")
	}
	if o.Since != "" {
		rest = append(rest, "--since", o.Since)
	}
	if o.Timestamps {
		rest = append(rest, "--timestamps")
	}
	if len(o.Services) == 1 {
		rest = append(rest, "--no-log-prefix")
	}
	rest = append(rest, o.Services...)
	return state.ComposeArgs(composePath, rest...), nil
}

// LogsHint is the end of an error about a service that did not come up: the
// command that shows what it said, rather than a `docker logs` with a
// container name the customer would have to know.
func LogsHint(service string) string {
	return fmt.Sprintf("see: `versola logs %s --tail 100`", service)
}

// logsSignalGrace is how long RunLogs waits, after docker exited with an
// error, for the Ctrl-C that may have made it exit. A Ctrl-C reaches docker
// and this process at the same time (docker.Run leaves it in this process
// group), and docker can be gone, its exit status collected, before Go's
// signal goroutine has put the signal on the channel.
var logsSignalGrace = 200 * time.Millisecond

// RunLogs runs `docker compose logs` with args (LogsArgs), its output going
// straight to the terminal. A Ctrl-C or SIGTERM is how a `logs -f` ends --
// the user is done reading, nothing failed -- so once one has arrived, docker's
// exit (130, or a "context canceled" from compose) is not an error.
//
// No state lock is taken: logs only reads, like status, and holding the lock
// for as long as a `logs -f` runs would block every configure, up and
// rollout behind someone watching.
func RunLogs(args []string) error {
	sigs := make(chan os.Signal, 1)
	// syscall.SIGTERM compiles on Windows too, where it is simply never
	// delivered; a Ctrl-C there arrives as os.Interrupt.
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return runLogs(args, docker.Run, sigs)
}

func runLogs(args []string, run func(...string) error, sigs <-chan os.Signal) error {
	err := run(args...)
	select {
	case <-sigs:
		return nil
	default:
	}
	if err == nil {
		return nil
	}
	select {
	case <-sigs:
		return nil
	case <-time.After(logsSignalGrace):
		return fmt.Errorf("docker compose logs failed: %w", err)
	}
}
