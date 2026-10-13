# versola-cli

Command-line tool for deploying and managing Versola. Deliberately thin: it
checks the local machine and talks to Docker, but doesn't know anything
about Versola's own service topology (ports, service names, config schema)
— that lives in the separately-versioned `versola-tools` image, which
`bootstrap` pulls and runs. See the project design doc for why.

## Status

`doctor`, `bootstrap`, `configure`, `migrate`, `up`, `status`, `down`,
`secrets`, `uninstall`, and `version` are all implemented and have been
tested end-to-end against a real local deployment (Postgres + auth +
central + edge + the reverse proxy, real browser login through to the admin
console). Releases are automated (see Releasing below) and installed via
the one-line scripts below.

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
curl -fsSL https://raw.githubusercontent.com/versolauth/versola-cli/main/install.sh | sh -s 0.2.6
```
```powershell
$env:VERSOLA_VERSION = "0.2.6"
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
- Port 2821 free on localhost (`local` only — for `vps`, `configure`
  itself checks the ports its reverse proxy needs, which depend on
  `--proxy` and `--auth-url`: see "Deploying to a VPS" below)
- Enough free memory (differs by target, see below)
- Enough free disk space (~4 GiB, for pulling Versola's images)

Pass `--target local` (the default) or `--target vps` to match what
`configure <target>` will actually check — the two targets need genuinely
different memory checks, not just a different number:
- `local` checks Docker's own total memory (`docker info`) against ~4 GiB
  — three JVMs plus local's own containerized Postgres. On Docker
  Desktop (macOS/Windows) this is the VM's allocation, a budget nothing
  else competes with; on native Linux it's the host's own physical RAM.
- `vps` checks `/proc/meminfo`'s MemAvailable instead — vps's Postgres is
  a native, non-Docker install, so total physical RAM isn't the right
  number there (native Postgres and the rest of the host OS already
  compete for it). The minimum is ~1 GiB if `versola-central`/`auth`/
  `edge` are already running (an in-place redeploy roughly nets out) or
  ~3 GiB if none of them are yet (starting three JVMs from cold, closer
  to local's own estimate minus the containerized Postgres vps doesn't
  have), plus a bit more if OpenBao also isn't running yet. This check
  only works run directly on the VPS itself over SSH — it fails outright
  (not a silent skip) if the Docker CLI is pointed at a remote host
  (`DOCKER_HOST`, or a non-local context) or the process isn't on Linux,
  rather than quietly checking the wrong machine's memory.

Exits non-zero if any check fails. `bootstrap`/`configure` run the same
checks themselves before doing anything, so running `doctor` first is
optional but gives you the same picture ahead of time.

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
Postgres, central, auth, edge, and versola-cli's own reverse proxy (the
official nginx image, serving the admin console too, on
`127.0.0.1:2821`) via Docker Compose, waiting on readiness at each stage
-- the proxy until auth, edge and the admin console all answer through
it. Prints the admin login on success.

The proxy is published on `127.0.0.1` only, so the deployment isn't
reachable from other machines on the network. Versola releases whose
`versola-tools` doesn't ship the admin console can't be deployed this way
(`configure` says so) -- pick a newer version. If a local deployment made
by an older versola-cli still runs its own gateway (`versola-nginx`) on
that port, `configure` reports the port as taken: remove that container
first (`docker rm -f versola-nginx`).

`<version>` is a Versola release, which is tagged **without** a leading
`v` — so `versola bootstrap local 0.1.2`, not `v0.1.2`. (versola-cli's own
releases use the same format since 0.2.6; older ones were `v0.2.5` and
earlier.) Passing a version
that was never published fails with a clear error explaining that,
instead of Docker's raw `manifest unknown`; the available versions are
the tags published for the `versola-tools` package under the
organization's GitHub packages.

Once auth and edge are ready, this opens the admin console in your
default browser automatically. Pass `--no-browser` to skip that (e.g.
running headless over SSH).

Internally this runs `configure`, `migrate`, and `up` back to back —
see below for when to run those separately instead.

### `configure <target> <version>` / `migrate` / `up`

The same three steps `bootstrap` runs together, available individually
for when a deployment needs to stop between them — which on a server it
does, because "change the database schema" should be a decision someone
makes on purpose rather than a side effect of starting a service.

```
versola configure local 0.3.0
versola migrate
versola up
```

- **`configure <target> <version>`** — checks the machine, generates this
  release's config via `versola-tools`, and resolves its secrets against
  OpenBao (starting OpenBao itself first if it isn't already running).
  Starts none of Versola's own services, touches no database. For `vps`
  it also needs `--auth-url` and `--postgres-host` (see `bootstrap --help`).
- **`migrate`** — applies each service's own migrations (central, auth
  and edge each own their schema) against whatever `configure` last
  prepared, using one throwaway container that ships all three services'
  migrations and exits when done. No server starts. Records the time in
  `~/.versola`, which `status` then shows.
- **`up`** — starts the stack and waits until it's actually serving,
  not merely started. Assumes `migrate` already ran: services validate
  their schema at startup and refuse to start against an out-of-date
  one rather than quietly migrating it themselves.

Run against a `vps` deployment, each of the three asks for confirmation
first, describing what that particular step is about to do. Migrations
cannot be rolled back — back the database up before running `migrate`
against a live one.

### Secrets: `secrets plan`, `--generate-missing`, `--allow-legacy-secrets`

`configure` (and so `bootstrap`) keeps Versola's secrets in OpenBao, under
`secret/data/versola/<target>/<service>`. Each release ships a
`secrets.schema.json` that lists every secret, which services use it, and
what to do when it is missing: generate it, generate it **only on the first
install**, or expect you to provide it (`external`). Keys that make data
unreadable if replaced (JWT signing keys, the edge key, the utils client
key) are the "first install only" kind: once a deployment is running, the CLI
never invents a new one for you.

Before it writes anything, `configure` builds a plan from three facts: what
the schema says, what OpenBao holds now, and which schema revision this
machine last deployed (recorded in `~/.versola`). If the plan has a problem,
**nothing is written**.

| State | Meaning | What `configure` does |
|---|---|---|
| `first-install` | no record here, OpenBao empty | generates everything the schema asks for |
| `upgrade` | deployed before, OpenBao filled | keeps what is there; generates only secrets new in this release |
| `alarm` | deployed before, OpenBao **empty** | stops: the secrets look lost |
| `installing` | an earlier install here did not finish | continues it (a half-written key group is generated again as a whole) |
| `unknown` | no record here, OpenBao not empty | stops: it cannot tell a new secret from a lost one |
| `local` | `local` target (throwaway) | generates anything missing |

Versola's secret **values are never printed** by `configure`, `bootstrap`, or
`secrets plan` — only names and decisions. The one thing `configure` does
print is the AppRole `role-id`/`secret-id` of OpenBao itself (also saved in
`~/.versola/openbao/<target>.json`), by the team lead's decision.

#### `versola secrets plan <target> <version>`

Shows what `configure` would do, without doing it. Read-only: it reads
OpenBao, runs `versola-tools` into a temporary directory (deleted
afterwards), and prints one line per secret: `keep`, `create`, or a stop.
Run it before upgrading a production deployment.

```
versola secrets login vps        # once: AppRole credentials
versola secrets plan vps 0.7.0
versola secrets plan vps 0.7.0 --json
```

- Needs the AppRole credentials, Docker, and the `versola-tools` image.
- Progress goes to stderr, the report to stdout.
- Exit code is non-zero when the plan has problems
  (`the plan has N problem(s): nothing would be applied`).
- `--json` prints the same report for scripts:
  `{version, ok, target, state, schemaRevision, deployedRevision?, actions[], problems[]}`.
  Actions are `{service, name, action, source?, reason?}`; problems are
  `{kind, name?, service?, services?, group?, present?, missing?}`.
  It never contains a value.
- A release that predates the schema (no `secrets.schema.json`) cannot be
  planned; the command says so.

Problems that mean a broken schema or release rather than a decision for you
(`shared-conflict`, `no-candidate`, `candidate-conflict`, `unknown-secret`,
`invalid-schema`) are reported the same way; they usually need a fixed release.

Typical problems:

| Problem | Meaning | What to do |
|---|---|---|
| `lost` | a first-install-only secret is missing though the deployment exists | restore it from a backup; a new one would make existing data unreadable |
| `group-partial` | part of a key group (e.g. a JWT key pair) is present | restore the missing half, or use `--generate-missing` if the data is expendable |
| `vault-empty-with-state` | this machine has a record, OpenBao is empty | check that you point at the right OpenBao; restore it |
| `unknown-state` | OpenBao has secrets, this machine has no record | `secrets plan` to inspect; then seed by hand or `--generate-missing` |
| `external-missing` | a secret you must supply is absent | write it to OpenBao yourself |

#### `--generate-missing NAME[,NAME...]`

Available on `configure`, `bootstrap`, and `secrets plan`. Allows the named
secrets to be generated although the plan would stop for them (one that
looks lost, or an install of unknown origin). A key group is generated only
if **every** member is named. Use it when you know the old value is gone
for good, never as a routine flag.

```
versola configure vps 0.7.0 --auth-url https://id.example.com \
  --postgres-host 127.0.0.1:5432 --generate-missing <NAME>,<NAME>
```

`<NAME>` is a secret name exactly as `secrets plan` prints it.

#### `--allow-legacy-secrets`

Available on `configure` and `bootstrap`. For a release **without** a secret
schema the CLI falls back to the old rule: anything missing from OpenBao is
generated. That is dangerous when OpenBao already holds secrets (a missing
one may be lost, not new), so the CLI refuses unless you pass this flag.
`local` does not need it, nor does retrying your own unfinished install.

#### Upgrading the CLI

This CLI writes its state file in `~/.versola` in **layout 3** (layout 2
added the schema revision; layout 3 the Postgres host and the records a
rolling upgrade needs). It reads the layouts older CLIs wrote, but an older
CLI cannot read layout 3 and stops with a "newer schema" error: after the
first command that saves state with this CLI, do not go back to an older one
on the same machine.

Deployments made by much older versola-clis -- before `state.json`, before
bundle directories, before the CLI's own reverse proxy, before replicas
(a proxy config without `proxy/conf.d/upstreams.conf`, versola-cli 0.2.7 and
earlier), or -- on `vps` -- before the proxy's config generations (see
"Deploying to a VPS: the reverse proxy") -- are no longer started or
changed as they are: `up` and
`replica` stop with "this deployment was made by an older versola-cli
-- run `versola configure` again". A `configure` (then `migrate` and `up`)
replaces it; the reverse proxy and the services restart once. The very
oldest records (no `state.json`, or no bundle directory -- local
deployments of the first versola-cli releases) can't be read at all, so
`status`, `down`, `migrate` and `uninstall` stop the same way; stop their
containers with `docker compose -f ~/.versola/active/compose.yml down`
and remove `~/.versola/active` by hand, or configure again.

### Deploying to a VPS: the reverse proxy

On `vps`, Versola is exposed through its own reverse proxy: the official
`nginx` image (not a Versola one), with its whole configuration generated
by this CLI. `configure vps` writes it into the deployment next to
versola-tools' files; `up` starts it last, once auth and edge are ready,
and waits until a request through it actually reaches auth. It also
serves the admin console at `/central/admin/`.

`--proxy` picks how it's exposed:

- **`nginx`** (default) — the proxy owns the server's ports 80 and 443.
  With an `https://` `--auth-url` it gets and renews a Let's Encrypt
  certificate by itself (nginx's own ACME module, no certbot). What the
  server needs:
  - the domain's DNS A (and, if you have IPv6, AAAA) record pointing at
    this server;
  - ports 80 and 443 free and reachable from the internet — `configure`
    fails early, naming the port, if something (typically a web server
    already installed) is using one;
  - `--auth-url` exactly `https://<domain>`: no port, path or IP address.

  ```
  versola bootstrap vps 0.6.0 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432
  ```

  For a test deployment, add `--acme-staging`: the certificate then comes
  from Let's Encrypt's staging environment — untrusted by browsers, but
  without production's rate limits (a handful of certificates per domain
  per week).

- **`external`** — this server already runs its own web server on 80/443
  (other sites, its own TLS). Versola's proxy then listens only on
  `127.0.0.1:2821`, plain HTTP, and your web server forwards the domain
  to it and keeps handling TLS itself. It must pass the Host header and
  set `X-Forwarded-For` (and `X-Forwarded-Proto`), e.g. for nginx:

  ```nginx
  location / {
      proxy_pass http://127.0.0.1:2821;
      proxy_set_header Host $http_host;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
      proxy_set_header X-Forwarded-Proto $scheme;
  }
  ```

  ```
  versola bootstrap vps 0.6.0 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432 --proxy external
  ```

  The forwarding web server has to run on this same host (natively, or
  in a container with host networking): 127.0.0.1:2821 isn't reachable
  from anywhere else.

The proxy is part of the same Docker Compose project as the rest of the
deployment, so `status`, `down` and `uninstall` include it. Its
certificates live in the `versola-acme-vps` Docker volume, which
survives `down --volumes` and `uninstall`, so a redeploy doesn't request
a new certificate.

The proxy also stands in front of auth's two internal listeners, so that
every auth replica is reached, not just the first: it listens on
`127.0.0.1:8082` (auth's APORT, which edge and central call) and
`127.0.0.1:8083` (auth's mutual-TLS listener, MPORT, which edge calls --
passed through as TCP, TLS stays between edge and auth), and balances both
over the replicas. The replicas themselves listen 1000 higher: the first
on 9082/9083 (set in `overrides.yml` next to the compose file), the next
on 9182/9183, and so on. Those numbers follow the compose file's APORT and
MPORT; versola-tools' configs are unchanged.

Its configuration lives outside the deployment bundles, in
`~/.versola/active/proxy-vps/`, as generations (`gen-000001/`, ...) with
`current` pointing at the one in use. `up` and `replica add|remove` build
a new generation, have nginx check it in a throwaway container, switch
`current` and reload nginx -- the container is not restarted, so requests
in flight finish. Compose recreates it only when its definition changes:
once, the first time a deployment moves to this layout, and after that for
a new proxy image or a change of `--proxy` mode or TLS.

A reload ends the TCP connections passed through to auth's MPORT once the
old nginx workers stop (after up to 30 seconds), and a connection idle for
an hour is closed; edge reconnects. A replica added later gets new
connections only.

This needs a Versola release whose versola-tools ships the admin console;
`configure vps` says so clearly when pointed at an older one.

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

On a `vps` deployment, OpenBao's data volume and the TLS certificate
volume (`versola-acme-vps`) are left in place; `uninstall` prints the
`docker volume rm` commands if you really mean to discard them.

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

Releases are automated: pushing a version tag — no leading `v`, same as
Versola's own releases — triggers
`.github/workflows/release.yml`, which cross-compiles all four platform
binaries from that commit, generates `checksums.txt`, and publishes them
as GitHub Release assets.

```bash
git tag 0.2.6
git push origin 0.2.6
```

Nothing is built or uploaded by hand: the tag is the only input, and the
version it names is compiled into the binaries via ldflags, so
`versola version` can't disagree with the release it came from.

## Layout

```
cmd/versola/            entry point
internal/cmd/           cobra commands (doctor, bootstrap, configure, migrate, up, status, down, secrets, uninstall, upgrade, version)
internal/checks/        the actual check logic doctor (and configure) run
internal/deploy/        the deployment steps themselves — configure, migrate, up — which bootstrap chains together
internal/state/         locates ~/.versola/active, the on-disk state configure writes
internal/wait/          polls a readiness endpoint until it answers 200 or times out
internal/browser/       opens a URL in the default browser — used by bootstrap (admin console) and doctor (install pages)
install.sh              one-line installer for macOS/Linux
install.ps1             one-line installer for Windows
.github/workflows/      ci.yml (vet/build/test on every push+PR), release.yml (builds binaries + checksums on tag push)
```
