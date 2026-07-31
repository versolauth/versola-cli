// Package checks implements the individual, read-only checks that
// "versola doctor" runs. Each check only inspects machine state — none
// of them install or change anything. Installing missing dependencies is
// a separate, explicit, user-confirmed step (see the project design doc,
// section 4.3): this package never does it silently.
package checks

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Result is the outcome of a single check.
type Result struct {
	Name   string // human-readable name, e.g. "Docker daemon"
	OK     bool
	Detail string // version string on success, or a hint on failure
}

func (r Result) String() string {
	mark := "[ok]"
	if !r.OK {
		mark = "[!!]"
	}
	if r.Detail == "" {
		return fmt.Sprintf("%s %s", mark, r.Name)
	}
	return fmt.Sprintf("%s %-24s %s", mark, r.Name, r.Detail)
}

// DockerDaemon checks that a Docker daemon is running and reachable —
// not just that the `docker` binary is on PATH. `docker` being on PATH
// while Docker Desktop is stopped is the most common false positive
// here, so this asks the daemon to identify itself rather than checking
// for the binary's existence.
func DockerDaemon() Result {
	out, err := run(5*time.Second, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return Result{
			Name:   "Docker daemon",
			OK:     false,
			Detail: "not reachable — is Docker installed and running?",
		}
	}
	return Result{
		Name:   "Docker daemon",
		OK:     true,
		Detail: "v" + strings.TrimSpace(out),
	}
}

// ComposePlugin checks for the Docker Compose v2 plugin ("docker compose",
// not the standalone v1 "docker-compose" binary). It ships with current
// Docker Desktop and Docker Engine installs, but a bare Docker Engine
// install on Linux sometimes leaves it out.
func ComposePlugin() Result {
	out, err := run(5*time.Second, "docker", "compose", "version", "--short")
	if err != nil {
		return Result{
			Name:   "Docker Compose plugin",
			OK:     false,
			Detail: "not found — install the compose plugin for your Docker install",
		}
	}
	return Result{
		Name:   "Docker Compose plugin",
		OK:     true,
		Detail: "v" + strings.TrimSpace(out),
	}
}

// PortFree checks whether a port is free for Versola's gateway to bind.
// It checks two independent things, because they can disagree:
//
//  1. A raw OS-level bind attempt. Catches non-Docker processes (a local
//     dev server, IIS, etc.) holding the port.
//  2. Docker's own published-port bookkeeping, via `docker ps`. This is
//     necessary in addition to (1) — confirmed by hand while testing this
//     command: on Windows with Docker Desktop's WSL2 backend, a container
//     can hold a published port (docker itself refuses to reuse it, with
//     "port is already allocated") while a plain `net.Listen` on that same
//     port from the host still succeeds. Docker's port forwarding there
//     isn't always a literal OS socket a bind attempt collides with, so
//     relying on (1) alone produced a false "free" against a container
//     that was actually running and holding the port.
//
// A busy port is the most common reason a fresh bootstrap fails on
// someone's machine — see the port 5432 conflict found during the
// project's manual test.
func PortFree(port int) Result {
	name := fmt.Sprintf("Port %d free", port)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return Result{Name: name, OK: false, Detail: "already in use"}
	}
	_ = ln.Close()

	if owner, used := dockerPortInUse(port); used {
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("already used by Docker container %q", owner),
		}
	}

	return Result{Name: name, OK: true}
}

// hostPortMapping matches the host-side port(s) in one entry of `docker
// ps`'s Ports column, e.g. the "8080" in "0.0.0.0:8080->80/tcp" or the
// "8080" and "8081" in "0.0.0.0:8080-8081->8080-8081/tcp" (docker collapses
// consecutive port mappings into a range like this — PORT and DPORT next
// to each other, as Versola's services use, trigger it).
var hostPortMapping = regexp.MustCompile(`:(\d+)(?:-(\d+))?->`)

// dockerPortInUse asks the Docker daemon directly whether a running
// container already has this host port published, rather than relying on
// the OS to notice — see the comment on PortFree for why that's not
// always enough on its own.
func dockerPortInUse(port int) (owner string, used bool) {
	out, err := run(5*time.Second, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	if err != nil {
		// Docker unreachable — DockerDaemon() already reports this on its
		// own, so don't also fail this check because of it.
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		containerName, ports := fields[0], fields[1]
		for _, m := range hostPortMapping.FindAllStringSubmatch(ports, -1) {
			start, _ := strconv.Atoi(m[1])
			end := start
			if m[2] != "" {
				end, _ = strconv.Atoi(m[2])
			}
			if port >= start && port <= end {
				return containerName, true
			}
		}
	}
	return "", false
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s timed out after %s", name, timeout)
	}
	return string(out), err
}
