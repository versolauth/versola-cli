# versola-cli

Command-line tool for deploying and managing Versola. Deliberately thin: it
checks the local machine and talks to Docker, but doesn't know anything
about Versola's own service topology (ports, service names, config schema)
— that lives in the separately-versioned `versola-tools` image, which
`bootstrap` pulls and runs. See the project design doc for why.

## Status

`doctor`, `bootstrap`, `status`, `down`, `uninstall`, and `version` are all
implemented and have been tested end-to-end against a real local
deployment (Postgres + auth + central + edge + the gateway, real browser
login through to the admin console). Releases are automated (see
Releasing below) and installed via the one-line scripts below.

`upgrade` is newly added and not yet tested against a real published
release — try it after the next release goes out, and expect rough edges
before then.

## Install

**macOS/Linux:**
```bash
curl -fsSL https://raw.githubusercontent.com/versolauth/versola-cli/main/install.sh | sh
```

**Windows:**
```powershell
iwr https://raw.githubusercontent.com/versolauth/versola-cli/main/install.ps1 -useb | iex
```

Both scripts install without needing admin/sudo rights. `install.ps1`
also adds the install directory to your PATH automatically. `install.sh`
installs to `~/.local/bin`, which is already on PATH on most systems —
if it isn't, the script prints the exact line to add to your shell
profile. To pin a specific version instead of the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/versolauth/versola-cli/main/install.sh | sh -s v0.1.0
```
```powershell
$env:VERSOLA_VERSION = "v0.1.0"
iwr https://raw.githubusercontent.com/versolauth/versola-cli/main/install.ps1 -useb | iex
```

On Windows, restart your terminal so it picks up the updated PATH, then
run `versola doctor`. On macOS/Linux, only do this if `install.sh` printed
a warning that `~/.local/bin` wasn't already on PATH.

## Build from source

Only needed if you're contributing to `versola-cli` itself — most users
should use Install above instead. Needs Go installed. Builds are
cross-platform from any machine — you don't need a Mac to produce a macOS
binary, or Windows to produce a Windows one.

```bash
go mod tidy   # fetches cobra, generates go.sum — needs network the first time
```

**Windows:**
```powershell
go build -o versola.exe ./cmd/versola
```

**macOS (Apple Silicon — M1/M2/M3/etc., the common case today):**
```bash
GOOS=darwin GOARCH=arm64 go build -o versola-darwin-arm64 ./cmd/versola
```

**macOS (Intel):**
```bash
GOOS=darwin GOARCH=amd64 go build -o versola-darwin-amd64 ./cmd/versola
```

**Linux:**
```bash
GOOS=linux GOARCH=amd64 go build -o versola-linux-amd64 ./cmd/versola
```

On macOS/Linux, the built binary needs the executable bit set before it'll
run (Windows doesn't need this step):
```bash
chmod +x versola-darwin-arm64
```

## Run

If you installed via the Install section above, `versola` is on your PATH
(unless `install.sh` warned that `~/.local/bin` wasn't on it — in that
case, add the printed line to your shell profile first):
```bash
versola doctor
versola bootstrap local <version>
versola status
versola down
versola uninstall
versola upgrade
```

If you built from source instead, run the binary directly from where you
built it:

**Windows:**
```powershell
.\versola.exe doctor
```

**macOS/Linux:**
```bash
./versola-darwin-arm64 doctor
```
(substitute whichever binary you built above)

### `doctor`

Checks this machine has what Versola needs. It never installs anything on
its own — if Docker isn't found at all, the most it does is offer to open
an install page in your browser, and only after you confirm:
- Docker daemon reachable (not just `docker` on PATH)
- Docker Compose v2 plugin present
- Port 2821 free on localhost
- Docker has enough memory allocated (~4 GiB — three JVMs + Postgres)
- Enough free disk space (~4 GiB, for pulling Versola's images)

Exits non-zero if any check fails. `bootstrap` runs the same checks itself
before doing anything, so running `doctor` first is optional but gives you
the same picture ahead of time.

If Docker isn't reachable, `doctor` tells the two possible reasons apart
and reacts differently: already installed but not running just gets a
"start it" hint (installing something wouldn't help); not installed at
all offers to open a download page — a choice between Docker Desktop and
Colima on macOS, Docker Desktop on Windows, the Docker Engine install
docs on Linux. It only ever opens a browser tab — never installs or
downloads anything itself.

### `bootstrap local <version>`

Deploys a specific released version of Versola locally: pulls the
matching `versola-tools` image to generate config, then brings up
Postgres, central, auth, edge, and the gateway (nginx + central-ui) via
Docker Compose, waiting on readiness at each stage. Prints the admin
login on success.

`<version>` is a Versola release, which is tagged **without** a leading
`v` — so `versola bootstrap local 0.1.2`, not `v0.1.2`. (Don't confuse it
with versola-cli's own releases, which do use `v`.) Passing a version
that was never published fails with a clear error explaining that,
instead of Docker's raw `manifest unknown`; the available versions are
the tags published for the `versola-tools` package under the
organization's GitHub packages.

Once auth and edge are ready, this opens the admin console in your
default browser automatically. Pass `--no-browser` to skip that (e.g.
running headless over SSH).

### `status`

Shows the containers from the last `bootstrap` run and their health
(`docker compose ps` under the hood).

### `down`

Stops the locally deployed stack. Pass `--volumes` to also delete the
Postgres data volume (kept by default, so a later `bootstrap` picks up
the same data).

### `uninstall`

Removes everything versola deployed locally: stops the stack and deletes
its Postgres volume, removes the `versola-*` images that were pulled, and
clears `~/.versola`. Prompts for confirmation first — pass `-y`/`--yes`
to skip that.

If a deployment was recorded but Docker isn't reachable to confirm it's
actually stopped, `~/.versola` is deliberately left in place rather than
cleared — deleting it there would remove the only way to properly stop
that deployment once Docker is reachable again.

Does **not** remove the `versola` binary itself or its PATH entry;
safely deleting a program's own running executable isn't portable across
platforms (Windows won't allow it at all). It prints the binary's path
so you can remove it yourself.

### `upgrade`

Checks the latest versola-cli release on GitHub and, if it's different
from this binary's version, downloads it — verified against that
release's published `checksums.txt`, the same way install.sh/install.ps1
verify their own downloads — and replaces the running executable in
place. Prompts for confirmation first — pass `-y`/`--yes` to skip that.

Deliberately implemented with no new dependencies: it reuses the same
GitHub API endpoint and checksums.txt convention the install scripts
already rely on, rather than pulling in a self-update library for one
command. On Windows, where a running `.exe` can't be overwritten in
place, it moves the current binary aside, puts the new one in its place,
and cleans up the leftover on a later run (the OS won't allow deleting it
while this process is still using it).

Re-running the one-line install command (see Install above) works too;
`upgrade` is just a shortcut for the same thing.

Refuses to touch a `dev` build (one built from source rather than
installed from a release) — there's no version to compare it against,
and no guarantee it's safe to overwrite. Rebuild from source instead.

### `version`

Prints which versola-cli release this binary was built from, plus the Go
version and target platform. `versola --version` does the same thing.

This is the CLI's own version, independent of the Versola version it
deploys — that one is whatever you pass to `bootstrap local <version>`.
Binaries built from source without release flags report `dev`.

## Releasing

Releases are automated: pushing a `v*` tag triggers
`.github/workflows/release.yml`, which cross-compiles all four platform
binaries from that commit, generates `checksums.txt`, and publishes them
as GitHub Release assets.

```bash
git tag v0.2.0
git push origin v0.2.0
```

Nothing is built or uploaded by hand: the tag is the only input, and the
version it names is compiled into the binaries via ldflags, so
`versola version` can't disagree with the release it came from.

## Layout

```
cmd/versola/            entry point
internal/cmd/           cobra commands (doctor, bootstrap, status, down, uninstall, upgrade, version)
internal/checks/        the actual check logic doctor (and bootstrap) run
internal/state/         locates ~/.versola/active, the on-disk state bootstrap writes
internal/wait/          polls a readiness endpoint until it answers 200 or times out
internal/browser/       opens a URL in the default browser — used by bootstrap (admin console) and doctor (install pages)
install.sh              one-line installer for macOS/Linux
install.ps1             one-line installer for Windows
.github/workflows/      ci.yml (vet/build/test on every push+PR), release.yml (builds binaries + checksums on tag push)
```
