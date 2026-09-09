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
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
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
//  1. Docker's own published-port bookkeeping, via `docker ps`. Checked
//     first, and separately from (2) below — confirmed by hand while
//     testing this command: on Windows with Docker Desktop's WSL2 backend,
//     a container can hold a published port (docker itself refuses to
//     reuse it, with "port is already allocated") while a plain
//     `net.Listen` on that same port from the host still succeeds.
//     Docker's port forwarding there isn't always a literal OS socket a
//     bind attempt collides with, so relying on (2) alone produced a
//     false "free" against a container that was actually running and
//     holding the port.
//  2. A raw OS-level bind attempt. Catches non-Docker processes (a local
//     dev server, IIS, etc.) holding the port.
//
// ownContainer names the one Docker container it's fine for this port to
// already belong to — the compose file's fixed project name (see
// compose.fragment.yml.template's `name:`) means Up will update or
// restart that container in place on a second `configure`/`up`, not
// conflict with it, so finding it here isn't a real problem the way any
// other owner is. Without this, a redeploy while the previous one was
// still running would fail this check even though it would have worked
// fine.
//
// A busy port held by something else is the most common reason a fresh
// bootstrap fails on someone's machine — see the port 5432 conflict found
// during the project's manual test.
func PortFree(port int, ownContainer string) Result {
	name := fmt.Sprintf("Port %d free", port)

	owner, used := dockerPortInUse(port)
	if used && owner != ownContainer {
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("already used by Docker container %q", owner),
		}
	}

	// Still attempt the raw bind even when Docker's own bookkeeping says
	// our own container holds this port (used && owner == ownContainer),
	// but only on Windows: Docker Desktop's WSL2 backend can leave `docker
	// ps` still reporting a container's port as published after a backend
	// restart, while the WSL2-side forwarding process behind it has
	// actually died — the OS-level port is genuinely free again at that
	// point, and another host process can grab it before Docker restores
	// the forward on the next compose up/restart.
	//
	// This is safe to interpret as "something else must be squatting on
	// it" specifically on Windows/WSL2, where a live forward normally lets
	// a plain net.Listen from the host succeed anyway (confirmed by hand).
	// On native Docker (Linux, and Docker Desktop for Mac's vpnkit, which
	// doesn't share WSL2's failure mode), Compose's own publish mechanism
	// routinely DOES hold the actual OS-level port itself for a running
	// container — every ordinary redeploy of our own, healthy nginx would
	// then fail this raw bind and get misreported as a conflict, which is
	// worse than the rare WSL2 case this exists to catch.
	if used && owner == ownContainer && runtime.GOOS != "windows" {
		return Result{Name: name, OK: true}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if used {
			return Result{
				Name:   name,
				OK:     false,
				Detail: fmt.Sprintf("already in use — Docker still reports %q as the owner, but something else is holding the port at the OS level (possibly stale after a Docker Desktop/WSL2 restart)", ownContainer),
			}
		}
		return Result{Name: name, OK: false, Detail: "already in use"}
	}
	_ = ln.Close()

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

// DockerMemory checks whether this machine has enough memory for
// Versola's containers. "local" and "vps" need genuinely different
// measurements, not just a different minimum against the same number --
// an earlier version of this tried exactly that (one flat total-RAM
// minimum, lowered for vps to account for one fewer container) and got it
// wrong (flagged in review): total physical RAM on a real VPS also has to
// cover vps's native, non-Docker Postgres (see
// compose.fragment.vps.yml.template's own comment) and the rest of the
// host OS, none of which show up as a container to subtract -- a lower
// TOTAL minimum can pass a box where those already eat most of what
// "total" reports, which is exactly the failure mode this check exists to
// catch.
//
//   - local: `docker info`'s MemTotal is Docker Desktop's own VM
//     allocation on macOS/Windows -- a budget set aside specifically for
//     Docker, with nothing else competing for it at that layer. Total is
//     an accurate proxy for "room these containers will have" there, and
//     the ~4 GiB minimum (three JVMs + local's own containerized Postgres,
//     see compose.fragment.yml.template) is unchanged.
//   - vps: reads /proc/meminfo's MemAvailable instead -- the kernel's own
//     "how much could a new process actually get right now" estimate,
//     already netting out whatever native Postgres, the OS, and anything
//     else already running on the box (this check's own history includes
//     a real production VPS running a whole separate observability stack
//     alongside Versola) currently hold. This only works run directly on
//     the VPS itself, which is also the only place `configure vps` is
//     ever meant to run (see develop.md) -- skipped, not failed, on any
//     other OS. The minimum itself further splits on whether central/
//     auth/edge are already running (see dockerMemoryAvailableVps's own
//     comment): available-right-now only means something once you know
//     whether the workload it has to hold is about to grow from zero or
//     just swap in place.
//
// Neither compose template sets a per-service `mem_limit` today, so even
// this remains a proxy, not a guarantee -- the real fix (mem_limit on
// central/auth/edge in compose.fragment.vps.yml.template, so an
// unconstrained JVM's -XX:MaxRAMPercentage=75.0 has a real ceiling to
// work against instead of whatever's currently free) belongs in the
// versola repo, follow-up.
func DockerMemory(target string) Result {
	if target == "vps" {
		return dockerMemoryAvailableVps()
	}
	return dockerMemoryTotalLocal()
}

func dockerMemoryTotalLocal() Result {
	const name = "Docker memory"
	const minBytes = 4 * 1024 * 1024 * 1024 // ~4 GiB: three JVMs + a containerized Postgres, per the project design doc's estimate.

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
		// Docker Desktop is the common case this hint fits, but not the
		// only one: `local` runs on native Linux too (bare Docker Engine,
		// no VM), where MemTotal is the host's own physical RAM the same
		// way it is for vps below -- there's no "Docker Desktop memory
		// limit" setting to point someone at there, only more RAM on the
		// machine itself (flagged in review: this used to say "raise
		// Docker Desktop's memory limit" unconditionally, including on
		// Linux, where that control doesn't exist).
		hint := "raise Docker Desktop's memory limit"
		if runtime.GOOS == "linux" {
			hint = "this machine's own RAM, not a Docker VM limit to raise"
		}
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("%.1f GiB — Versola needs ~4 GiB (three JVMs + Postgres); %s", gib, hint),
		}
	}
	return Result{Name: name, OK: true, Detail: fmt.Sprintf("%.1f GiB", gib)}
}

func dockerMemoryAvailableVps() Result {
	const name = "Docker memory"
	// Two different floors, not one: MemAvailable right now only tells you
	// what's free BEFORE `configure`/`up` starts anything. On a machine
	// already running central/auth/edge (the in-place replace this check's
	// own history was measured against -- see below), that number already
	// has the old containers' footprint baked in, and the new ones roughly
	// swap in for them rather than adding on top. On a genuinely fresh vps
	// (nothing running yet), the exact same "1 GiB free" would say nothing
	// about whether three JVMs that don't exist yet will actually fit once
	// they start -- a fresh 2 GiB box with 1.2 GiB idle would pass this
	// check and then overcommit the moment `up` actually starts them
	// (flagged in review). isReplaceVps below tells the two cases apart.
	//
	// minAvailableReplaceBytes (~1 GiB) is deliberately well under the
	// ~1.8 GiB MemAvailable this check's own history measured on a real,
	// already-stable production VPS (3.9 GiB total, native Postgres + a
	// separate observability stack + this exact three-JVM deployment all
	// already running) -- margin for smaller boxes without chasing an
	// exact number nothing here can derive precisely.
	//
	// minAvailableFreshBytes (~3 GiB) has no equivalent real-world
	// measurement to lean on -- nothing has been observed starting three
	// unconstrained JVMs from cold on a vps box yet -- so it falls back to
	// local's own ~4 GiB estimate for "three JVMs" minus a rough GiB for
	// the one thing vps genuinely doesn't containerize (Postgres), same
	// reasoning as the very first version of this fix, just applied to
	// the right measurement this time (available, not total).
	const minAvailableReplaceBytes = 1 * 1024 * 1024 * 1024
	const minAvailableFreshBytes = 3 * 1024 * 1024 * 1024
	// Configure starts versola-openbao-<target> itself whenever it isn't
	// already running (see deploy.Configure's own "Starting OpenBao..."
	// step) -- independently of whether central/auth/edge are being
	// replaced or started fresh. Our own real first-ever `configure vps`
	// against this exact repo's target VPS is exactly that combination:
	// all three JVMs already up (the cheap "replace" floor), OpenBao not
	// running yet at all (flagged in review: the replace floor as written
	// didn't budget for it). OpenBao is a small Go binary, not a JVM, so
	// this is a flat, deliberately generous-for-its-size add-on rather
	// than a third tier of floors -- added whenever it isn't already up,
	// on top of whichever of the two floors above otherwise applies.
	const openbaoOverheadBytes = 256 * 1024 * 1024

	// configure vps is only ever meant to run directly on the VPS itself
	// (see develop.md's "the machine that will run `versola configure
	// vps`... the VPS itself") -- Configure() itself doesn't enforce that,
	// though, and Docker's own remote-daemon support (DOCKER_HOST, or a
	// context whose endpoint isn't a local socket, pointed at the VPS)
	// would let someone actually reach that far while this check reads
	// /proc/meminfo on whatever machine the CLI process itself is
	// running on -- not necessarily the daemon's. A Linux workstation
	// with ample RAM pointed at a memory-starved remote VPS would pass
	// runtime.GOOS == "linux" and still be checking the wrong machine's
	// memory entirely (flagged in review: an earlier version only ever
	// checked GOOS, which only catches macOS/Windows). Failing loudly on
	// EITHER signal, not skipping as OK, is deliberate: unlike a
	// genuinely inconclusive read (daemon unreachable, docker info
	// unparseable -- both skip as OK below), this is the single
	// highest-stakes case this check exists for (a real remote production
	// deploy) getting silently zero protection otherwise.
	if runtime.GOOS != "linux" {
		return Result{
			Name:   name,
			OK:     false,
			Detail: "can't verify free memory from " + runtime.GOOS + " against a remote vps Docker daemon -- run `versola configure vps` directly on the VPS itself (over SSH), or check `free -m` there by hand first",
		}
	}
	if remote, endpoint := dockerTargetsRemoteHost(); remote {
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("Docker is pointed at %q, not this machine's own daemon -- /proc/meminfo here would check the wrong host's memory. Run `versola configure vps` directly on the VPS itself (over SSH), or check `free -m` there by hand first", endpoint),
		}
	}

	available, err := linuxMemAvailable()
	if err != nil {
		return Result{Name: name, OK: true, Detail: "skipped (couldn't read /proc/meminfo)"}
	}

	minBytes := int64(minAvailableFreshBytes)
	situation := "a fresh deployment (no central/auth/edge running yet)"
	if isReplaceVps() {
		minBytes = minAvailableReplaceBytes
		situation = "replacing the already-running central/auth/edge"
	}
	if !isRunningVps("versola-openbao-vps") {
		minBytes += openbaoOverheadBytes
		situation += ", plus starting OpenBao"
	}

	gib := float64(available) / (1024 * 1024 * 1024)
	minGib := float64(minBytes) / (1024 * 1024 * 1024)
	if available < minBytes {
		return Result{
			Name:   name,
			OK:     false,
			Detail: fmt.Sprintf("%.1f GiB available — Versola needs ~%.1f GiB free right now for %s (native Postgres, the OS, and anything else already on this host all compete for the same RAM here, unlike Docker Desktop's dedicated VM)", gib, minGib, situation),
		}
	}
	return Result{Name: name, OK: true, Detail: fmt.Sprintf("%.1f GiB available", gib)}
}

// isReplaceVps reports whether ALL THREE of vps's fixed container names
// (see compose.fragment.vps.yml.template) are already running -- not just
// any one of them. The lower "replace" memory floor this decides between
// (see dockerMemoryAvailableVps's own comment) only holds because the
// upcoming `up` swaps like for like, three JVMs stopping for three JVMs
// starting, roughly memory-neutral. If only central is up -- a previous
// `up` that failed partway, or one service stopped/crashed on its own --
// the next `up` still has to start auth and edge from cold on top of
// whatever's currently free, exactly the fresh-deploy case with no
// existing workload to net out against (flagged in review: an earlier
// version of this treated "any one of the three" as enough to call it a
// replace, which is true for partial state too, and would then pass this
// preflight right before starting the two JVMs that weren't already
// counted in "available").
//
// Best-effort: if `docker ps` itself fails, this returns false (the
// fresh-deploy, higher-floor case) rather than silently assuming the
// lower one, same reasoning as PortFree's own dockerPortInUse falling
// back to "not found" on error, just the opposite default -- this check
// would rather over-demand memory on a `docker ps` hiccup than
// under-demand it right before starting three JVMs.
func isReplaceVps() bool {
	names, err := dockerPsNames()
	if err != nil {
		return false
	}
	for _, want := range []string{"versola-central", "versola-auth", "versola-edge"} {
		if !slices.Contains(names, want) {
			return false
		}
	}
	return true
}

// isRunningVps reports whether a single named container is currently
// running -- used by dockerMemoryAvailableVps for versola-openbao-vps,
// same underlying `docker ps` call and same "assume not running" fallback
// on error as isReplaceVps above (over-demanding memory on a hiccup, not
// under-demanding it).
func isRunningVps(name string) bool {
	names, err := dockerPsNames()
	if err != nil {
		return false
	}
	return slices.Contains(names, name)
}

// dockerPsNames returns the exact set of currently-running container
// names, one per line as `docker ps` itself prints them, split into a
// slice -- isReplaceVps and isRunningVps then check exact membership, not
// a substring match. A substring match on the raw output would treat
// "versola-central-old" (a leftover renamed container, or any other name
// that merely contains the one being looked for) as if "versola-central"
// itself were running, silently picking the cheaper "replace" floor or
// skipping the OpenBao add-on for a container that was never actually
// there (flagged in review).
//
// Not cached across the calls this check makes per run: both are cheap,
// and caching a container list that could change between them (Configure
// starting OpenBao mid-run isn't a real scenario within one doctor/
// configure invocation, but there's no benefit to assuming it never could
// be) buys nothing worth the extra state.
func dockerPsNames() ([]string, error) {
	out, err := run(5*time.Second, "docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// dockerTargetsRemoteHost reports whether the Docker CLI is currently
// pointed at a daemon reached over the network rather than a local
// socket, and the endpoint string it found if so. Two independent
// signals, checked in the same order the Docker CLI itself resolves
// them: $DOCKER_HOST overrides everything if set; otherwise the active
// context's own endpoint (`docker context inspect` with no name argument
// inspects whichever context is current) is what every `docker` command
// actually connects to. A `unix://` (or empty/unset) endpoint is local;
// `tcp://`, `ssh://`, or anything else is not.
//
// Best-effort like this file's other docker-shelling checks: if
// inspecting the context fails for any reason, this reports "not remote"
// rather than blocking the vps memory check on a problem unrelated to
// memory -- DOCKER_HOST is still checked either way, since that needs no
// subprocess call to read.
func dockerTargetsRemoteHost() (remote bool, endpoint string) {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		if !strings.HasPrefix(h, "unix://") {
			return true, h
		}
		return false, ""
	}

	out, err := run(5*time.Second, "docker", "context", "inspect", "--format", `{{(index .Endpoints "docker").Host}}`)
	if err != nil {
		return false, ""
	}
	host := strings.TrimSpace(out)
	if host == "" || strings.HasPrefix(host, "unix://") {
		return false, ""
	}
	return true, host
}

// linuxMemAvailable reads /proc/meminfo's MemAvailable line, in bytes --
// the kernel's own estimate of memory available for a new process without
// swapping, already accounting for reclaimable caches the way a naive
// "free" total wouldn't. See dockerMemoryAvailableVps's caller for why
// this, not docker info's MemTotal, is what vps's check needs.
func linuxMemAvailable() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected /proc/meminfo line: %q", line)
		}
		// /proc/meminfo reports kiB regardless of the "kB" suffix's own
		// wording -- this is the kernel's long-standing (mislabeled but
		// stable) convention, not an actual decimal-kilobyte value.
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("couldn't parse MemAvailable: %w", err)
		}
		return kib * 1024, nil
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
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
