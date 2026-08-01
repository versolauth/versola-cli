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
	"runtime"
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

// DockerMemory checks how much memory the Docker daemon reports having.
// On macOS and Windows (Docker Desktop) this is the memory given to
// Docker's own VM, not the host's total RAM — which is the number that
// actually matters: three JVMs plus Postgres need real room, and Docker
// Desktop's default VM allocation is often well under what's needed.
//
// If the daemon isn't reachable, or its report can't be parsed, this
// check reports OK rather than failing — DockerDaemon() already reports
// an unreachable daemon on its own, and this check has nothing reliable
// to say in that case.
func DockerMemory() Result {
	const name = "Docker memory"
	const minBytes = 4 * 1024 * 1024 * 1024 // ~4 GiB, per the project design doc's estimate

	out, err := run(5*time.Second, "docker", "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return Result{Name: name, OK: true, Detail: "skipped (daemon unreachable)"}
	}
	total, convErr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if convErr != nil {
		return Result{Name: name, OK: true, Detail: "skipped (couldn't read `docker info`)"}
	}

	gib := float64(total) / (1024 * 1024 * 1024)
	if total < minBytes {
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("%.1f GiB — Versola needs ~4 GiB (three JVMs + Postgres); raise Docker's memory limit", gib),
		}
	}
	return Result{Name: name, OK: true, Detail: fmt.Sprintf("%.1f GiB", gib)}
}

// DiskSpace checks free disk space at the CLI's current working
// directory, as a rough proxy for whether there's room to pull Versola's
// images (~4 GiB, per the project design doc).
//
// This is an approximation, not an exact check: on macOS and Windows,
// Docker Desktop stores images inside its own VM's virtual disk, which
// is a different number from the host's free space checked here — Docker
// doesn't expose the VM disk's free space through a simple command the
// way it exposes memory through `docker info`. Good enough as a first
// warning; worth revisiting if it turns out to give false confidence in
// practice.
//
// Implementation differs by OS (`df` on Linux/macOS, PowerShell on
// Windows) because there's no single portable way to ask for this. The
// Windows path in particular has not been run — only reasoned through
// against PowerShell's documented behavior — so treat a wrong answer
// there as more likely than in the rest of this package until it's been
// verified on a real Windows machine.
func DiskSpace() Result {
	const name = "Disk space"
	const minBytes = 4 * 1024 * 1024 * 1024 // ~4 GiB

	free, err := freeDiskBytes(".")
	if err != nil {
		return Result{Name: name, OK: true, Detail: "skipped (" + err.Error() + ")"}
	}

	gib := float64(free) / (1024 * 1024 * 1024)
	if free < minBytes {
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("%.1f GiB free — Versola's images need roughly 4 GiB", gib),
		}
	}
	return Result{Name: name, OK: true, Detail: fmt.Sprintf("%.1f GiB free", gib)}
}

func freeDiskBytes(path string) (int64, error) {
	switch runtime.GOOS {
	case "windows":
		// Free space, in bytes, of the drive the given path lives on.
		out, err := run(5*time.Second, "powershell", "-NoProfile", "-Command",
			"(Get-PSDrive -Name (Get-Location).Drive.Name).Free")
		if err != nil {
			return 0, fmt.Errorf("couldn't run PowerShell: %w", err)
		}
		return strconv.ParseInt(strings.TrimSpace(out), 10, 64)

	case "linux", "darwin":
		// -P: POSIX output format, guaranteed one line per filesystem
		// (plain `df` can wrap onto a second line for a long device
		// name, which would break the fixed-column parse below).
		// -k: sizes in 1024-byte blocks, so the arithmetic below is
		// unambiguous regardless of df's platform-dependent default unit.
		out, err := run(5*time.Second, "df", "-Pk", path)
		if err != nil {
			return 0, fmt.Errorf("couldn't run df: %w", err)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) < 2 {
			return 0, fmt.Errorf("unexpected df output")
		}
		// Header: Filesystem 1024-blocks Used Available Capacity Mounted-on
		fields := strings.Fields(lines[len(lines)-1])
		if len(fields) < 4 {
			return 0, fmt.Errorf("unexpected df output")
		}
		kib, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("couldn't parse df output: %w", err)
		}
		return kib * 1024, nil

	default:
		return 0, fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
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
