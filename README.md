<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/wordmark-white.svg">
    <img alt="Versola" src="docs/brand/wordmark-ink.svg" height="36">
  </picture>
</p>

# versola-cli

Command-line tool for deploying and managing Versola. Deliberately thin: it
checks the local machine and talks to Docker, but doesn't know anything
about Versola's own service topology (ports, service names, config schema)
— that lives in the separately-versioned `versola-tools` image, which
`bootstrap` pulls and runs. See the project design doc for why.

## Status

Commands: `doctor`, `bootstrap`, `versions`, `configure`, `postgres`, `migrate`,
`up`, `status`, `stats`, `logs`, `backup`, `down`, `uninstall`, `rollout`,
`replica`, `upgrade`, `secrets`, `certs`, `edge` and `version`.

What was tested end-to-end:

- a real local deployment (Postgres + auth + central + edge + the reverse
  proxy, real browser login through to the admin console);
- a test VPS with Versola 0.7.1 and an external Postgres: a from-scratch
  install, `down`/`up`, replicas added and removed under load;
- `rollout` (vps) with stand-in images only, not with two real
  Versola releases;
- `upgrade`, moving a production server between published releases.

The default bundled Postgres path on VPS, and the `postgres`, `backup`,
`certs` and `edge` commands, were not part of these tests.
Releases are automated (see Releasing below) and installed via the one-line
scripts below.

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
curl -fsSL https://raw.githubusercontent.com/versolauth/versola-cli/main/install.sh | sh -s 0.3.2
```
```powershell
$env:VERSOLA_VERSION = "0.3.2"
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
versola bootstrap local            # the latest Versola release
versola bootstrap local 0.7.5      # or a version you pick (versola versions lists them)
versola status
versola stats
versola logs
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

### Help: `--help` and `--help-all`

`versola --help` lists the commands grouped in the order a first deployment
uses them. `versola <command> --help` starts with examples and shows the flags
a deployment on one server needs; the flags for remote edges, test registries,
staging certificates, a Postgres elsewhere and recovery are left out of it
(they work the same). `versola <command> --help-all` (or `versola help
<command> --help-all`) shows them too, under "Advanced flags", and every
command that has such flags ends its `--help` with a line saying so. Shell
completion does not offer the advanced flags; type them out. This README
describes every flag either way.

### `doctor`

Checks this machine has what Versola needs. It never installs anything on
its own — if Docker isn't found at all, the most it does is offer to open
an install page in your browser, and only after you confirm:
- Docker daemon reachable (not just `docker` on PATH)
- Docker Compose v2 plugin present
- Port 2821 free on localhost (`local` only)
- With `--target vps`, the checks of what `configure vps` will be given.
  Pass the same flags you will pass it (`--auth-url`, `--proxy`,
  `--postgres-host`, `--registry`); on a server that already has a vps
  deployment, the ones `configure` remembers are taken from it when left
  out, as `configure` takes them: `--auth-url`, `--proxy` and the public
  names, listed first, and the Postgres, shown on its own line. A value `configure` would
  refuse is refused here, before any check runs, and these flags are
  refused with `--target local`:
  - with `--auth-url`: its name resolves, to this server. A name that
    doesn't resolve fails; one that resolves to an address none of this
    server's interfaces has is a warning (right behind NAT or a load
    balancer, wrong otherwise -- with `--proxy nginx` and an https URL,
    Let's Encrypt then issues no certificate), and so is one that resolves
    here and elsewhere too (a stale A or AAAA record)
  - with `--auth-url`: the ports Versola's proxy binds are free, the same
    check as `configure`'s, with the same exception for this deployment's
    own proxy: 80 and 443 with `--proxy nginx` (80 alone for an http URL),
    `127.0.0.1:2821` with `--proxy external` (the default is `nginx`)
  - the Postgres, decided as `configure` decides it (see "Postgres on vps"):
    for the container (no `--postgres-host`, and no deployment here with a
    Postgres of its own), port 5432 is free; for your own, which one it is
    (`postgres init` checks and prepares it after `configure`)
  - your own Postgres (`--postgres-host`, not a loopback address, and `--postgres-ca`
    when its CA is private; without the flag, the CA recorded by `configure` for that
    same host is used): the address answers and the certificate is accepted
    for that name, as the services will check it (`sslmode=verify-full`). A
    `psql` runs in a throwaway `postgres:18-alpine` container; no role or
    password is known before `configure`, so the check ends at the login. A
    wrong CA or host name fails, with the flag that fixes it; an address that
    can't be reached, or Docker unable to run the check, is a warning
  - the image registry (`ghcr.io`, or the host of `--registry`) answers over
    HTTPS: any HTTP answer will do, no answer fails. Docker pulling through
    a proxy set for its daemon is not seen by this check
  - lingering of your user (`loginctl`), when it can be told: off is a
    warning with `sudo loginctl enable-linger $USER`, since the
    certificate-renewal timer `up` installs stops with your last session
    otherwise

  When nothing fails, doctor prints the `bootstrap vps <version>` command with the
  flags given (remembered ones are left to `configure`), followed by a reminder to
  replace `<version>` with a Versola release from `versola versions`.
  It includes `--auth-url https://<your domain>` when there
  is none, neither given nor remembered. Without `--auth-url` the name and
  port checks are skipped, and `doctor` says to pass it. Warnings don't fail `doctor`; their number is printed above it.
- With `--target vps` and `--auth-url`, and any of `--mtls-domain`,
  `--central-mtls-domain`, `--central-domain` (the names you will deploy
  with, see "Public names" below): each name resolves, to this server, and
  443 answers a TLS handshake for it. `--central-url`, when you pass one,
  is checked the same way on its own port. Meant for a server where Versola
  already runs (after `versola up`): before that there is nothing to answer
- With `--target vps`, on a host that already has a deployment: the
  mutual-TLS certificates in the `versola-mtls-vps` volume are not
  expired and not within the last third of their lifetime (a warning
  then; see `certs renew`), and then a list of every certificate with its
  `notAfter` -- the volume's, and what each public name presents --
  soonest to expire first. `status` prints the same list
- Enough free memory (differs by target, see below)
- Enough free disk space (~4 GiB, for pulling Versola's images)

Pass `--target local` (the default) or `--target vps` to match what
`configure <target>` will actually check — the two targets need genuinely
different memory checks, not just a different number:
- `local` checks Docker's own total memory (`docker info`) against ~4 GiB
  — three JVMs plus local's own containerized Postgres. On Docker
  Desktop (macOS/Windows) this is the VM's allocation, a budget nothing
  else competes with; on native Linux it's the host's own physical RAM.
- `vps` checks `/proc/meminfo`'s MemAvailable instead — on a server the
  services share the host's RAM with everything else on it (the Postgres
  container, or a native Postgres, and the OS), so total physical RAM isn't
  the right number there. The minimum is ~1 GiB if `versola-central`/`auth`/
  `edge` are already running (an in-place redeploy roughly nets out) or
  ~3 GiB if none of them are yet (starting three JVMs from cold), plus a
  bit more if OpenBao also isn't running yet, and 256 MiB more when the
  deployment runs the bundled Postgres container (a first install, or one
  recorded as running it) and it is not running yet. That 256 MiB is an
  estimate, to be replaced by the figure the release publishes
  (versolauth/versola#825). This check
  only works run directly on the VPS itself over SSH — it fails outright
  (not a silent skip) if the Docker CLI is pointed at a remote host
  (`DOCKER_HOST`, or a non-local context) or the process isn't on Linux,
  rather than quietly checking the wrong machine's memory.

**`--bundle <file>`** writes one archive for whoever helps you with a failed or
ailing deployment (and exits as `doctor` would: the checks failing is why it is
wanted). `<file>` is a name that does not exist yet, or a directory (it then
makes `versola-diagnostics-<target>-<UTC time>.tar.gz` in it); it never replaces
a file, is mode 0600 and is packed under a temp name (made before anything is gathered, so a directory that cannot be written fails at once) and moved into place at the end without replacing anything.
The target is the one of the deployment on this machine unless `--target` is
given. It holds no password or key (see below for how that is kept), but it identifies the server: it names your domain, the addresses the names resolve to, your database host and user name when the database is your own, the paths under your home directory, and the user you ran it as. It holds:

| Member | What |
|---|---|
| `manifest.json` | CLI version, OS and architecture, time, target, Versola version, size and SHA-256 of every member, what could not be made and why, and what was left out |
| `doctor.txt` | what `doctor` printed |
| `versions.txt` | this CLI, the OS, Docker and Compose (a call that fails is a line saying so) |
| `state.json` | the CLI's record of the deployment (versions, the auth URL and public names, the proxy mode, the Postgres mode and, for your own Postgres, its host and database name; no secret) |
| `status.json`, `stats.json` | `status --json` and `stats --json`, as those commands run: `status` probes the certificates over the network, and `stats` on a vps may start a throw-away `psql` container to count the Postgres connections, which reads the live database |
| `files/compose.yml`, `files/replicas.yml`, `files/proxy.yml`, `files/release.json` | the generated files of the deployment's bundle that hold no secret, a fixed list; whatever looks like a secret in them (a private key block, the rest of a line after a password, secret, token, key or credential name, a bearer token, the password of a URL) is blanked as a second line of defence, a best effort that does not promise to catch everything |

Never in it: the `*.secrets.env` files, OpenBao's data and
`~/.versola/openbao` (credentials and keys), the containers' environment, the
database. A member that cannot be made (Docker is down: the usual reason for
the archive; no deployment yet) is listed in the manifest and the output with
the reason, and the rest is written. A member over 8 MiB is cut there.

**`--logs`** (with `--bundle` only) adds `logs/<service>.log`: the last 200
lines of every service, with timestamps, as `versola logs` shows them. They are
**not redacted**: they can hold addresses, names and ids of your users, and
credentials too (Postgres logs the text of a failed statement, `CREATE ROLE ...
PASSWORD` included; the verifier and the quoted password of such a statement
are blanked, as a best effort, and nothing else is). They are left out by
default for that reason; read them before you send the file. The rest of the
archive is also scrubbed of a password a flag you typed may have carried (a
URL in `--postgres-host`, say) in `doctor.txt`, the record, status and stats.

Exits 3 if any check fails (see "Exit codes"). `bootstrap`/`configure` run the
machine checks themselves before doing anything (Docker, memory, disk,
the proxy's ports, port 5432 for the Postgres container); the name of
`--auth-url`, the registry and lingering are only checked here, so on a
server run `doctor --target vps` with your flags first.

On a server, when one of those checks fails, `configure`/`bootstrap` end with
the `versola doctor --target vps` command for the flags of that run (bare
`versola doctor` checks local). A taken port 5432 for the Postgres container
says the way out instead: stop the Postgres that holds it, or pass
`--postgres-host 127.0.0.1:5432` to use it as your own (on a deployment that already
runs its own Postgres only the first: configure refuses `--postgres-host` there).

If Docker isn't reachable, `doctor` tells the two possible reasons apart
and reacts differently: already installed but not running just gets a
"start it" hint (installing something wouldn't help); not installed at
all offers to open a download page — a choice between Docker Desktop and
Colima on macOS, Docker Desktop on Windows, the Docker Engine install
docs on Linux. It only ever opens a browser tab — never installs or
downloads anything itself.

### `bootstrap local [<version>]`

```
versola bootstrap local
versola bootstrap local 0.7.5
```

`bootstrap vps <version>` does the same on a server, with the flags of
`configure vps` below; it asks for confirmation first. On a first install,
the default bundled Postgres is initialised automatically and bootstrap
continues through `migrate` and `up` (see "Postgres on vps"). Only with your
own Postgres (`--postgres-host`) does it stop before `migrate` -- with a
non-zero exit code -- once it has written the SQL that sets the Postgres
role's password (see Secrets below). Run `versola postgres init` or that
SQL, then `versola migrate` and `versola up`.

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

Without `<version>`, the first line says which version it deploys:

- nothing deployed on this machine yet (or a record of a VPS deployment, or
  one too old to read): the latest Versola release, as GitHub defines it --
  never a pre-release: `Deploying Versola 0.7.5 (the latest release; pass a
  version to pick another).` If GitHub can't be reached (offline, rate
  limit), it stops with nothing done and asks for a version instead of
  guessing one.
- a local deployment already recorded: its version is kept, so running
  `bootstrap local` again (after `down`, or to change a flag) never turns
  into an upgrade on its own: `Keeping Versola 0.7.2 (deployed; 0.7.5 is the
  latest, pass it to move).` The latest is looked up best-effort; offline the
  line just says to pass a version to move.

The version chosen this way is used and recorded exactly as a typed one.
`vps` always needs the version: a server runs only a version someone named.

`<version>` is a Versola release, which is tagged **without** a leading
`v` — so `versola bootstrap local 0.1.2`, not `v0.1.2`. (versola-cli's own
releases use the same format since 0.2.6; older ones were `v0.2.5` and
earlier.) `versola versions` lists them. Passing a version
that was never published fails with a clear error explaining that,
instead of Docker's raw `manifest unknown`. A release's images are
built after the release is published, so for a short while the newest
release has none yet: when `bootstrap local` picked that release itself, it
says so -- retry in a few minutes, or pass the release before it.

On vps, `up` ends with the admin's first login: when auth issued a one-time
link at this start (a release that has them: versolauth/versola#824) it is
printed, once, and nothing stores it -- open it and set the permanent password
there. When there is none (auth is already running and issued its link at an
earlier start, or the release has no links) `up` says which file holds
`ADMIN_BOOTSTRAP_PASSWORD` (the path, never the value) and points at
`versola admin reset-password`.

Once auth and edge are ready, this opens the admin console in your
default browser automatically. Pass `--no-browser` to skip that (e.g.
running headless over SSH).

Internally this runs `configure`, `migrate`, and `up` back to back —
see below for when to run those separately instead. On vps it asks for
confirmation once, up front, before `configure` changes anything, rather
than once per step, and at the end installs the daily timer that renews the
mutual-TLS certificates (on a host without systemd it prints the crontab
line to add instead; see `certs renew`).

### `configure <target> [<version>]` / `migrate` / `up`

The same three steps `bootstrap` runs together, available individually
for when a deployment needs to stop between them — which on a server it
does, because "change the database schema" should be a decision someone
makes on purpose rather than a side effect of starting a service.

```
versola configure local 0.3.0
versola migrate
versola up
```

- **`configure <target> [<version>]`** — checks the machine, generates this
  release's config via `versola-tools`, and resolves its secrets against
  OpenBao (starting OpenBao itself first if it isn't already running).
  Starts none of Versola's own services, touches no database. On `local`
  the version can be left out, with the same rule as `bootstrap local`:
  the deployed version is kept, otherwise the latest release is used. For `vps`
  the version is required, and it also needs `--auth-url` on the first run: the
  public URL the deployment will be reachable at (e.g. `https://id.example.com`).
  It is specific to whoever deploys, so there is no shared default; later runs
  on the same server remember it. `local` doesn't need it (its own defaults suit
  a throwaway dev stack). It accepts
  `--postgres-host` (your own Postgres; without it, Postgres runs in a
  container of the deployment, see "Postgres on vps"),
  `--postgres-db` and `--postgres-ca`, and `--mtls-domain`,
  `--central-mtls-domain` and `--central-domain` (see "Public names").
  `--registry <prefix>` (both targets) pulls the images from another
  registry, see "Images from another registry". On `vps` it remembers the
  deployment it replaces, see "Redeploying on the same server" below.
  On `local` the vps-only flags are refused (exit code 2) rather than
  ignored: `--auth-url` (local always runs at `http://localhost:2821`),
  `--postgres-host`, `--setup-openbao`, `--no-certs-timer`, `--proxy`,
  `--acme-staging`, `--central-url`, `--postgres-db`, `--postgres-ca`, `--est` and
  the public-name flags; `bootstrap local` the same. An unknown target is
  refused before anything is asked. `--registry`, `--generate-missing`,
  `--allow-legacy-secrets` and `--yes` work on both targets.
- **`postgres init`** — for a vps deployment pointed at a Postgres of your
  own (`--postgres-host`): creates the role, the database and the schemas
  `auth`, `central` and `edge` the generated configs name, by the rights it
  finds. See "Your own Postgres: `postgres init`".
- **`migrate`** — applies each service's own migrations (central, auth
  and edge each own their schema) against whatever `configure` last
  prepared, using one throwaway container that ships all three services'
  migrations and exits when done. No server starts. Records the time in
  `~/.versola`, which `status` then shows. `--dry-run` lists the
  migrations that would be applied and applies none; `--service
  auth|central|edge` limits the run to one service.
- **`up`** — starts the stack and waits until it's actually serving,
  not merely started. Assumes `migrate` already ran: services validate
  their schema at startup and refuse to start against an out-of-date
  one rather than quietly migrating it themselves.

Run against a `vps` deployment, each of the three asks for confirmation
first, describing what that particular step is about to do. So does `down`,
which stops the whole stack. `configure` starts and stops nothing, but it
replaces this machine's record of which deployment is the current one, which
is what `status`, `down` and `uninstall` act on. Migrations
cannot be rolled back — back the database up before running `migrate`
against a live one.

The answer is read from stdin one line at a time, never further than the
newline, so a script can feed several answers to consecutive commands
(`printf 'y\ny\n' | { versola configure vps ...; versola migrate; }`).
Pasting several commands into an interactive terminal is different: the shell
has already taken the next line as typed-ahead input, and the prompt reads it
as the answer. To avoid prompts in scripts, pass `-y`/`--yes` to `configure`,
`migrate`, `up`, `down`, `bootstrap` and `replica add|remove`; `rollout`,
`postgres init`, `upgrade` and `uninstall` accept it too. With `--yes`, `configure`, `migrate`,
`up`, `down`, `bootstrap`, `replica` and `rollout` still print the "This will …" line (so a
script's log shows what it agreed to) and then go on without asking.
`local` never asks about its own deployment, so `--yes` changes nothing there.
`uninstall --purge` is not covered either: it asks you to type `vps`, with or
without `--yes` (see `uninstall`).
The other exception it does not cover: `configure local` / `bootstrap local` on a
machine whose record still points at a running VPS deployment ask before
replacing that record, even with `--yes`, because replacing it leaves `status`,
`down` and `uninstall` unable to find the VPS containers. In a script that
question is answered by stdin, or the command aborts.

#### Redeploying on the same server

`configure vps` (and `bootstrap vps`) run again on a server that already has a
vps deployment take `--auth-url`, `--proxy`, `--acme-staging`,
`--setup-openbao` and the Postgres it uses (the container, or your own at its
`--postgres-host`) from that deployment, so a redeploy or a move to another
version without `rollout` is

```
versola configure vps 0.8.1
versola migrate
versola up
```

with no flag to remember. Before it asks to continue it prints what it reused,
marking anything given on the command line:

```
Using the recorded settings of this deployment (pass a flag to change one):
  --auth-url       https://id.example.com
  --proxy          external
  Postgres         your own, at 127.0.0.1:5432
```

A flag always wins over the recorded value: `--proxy nginx` changes the mode
(the port checks then run for the new one), `--acme-staging=false` goes back to
a trusted certificate, `--setup-openbao=false` to automatic OpenBao setup.
Switching between the Postgres container and your own is refused (it is a
backup and a restore, see "Postgres on vps"). A recorded `--acme-staging` is
left out, not refused, when the new `--proxy` or `--auth-url` means Versola's
proxy no longer gets a certificate itself. `--postgres-db`, `--postgres-ca`,
the public names, `--cert-lifetime-days` and `--est` were remembered already;
`--registry` is not (see "Images from another registry"). `versola status`
shows the recorded auth URL, the proxy mode (marked when the certificate comes
from Let's Encrypt staging) and, when it was set up by hand, OpenBao.

**A release that cannot be upgraded in place.** After versola-tools has
written the new version's configs, and before any secret is written or the
record replaced, `configure` (vps and local) reads the release's manifest (`release.json`,
see `rollout`) and compares it with the version recorded here. If the release
lists that version in `freshDatabaseFrom` (0.6.* to 0.7.0, say), `configure`
stops with the release notes and the way on -- a backup, `uninstall --purge`,
`bootstrap` -- instead of letting `migrate` fail later on a Flyway checksum.
`--allow-fresh-database` (an advanced flag, `--help-all`) goes on anyway, for
someone who has already put a new, empty database behind the deployment; it
is refused on `local`. It is for a deployment on your own Postgres (the refusal
names `--postgres-host` and `--postgres-db` for it); with the bundled Postgres
there is no other database to put behind the deployment, so the refusal names
only `uninstall --purge` and `bootstrap`. On `local` the refusal names `uninstall` (a plain one removes its Postgres data) and `bootstrap local`; there is no flag to go on. The flag itself is refused over a
deployment recorded as running the bundled Postgres at once, before the
confirmation question and whatever the release says (leave it out: `configure`
finds out by itself whether the release can be upgraded in place), with the
same way out. Anything else the manifest says, or no manifest at all
(an older release), changes nothing here, and a first deployment has no
version to upgrade from. `bootstrap` has no such flag: it is for a first
deployment, and a refusal there (own Postgres only) tells you to run `configure --allow-fresh-database`, then `migrate` and `up`.

`--auth-url`, `--proxy` and the Postgres are fields of `state.json`. Staging and
"OpenBao set up by hand" are not: they are read the way `rollout` reads them,
from the proxy configuration of the current deployment and from whether
`~/.versola/openbao/vps-admin.json` exists (this CLI writes it whenever it
provisions OpenBao itself), so deployments made by an older CLI are covered
too and the state layout is unchanged. Nothing is reused from a `local`
record, or when there is no readable record: the first `configure vps` needs
`--auth-url` as before.

### OpenBao: automatic setup, `--setup-openbao`, `secrets login` / `secrets test`

`configure` starts an OpenBao container for the target
(`versola-openbao-local` / `versola-openbao-vps`, on `127.0.0.1:8200`) and,
the first time, provisions it itself: init with one unseal key, unseal, KV
v2, AppRole, a policy limited to `secret/data/versola/<target>/*`. It keeps
two files in `~/.versola/openbao/`: `<target>.json` (the AppRole
credentials it reads and writes secrets with) and `<target>-admin.json`
(the root token and unseal key). OpenBao comes back **sealed** after every
restart of the server; the CLI unseals it with that key before it logs
in (see Secrets below), and the next `configure` recreates the container
if it was stopped. **Keep a copy of `<target>-admin.json` off the
machine** -- neither value can be recovered. Versola's services never talk
to OpenBao; it only has to run while `configure` (or `rollout`, `secrets
plan`) does.

If you run OpenBao yourself, pass `--setup-openbao` to the first `configure vps` /
`bootstrap vps` (later runs remember it, see "Redeploying on the same server"): the CLI then never calls OpenBao's admin API, unsealing is
yours, and it expects credentials stored beforehand with

```
versola secrets login vps http://127.0.0.1:8200 <role-id>   # secret-id is a hidden prompt
versola secrets test vps                                    # logs in, writes and reads a scratch value
```

`secrets login` is also how another machine (or one whose
`~/.versola/openbao/<target>.json` is gone) gets access. The manual setup
steps are in the `versola` repository's `develop.md` ("Doing it by hand").

### Images from another registry: `--registry`

`bootstrap`, `configure` and `secrets plan` take `--registry <prefix>`, for
test images pushed somewhere other than Versola's own registry
(`ghcr.io/versolauth`):

```
docker login cr.yandex
versola bootstrap local 0.8.0-test1 --registry cr.yandex/crp1a2b3c
```

The prefix is a registry host (with a port, if any) and a path, lower case,
without a scheme or a trailing slash. The Versola images of this deployment
are then named under it: `<prefix>/versola-tools:<version>`, and — through the compose file
that `versola-tools` generates, which gets the prefix as `IMAGE_REGISTRY` —
`<prefix>/versola-central:<version>`, `-auth`, `-edge`. Third-party images
(OpenBao, Postgres, nginx, step-ca) are not affected, and neither is the
compose file `versola edge bootstrap` writes for a remote edge, which still
names `ghcr.io/versolauth/versola-edge` (a known gap). What the registry must
hold:

- all four images, under the **same tag** you pass as `<version>`;
- a `versola-tools` built with `--build-arg VERSION=<that tag>` (the compose
  file takes the images' tag from the `VERSION` baked into the tools image),
  and from a Versola revision that knows `IMAGE_REGISTRY`.

configure checks the generated compose file before anything is started: if
a `versola-*` image in it is not under the prefix (an older `versola-tools`
ignores the setting and would deploy from ghcr.io), or its tag is not
`<version>`, it stops with an error and says which.

The CLI does not log in to a registry. A refusal by the registry
(`unauthorized`, `pull access denied`) when `versola-tools` is pulled is
reported as such, with `docker login <host>` as the next step (Yandex
Container Registry answers `401 Unauthorized` for a tag that does not exist
as well, so the same message can mean a typo in the version). Pulling
central, auth and edge themselves is done by Docker Compose (`up`, and
`rollout`'s pull step) and reports Compose's own message.

The prefix is recorded in `~/.versola` (`imageRegistry` in `state.json`;
absent means Versola's own registry, which is what every record from before
the flag means, so the state layout did not change). It is what
`versola rollout` (and `--abort`, and `--dry-run`) pull from: a rolling upgrade
cannot change the registry, because the version it would go back to was
pulled from the old one and the same tag elsewhere can be another image. To
change the registry, `configure` again (with or without `--registry`; a
configure without the flag is a deployment from Versola's own registry),
`migrate` and `up` (configure prints a note when the registry changes). `status` shows the registry when it is not the default,
and `uninstall` also removes the images pulled from it.

`secrets plan` takes the flag on its own, like `configure`: it plans what a
`configure` with the same flag would do.

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
| `unknown` | no record here, OpenBao not empty | keeps what is there; stops if a first-install-only secret is missing (it cannot tell a new one from a lost one) |
| `local` | `local` target (throwaway) | generates anything missing |

Secret **values are never printed** by `configure`, `bootstrap`, `rollout`, or
`secrets plan` — only names, decisions and where a value is kept:

- OpenBao's own AppRole `role-id`/`secret-id`: `configure` prints the path,
  `~/.versola/openbao/<target>.json` (readable only by you).
- The Postgres password generated on the first `configure` against a new
  OpenBao, when the Postgres is your own (with the one in a container there
  is nothing to run, see "Postgres on vps"): `configure` writes the SQL that
  sets it on Versola's role to
  `~/.versola/openbao/<target>-postgres-role.sql` (readable only by you; it
  creates the role if it doesn't exist and sets the password and allows login if it does)
  and prints the command to run it:

  ```sh
  sudo -u postgres psql < ~/.versola/openbao/vps-postgres-role.sql
  ```

  The file holds the password's SCRAM-SHA-256 verifier, which Postgres stores
  as is, not the password (the password itself only if it isn't ASCII;
  generated ones are), so it doesn't end up in Postgres' server log either.
  It needs `psql` 10 or newer and a POSIX shell (the server's, not
  PowerShell).

  Use `<`, not `psql -f`: the `postgres` user can't open your file, your
  shell can. For managed or remote Postgres, feed the same file to `psql`
  connected as an admin role. You don't need to know the password itself;
  it is stored in OpenBao and in the deployment's `auth.secrets.env`.

The one exception: on `local` (a throwaway OpenBao), if the CLI can't save
OpenBao's root token and unseal key right after initializing it, it prints
them once so they aren't lost; on `vps` it stops with an error instead.

OpenBao comes back sealed after every restart of the server. Before it logs
in (`configure`, `rollout`, `secrets plan`, `secrets test`, `secrets login`)
the CLI unseals it itself with the key it saved in
`~/.versola/openbao/<target>-admin.json`, waiting up to 30 seconds for the
container to start, and sends that key only to a local address. If you set
OpenBao up yourself (`--setup-openbao`) the CLI has no key: it stops with the
`bao operator unseal` command to run by hand.

#### `versola secrets plan <target> <version>`

Shows what `configure` would do, without doing it. Read-only: it reads
OpenBao, runs `versola-tools` into a temporary directory (deleted
afterwards), and prints one line per secret: `keep`, `create`, or a stop.
Run it before upgrading a production deployment.

```
versola secrets plan vps 0.7.1
versola secrets plan vps 0.7.1 --json
```

- Needs OpenBao running, this machine's AppRole credentials (`configure`
  stores them when it provisions OpenBao; with `--setup-openbao`, `versola
  secrets login`), Docker, and the `versola-tools` image.
- `rollout` builds and checks the same plan for the version it moves to, so
  a secret new in that version is generated before any of it starts.
- Progress goes to stderr, the report to stdout.
- Exit code is 5 when the plan has problems
  (`the plan has N problem(s): nothing would be applied`); `configure`,
  `bootstrap` and `rollout` stop with the same code when their own secrets
  plan has problems.
- `--json` prints the same report for scripts:
  `{version, ok, target, state, schemaRevision, deployedRevision?, actions[], problems[], compatibility?}`.
  On vps, with a recorded version and a release that has a manifest,
  `compatibility` is `{from, rolling, freshDatabase, notes?}`: what that release says about an upgrade from the
  recorded version (`rolling` is `true`, `false` for a fresh-database release, or `null` when it does not say).
  A manifest this versola cannot read is left out here with a note on stderr, not an error: a plan is about secrets (rollout and configure do stop on it).
  Actions are `{service, name, action, source?, reason?}`; problems are
  `{kind, name?, service?, services?, group?, present?, missing?}`.
  It never contains a value. The other machine-readable outputs,
  `status --json` and `stats --json`, are described with their commands.
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
| `unknown-state` | OpenBao has secrets, this machine has no record (on vps, when the plan has no other problem, a `configure` with no `--postgres-host` and no bundled data volume stops with a Postgres-specific message instead: the earlier install most likely used a Postgres of your own, see "Postgres on vps") | starting over after a failed attempt (after a failed first install a plain `uninstall` keeps the note that lets `configure` resume it, so this is rarer than it was): `versola uninstall --purge`, then configure again. A server that ran Versola and lost `~/.versola`: do **not** purge, the keys protect live data; restore the missing secrets, or `--generate-missing` if they are gone for good. The plan prints both, once |
| `external-missing` | a secret you must supply is absent | write it to OpenBao yourself |

#### `--generate-missing NAME[,NAME...]`

Available on `configure`, `bootstrap`, `rollout` and `secrets plan`. Allows the named
secrets to be generated although the plan would stop for them (one that
looks lost, or an install of unknown origin). A key group is generated only
if **every** member is named. Use it when you know the old value is gone
for good, never as a routine flag.

```
versola configure vps 0.7.1 --auth-url https://id.example.com \
  --postgres-host 127.0.0.1:5432 --generate-missing <NAME>,<NAME>
```

`<NAME>` is a secret name exactly as `secrets plan` prints it.

#### `--allow-legacy-secrets`

Available on `configure`, `bootstrap` and `rollout`. For a release **without** a secret
schema the CLI falls back to the old rule: anything missing from OpenBao is
generated. That is dangerous when OpenBao already holds secrets, or this
machine has deployed before (a missing one may be lost, not new), so the CLI
refuses then unless you pass this flag.
`local` does not need it, nor does retrying your own unfinished install.

#### Upgrading the CLI

This CLI writes its state file in `~/.versola` in **layout 4**, or in
**layout 5** when the deployment runs its own Postgres in a container or has
EST on (layout 2 added the schema revision; layout 3 the Postgres host and the
records a rolling upgrade needs; layout 4 the Postgres database name and CA,
from `--postgres-db` and `--postgres-ca`; layout 5 `postgresMode` and
`estEnabled`). A record is in layout 4 unless it carries one of those two: a
CLI that did not know them would read the record, ignore the key and drop it at
its next save, so a deployment with its own Postgres would come back as a
Postgres of your own on the loopback address with no Postgres container in the
compose file it generates, and EST would be off after the next rollout. It
reads the layouts older CLIs wrote, but an older CLI cannot read a layout
newer than its own and stops with a "newer schema" error (`state layout 5,
this one reads up to 4`) before it changes anything: after the first command
that saves state with this CLI, do not go back to an older one on the same
machine. A deployment in layout 5 cannot go back at all (an older CLI would
not know how to run its Postgres, and a copy of `state.json` is in layout 5
too): the error touches nothing in Docker or in `~/.versola`, and installing
the newer release again (the install script with its version; `versola
upgrade` only if the older release's latest stable is the newer one) brings the commands back. For any other deployment, copy
`~/.versola/active/state.json` first if you may need to go back.

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

### Postgres on vps

`--postgres-host` is `host` or `host:port`: a name or an IP, a port from 1 to
65535, an IPv6 address in brackets (`db.example.com`, `10.0.0.5:5432`,
`[::1]:5432`). Anything else -- a connection URL, a user name or password, a
path, a space -- is a usage error (exit code 2) in `configure`, `bootstrap` and
`doctor`, before the lock and any question, and the message never repeats the
value, so a password pasted by mistake is not echoed or recorded.

Without `--postgres-host`, `configure vps` makes Postgres a service of the
deployment: a `postgres` container (the image `versola-tools` pins) on the
host network, listening on `127.0.0.1:5432` only, so port 5432 has to be free
on the server (`configure` checks it; a Postgres already listening there
stops the container from starting). Nothing in this mode is done by hand:

- **No database step.** The container's first start, with an empty data
  directory, creates the role (the user in the generated configs), the
  database (`--postgres-db`, default `auth`) and the schemas `auth`,
  `central` and `edge`, from the password `configure` stored in OpenBao.
  `bootstrap vps` therefore does not stop before `migrate`. A later start
  finds the data directory and initialises nothing.
- **The password** reaches the container in `postgres.secrets.env` (mode
  0600, in the deployment's directory), which holds `POSTGRES_PASSWORD` and
  nothing else of auth's secrets. Docker keeps it in the container's
  configuration, so `docker inspect` shows it to whoever can use Docker on
  the server.
- **The data** is in the Docker volume `versola-postgres-data-vps` on the
  server, declared `external`, like OpenBao's: `down`, `down --volumes`,
  `uninstall` and a redeploy leave it alone, and `configure` and `up`
  create it if it is missing. If the volume already holds a Postgres from an
  earlier deployment and this `configure` generated a new password (a new
  OpenBao), it says so and ends with the command that starts over,
  `versola uninstall --purge`: the database keeps the password it was
  created with, and what it holds was encrypted with the keys of the OpenBao
  that is gone. No `docker` command is asked of you.
- **After a plain `uninstall` of a deployment with your own Postgres**,
  `uninstall` forgets which one it was (the record is removed) but keeps
  OpenBao and its secrets. A `configure` without `--postgres-host` then
  finds no record, no data volume of a bundled Postgres and a Postgres
  password already in OpenBao, and refuses before it stores a secret or starts a container: it
  would otherwise start an empty container and run Versola on an empty
  database. The message ends with the two ways out: the same command with
  `--postgres-host <host:port>`, or `versola uninstall --purge` to start over
  (this destroys OpenBao's secrets). `uninstall` prints the address it
  forgets and the flag to give it back. An interrupted first install being
  continued, and a first install, are not affected.
- **Where the data is** is in `versola status`: the volume
  `versola-postgres-data-vps` and where Docker keeps it on the server
  (`Postgres: in a container of this deployment, ...; data in the Docker volume ...`).
  For your own Postgres it says whose it is.
- **Starting.** `up` starts Postgres first and waits for its healthcheck,
  then central, auth and edge. Compose recreates the container when the new
  deployment's files are in another directory, as after every `configure`:
  the database restarts for a moment (the data is in the volume, untouched),
  in the same short stop as the services' own. A rolling upgrade never
  recreates it. A release that moves the pinned image to another PostgreSQL
  major version needs a dump and a restore, which nothing here does.
- **The choice is remembered.** `configure` run again without
  `--postgres-host` keeps the Postgres it chose the first time: the
  container, or the external one at its recorded host (never a new empty
  database over a deployment that used one of its own). Passing
  `--postgres-host` to a deployment that runs its own is refused: moving the
  data is a backup and a restore, which `configure` does not do. A rolling
  upgrade keeps the mode. If the record of the current deployment can't be
  read, `configure` without `--postgres-host` refuses instead of guessing.
- **The database name is fixed by the first start.** `--postgres-db` can
  not be changed afterwards (a dump and a restore, not a configure's job).
- **Not combined with `--postgres-ca`**: the container speaks no TLS, on
  loopback only (the services connect with `sslmode=disable`).
- **It needs a `versola-tools` release that can run it.** An older one
  ignores the mode and writes a compose file without the service; `configure`
  then refuses before anything is started and says to use a newer release or
  `--postgres-host`.

**Back it up.** The volume is the database, on one server: `versola backup
create` (see [`backup create`](#backup-create--backup-list--backup-restore)) dumps it through
the container, together with what the dump needs to be readable.

`uninstall` keeps the data volume; `uninstall --purge` destroys it together
with OpenBao's (see `uninstall`).

`status` lists the container with the others, `stats` shows what it uses
(its memory counts as used in the stand-in verdict), and `versola logs postgres`
shows its output (see [`logs`](#logs)). `doctor --target vps` counts it in the
memory floor (a fixed 256 MiB until the release publishes a figure).

### Your own Postgres: `postgres init`

With `--postgres-host`, the Postgres is yours, and the role, the database and
the schemas have to exist before `migrate`. `versola postgres init` makes them,
for the vps deployment `configure` last prepared (the role, the database and
the address are those of its `auth.conf`, the password the one in OpenBao):

```
versola postgres init --sudo-postgres
versola postgres init --admin-url 'postgres://admin@db.example.com:5432/postgres?sslmode=verify-full'
```

What it does depends on the rights it finds:

| What you give | What it does |
|---|---|
| An admin (`--admin-url`, or `--sudo-postgres` for the Postgres of this server) that can create roles and databases | creates the role (with the deployment's password) if it is missing, the database owned by it if it is missing, then, logged in **as that role**, the schemas. An admin that is not a superuser makes itself a member of the role it has just created, so that it can own the database; that membership stays |
| No admin, but a role that already owns the database | only the schemas that are missing, as that role |
| Not enough rights | runs nothing; says which rights are missing and writes the SQL for an admin to `~/.versola/openbao/vps-postgres-init.sql` (0600), safe to run again (not in a `--dry-run`, which only prints) |
| A database that belongs to someone else, or one to be made for a role that exists by an admin that is not a superuser | runs nothing and writes no SQL: it says what to do (let the role own it, or point the deployment at another database) |

- **It only creates what is missing.** No `DROP`, no `ALTER` of a database or a
  schema that exists. The one exception is the password of a role that exists
  (a role it created gets the deployment's `POSTGRES_PASSWORD`): see the next
  item. A database that exists and belongs to another role is never taken
  over: the command stops and says what to do.
- **A role that exists but is refused its password** (Postgres answers
  `password authentication failed`: after `uninstall --purge` a new OpenBao
  generated a new password and the role kept the old one) is met when an admin
  was given. The command shows `ALTER ROLE ... PASSWORD '<hidden>'` and asks
  `Set the password of the role? [y/N]`; on yes the admin sets it (the
  password's SCRAM verifier goes over psql's stdin, never on a command line or
  to the screen), the login is checked again and the run goes on. This happens
  before the plan below is shown, so a plan that is then refused, or a no at
  `Continue?`, leaves the new password in place; run the command again to
  continue. Any other application that logs in as that role with its old
  password stops working, so **`--yes` does not answer this question** and
  neither does a piped `y`: only an answer at a terminal does. Without one, or on
  no, nothing is changed, the exit code is 4 and the message names
  `~/.versola/openbao/vps-postgres-role.sql` (when it is still there) and the
  command that runs it. `--dry-run` prints the statement, changes nothing and
  exits non-zero (the deployment is not ready). A login that fails for another
  reason (Postgres not reachable, `pg_hba.conf`, TLS, a role that may not log
  in) is never offered this: the error says what to check and nothing is
  changed; and if the role still cannot log in after the password was set, the
  error names the usual causes (`pg_hba.conf`, an expired password).
- **The SQL is printed before it runs**, with the password hidden (the role is
  created with the password's SCRAM verifier, not the password; one that is not
  ASCII, which the generated ones never are, goes in as itself, and then can
  show up in the server's log), and a
  `Continue? [y/N]` follows; `--yes` skips it, `--dry-run` only prints (it still
  asks Postgres what exists: read-only).
- **The admin's password** is read from `--admin-password-file` (readable by you
  only, mode 600 or stricter, checked where the system has modes; first line), else `PGPASSWORD`, else asked for with no echo. It is never taken from
  `--admin-url` (a URL with a password is refused) and never on a command line:
  `psql` runs in a throwaway `postgres:18-alpine` container on the host network,
  with the password in its environment, the SQL on its standard input. The SQL
  shown for the database of a non-superuser admin includes psql's own
  conditionals (`\if`, `\gset`) that choose the form of `GRANT` for the server's
  version.
  `--sudo-postgres` needs `psql` and `sudo` on this server and no password; it
  acts on the cluster at the deployment's port, and is refused when the
  deployment's Postgres is at another host (it would act on this server's
  Postgres), and then `--admin-url` is the way.
- **A role that exists** has to log in with the deployment's password before a
  database is made for it; otherwise nothing is changed and it says so. The
  login is tried on the `postgres` database: a server that does not let a role
  connect there stops the run with that reason.
- **Without an admin** nothing tells in advance whose the database is: a
  `--dry-run` prints the schemas it would create, and a run that finds the
  database is someone else's fails at that step ("permission denied"), changing
  nothing.
- **TLS** is as the services' own URL has it (`sslmode` of the deployment, its
  `--postgres-ca`), unless `--admin-url` says another `sslmode`. A mode that
  does not verify the server (the deployment's `disable` for the loopback, say)
  is carried over only when both are on the loopback: for any server across the
  network, even the very host the deployment uses, it is `verify-full` unless
  the URL says otherwise, so the admin's password does not cross the network in
  the clear.
- **Names** of the role and the database are taken from `auth.conf` and must be
  plain (1-63 letters, digits, `_` or `-`); anything else is refused, create it
  by hand.
- **Declining** the question, or having no terminal without `--yes`, ends with
  an error and exit code 4, so `postgres init && migrate` does not go on.
- **Run again**, it re-checks: a deployment that is ready says "Nothing to do".
  Cut off half way, it continues with what is missing.
- Not for a deployment that runs its own Postgres in a container: that one
  creates all of this by itself.

### Deploying to a VPS: the reverse proxy

On `vps`, Versola is exposed through its own reverse proxy: the official
`nginx` image (not a Versola one), with its whole configuration generated
by this CLI. `configure vps` writes it into the deployment next to
versola-tools' files; `up` starts it last, once auth and edge are ready,
and waits until a request through it actually reaches auth. It also
serves the admin console at `/central/admin/`, with the security headers
(`X-Content-Type-Options`, `Referrer-Policy`, `X-Frame-Options`, a `frame-ancestors`
Content-Security-Policy) and, when the browser used https, `Strict-Transport-Security`
(with `--proxy external`, as `X-Forwarded-Proto` says).

Its access log (`versola logs proxy`, on `local` and on `vps`) records the method, the path and the
protocol of a request, and the Referer, never with a query string: the
one-time first-login link (`?link_token=...`), OAuth codes and signed request
objects would otherwise reach `docker logs` and whatever collects it. The
path is the one after the proxy's own rewrites, and the query (`client_id`,
`redirect_uri`, ...) is not kept. A deployment configured before this change
picks the format up on its next `configure` followed by `up` (`bootstrap`
does both); old log lines stay.

What this does not cover: nginx's error log. On an upstream failure (a
restart, a rolling upgrade, edge still starting) and when a rate limit
refuses a request, it prints the request line, the upstream URL and the
Referer, all with their query strings, and nginx cannot strip them. Treat
`versola logs proxy` as sensitive until the first-login link is used. With
`--proxy external` the proxy in front keeps its own log.

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
  versola bootstrap vps 0.7.1 --auth-url https://id.example.com
  versola bootstrap vps 0.7.1 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432
  ```

  The first runs Postgres in a container of the deployment (see "Postgres on
  vps"); the second uses a Postgres of your own, here the server's native one.

  Postgres can also be a remote, TLS-only server: `--postgres-db <name>`
  picks the database (default `auth`) and `--postgres-ca <file>` is a PEM
  file with the CA that signed its certificate. Both are vps-only and are remembered
  (the CA file is copied next to the deployment's state): `rollout` reuses
  them, and a later `configure` without the flag keeps the stored value. They need a
  `versola-tools` release that understands `POSTGRES_DB` /
  `POSTGRES_CA_FILE`: **Versola 0.7.1 or later**.

  The services verify Postgres's certificate (`sslmode=verify-full`) unless
  the host is literally `127.0.0.1`, `localhost` or `[::1]`: with
  `--postgres-host 127.0.0.1:5432` TLS is not verified, but with any other
  host name -- even one that resolves to this very machine -- the name must be in the certificate's subject alternative names,
  and a self-signed or private CA must be given with `--postgres-ca`.
  On a first install `configure` writes the SQL that creates Versola's role
  or sets its password to `~/.versola/openbao/vps-postgres-role.sql` (see
  Secrets above): run it **before** `migrate`, or `migrate` fails with
  `password authentication failed`.

  Right after that, for a Postgres of your own, `configure` connects the way
  the services will (a `psql` in a throwaway container; the password goes
  only into its environment) and prints one line: reachable, TLS accepted,
  login, the server's version (a warning below 18, the version CI runs
  against) and which of the schemas `auth`, `central` and `edge` exist. It
  only reads. A certificate problem (unknown CA, a name not in the
  certificate, no TLS offered) stops `configure` before `migrate` and `up`,
  with the flag that fixes it (`--postgres-ca`, or the certificate's name as
  `--postgres-host`), and the previous deployment stays as it was. The role
  not being set up yet on the first `configure` is "not yet", not an error;
  a refused login on a later one, an unreachable address, a missing
  database or a check that could not run (no Docker, no image) are warnings,
  so that a run of `configure` before the role SQL has been applied is not
  blocked.

  For a test deployment, add `--acme-staging`: the certificate then comes
  from Let's Encrypt's staging environment — untrusted by browsers, but
  without production's rate limits (a handful of certificates per domain
  per week). It is remembered by later `configure vps` runs;
  `--acme-staging=false` switches to a trusted certificate.

  Each public name you ask for (see "Public names" below) also needs a DNS
  record pointing at this server, next to `--auth-url`'s name.

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
  versola bootstrap vps 0.7.1 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432 --proxy external
  ```

  The forwarding web server has to run on this same host (natively, or
  in a container with host networking): 127.0.0.1:2821 isn't reachable
  from anywhere else. The domain in `--auth-url` also has to resolve from
  the server itself (central calls auth at that address); without DNS for it
  central logs `UnknownHostException`. This mode serves no 80/443 itself, so
  nothing answers on the domain until your web server does.

  Versola's proxy doesn't hold 443 here, so it can't publish a name on it;
  your web server has to (see "Public names").

### Public names: `--mtls-domain`, `--central-mtls-domain`, `--central-domain`

A deployment is addressed by `id.<domain>` (`--auth-url`: the web UI, the
OAuth endpoints, the admin console). Everything else is **private by
default**: auth's and central's mutual-TLS listeners, and central's
enrollment route, answer on this host's loopback only, and an edge on the
same host reaches them there, by address (`https://127.0.0.1:8083` and
`:8093`), never through a public name. With none of the three flags below,
nothing but 80 and 443 for `--auth-url` is open, and no stream block is
generated.

You need a public name only when something *outside this host* has to reach
one of those listeners:

| Flag | Name (by convention) | Publishes | You need it when |
|---|---|---|---|
| `--mtls-domain` | `id-mtls.<domain>` | auth's mutual-TLS listener | an edge in another network (or a partner's `tls_client_auth` client, or a FAPI client) calls `/par`, `/token`, `/userinfo`, ... with a client certificate |
| `--central-mtls-domain` | `central-mtls.<domain>` | central's mutual-TLS listener (the edge surface) | an edge in another network syncs its configuration from central |
| `--central-domain` | `central.<domain>` | central's enrollment route only | a new remote edge gets its first certificate (`versola edge bootstrap`) |

A partner's edge ("Sign in with X" on the partner's own domain) is an
ordinary OAuth client of auth: it uses `id.<domain>`, plus `--mtls-domain`
when it authenticates with a certificate -- never central. A first-party
edge in another region or cloud uses all three. A single-host deployment
uses none.

All three are optional, independent, remembered by the deployment (a later
`configure`, and `rollout`, keep them; `--mtls-domain=` with an empty value
stops publishing one) and need `--auth-url` to be `https`. Each name needs a
DNS record pointing at this server. They have to differ from each other and
from `--auth-url`'s name.

```
versola configure vps 0.8.0 --auth-url https://id.example.com --postgres-host 127.0.0.1:5432 \
    --mtls-domain id-mtls.example.com \
    --central-mtls-domain central-mtls.example.com \
    --central-domain central.example.com
```

What each does, with `--proxy nginx`:

- **The mutual-TLS names** are routed on 443 *without terminating TLS*, by the
  server name in the ClientHello (`ssl_preread`), so the handshake, the
  service's own certificate and the caller's client certificate reach it
  untouched. The listener's certificate (from the deployment's own CA, not
  from a public one) gets the name in its subject alternative names -- adding
  a name later re-issues it at the next `versola up` or `certs renew`. A
  remote edge pins that certificate rather than trusting a CA.
  Port 443 is then a stream (TCP) server, and the http server that was on it
  moves to `127.0.0.1:8444`, still with the ACME certificate; the stream server
  hands it the client's address in a PROXY protocol header, so auth's per-IP
  throttling still sees real addresses. Port 80 is unchanged, so Let's
  Encrypt's HTTP-01 challenge works as before. All of `127.0.0.1:8444`,
  `8445` and `8446` must be free; `configure` checks.
- Two servers per mutual-TLS port: the **internal** one is what a local
  edge uses -- auth's on `127.0.0.1:8083` (balanced over every auth replica)
  and the proxy's front of central's listener on `127.0.0.1:8093` (balanced over the central replicas, which listen on 28093) -- with no SNI and no
  limits; the **public** one is a loopback relay per name
  (`127.0.0.1:8446` for `--mtls-domain`, `:8445` for `--central-mtls-domain`)
  that the 443 server forwards to, with `limit_conn` of
  100 connections per client address (the only thing that can be said about a
  client before its certificate is checked; a refused connection is logged at
  `warn` in nginx's error log) and a log of its own: one line per connection
  tagged `mtls-public` on the proxy's stdout (`versola logs proxy | grep
  mtls-public`), with the name, the client address, the status and the bytes.
  Alert on a spike of sessions that end within a second after a few hundred
  bytes there: from outside, that is what failed handshakes look like (nginx
  does not see inside the TLS it passes through).
- **`--central-domain`** is plain https, terminated by Versola's proxy with a
  Let's Encrypt certificate of its own (nginx's ACME module, like
  `--auth-url`'s -- port 80 answers its challenge too). It serves **one
  route**, `POST /edge/enroll`, to central's enrollment listener
  (`127.0.0.1:8094`, central's `APORT`, not auth's 8082); everything else is 404. It is rate limited
  per client address (6 a minute, burst 5) before central sees a request, and
  central limits per token on its side.
- Central's own mutual-TLS listener (28093) and enrollment port (8094) are bound to
  the loopback by the compose file: unlike before, nothing is open on a
  public interface unless the proxy publishes it.

The generated files are `proxy/stream.d/public-mtls.conf` and, for the
enrollment name, a server in `proxy/conf.d/versola.conf` (see
`~/.versola/active/proxy-vps/current/`).

**Firewall:** open only 80 and 443 (and only 443 reaches the mutual-TLS
names). Ports 8083, 8093, 8094 and 8444-8446 are loopback and must stay closed
to the internet, as must 8082 and 8090. An edge that cannot do SNI
through its own firewall has no alternative port: that was removed with the
public bind of 8093.

With `--proxy external`, Versola's proxy only listens on `127.0.0.1:2821` and
doesn't see 443, so the web server in front has to do the same itself --
`configure` and `up` print what is needed for the names you gave. For nginx,
a `stream` block in the main context sharing 443 with the `http` server that
handles `--auth-url`'s name, and, for the enrollment name, an `http` server
that forwards one route:

```nginx
stream {
    map $ssl_preread_server_name $versola_tls {
        hostnames;
        id-mtls.example.com       127.0.0.1:8083;   # auth's mutual-TLS listener
        central-mtls.example.com  127.0.0.1:8093;   # central's mutual-TLS listener
        default                   127.0.0.1:8443;   # your existing https server, moved off 443
    }
    limit_conn_zone $binary_remote_addr zone=mtls_conn:10m;
    server {
        listen 443;
        ssl_preread on;
        limit_conn mtls_conn 100;
        proxy_pass $versola_tls;
    }
}
```

(Your `http` server for the auth name then listens on 8443; to keep the client
address it uses `proxy_protocol` on both ends, or the address is left out of it.
Limiting every connection on 443 per address, as here, also counts browsers: raise
the number, or give the mutual-TLS names a listener of their own.) For
`--central-domain`: terminate TLS for the name with your own certificate and
`location = /edge/enroll { limit_except POST { deny all; } proxy_pass
http://127.0.0.1:8094; }` with a `limit_req`.

**Migrating from `--central-url` of an earlier version.** `--central-url` used
to default to `https://central.<domain>`, which the proxy routed to central's
port 8093 by SNI, and 8093 was open on every interface. Neither is the case
now: central's mutual-TLS name is `central-mtls.<domain>` and must be asked
for with `--central-mtls-domain`; `central.<domain>` is the enrollment name
(`--central-domain`); a local edge no longer goes out through any name; and
8093 answers on the loopback only. `--central-url` is still accepted, only as
an explicit address an edge configured by hand is given -- it routes nothing,
and is no longer derived from `--auth-url`. A deployment whose edges were
elsewhere re-runs `configure` with `--central-mtls-domain central-mtls.<domain>`
(its leaf certificates are re-issued with the name), points those edges at it,
and goes through `versola up`.

`versola doctor --target vps --auth-url ... --mtls-domain ...` checks each name
resolves to this server and answers a TLS handshake on 443.

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

Central is behind the proxy the same way, so that a rollout can move it
without downtime (through a stand-in, below): the proxy listens on `127.0.0.1:8090` (the
compose file's central PORT, which auth, edge and the admin console call --
their URLs are unchanged) and balances over the central replicas. Central
itself listens 20000 higher: PORT and DPORT on 28090/28091 (set in
`overrides.yml`), a second replica on 28190/28191. Those ports are outside
the 9090-9100 range that Prometheus and its exporters use and below Linux's
ephemeral ports (from 32768). Only the proxy may use 8090 on a vps now, and central's diagnostics port
(`/readiness`) is no longer on 8091 but on 28091: point anything outside the
CLI that probes it there. `replica add|remove` refuses on a deployment that
has not been moved yet (`versola up` does it; central restarts once).

A release whose central serves edges on its own mutual-TLS listener (its
compose file sets `MPORT: 8093` for central; versola#607) gets that listener
moved the same way: the proxy listens on `127.0.0.1:8093` -- the loopback
only, never a public interface; an edge on this host uses it by address, and a
remote edge reaches it only through `--central-mtls-domain`, whose relay ends
there -- and balances over the central replicas, which listen on the loopback
20000 higher (28093; 28193 for a second replica): `overrides.yml` and
`replicas.yml` set `MPORT` and `MBIND_HOST: "127.0.0.1"` for them. Port 8093
is then the proxy's: `configure` and `up` refuse another service on it, and a
rollout's central stand-in takes its own pair of ports (28190/28191, 28193)
instead of fighting the running central for 8093. A release whose central has
no `MPORT` in its compose file (0.7.x) is untouched: the proxy listens on
nothing there. Moving the listener's port between two releases is refused by
`rollout` like any other moved port; gaining or losing it is not.
`up` starts the proxy as soon as central is ready, before auth and edge:
they reach central through it. The first `rollout` of a deployment made
before this change moves central to the new ports within its usual central
restart (the admin console is unavailable for a moment longer, once);
auth and edge keep serving from their caches.

Its configuration lives outside the deployment bundles, in
`~/.versola/active/proxy-vps/`, as generations (`gen-000001/`, ...) with
`current` pointing at the one in use. `up` and `replica add|remove` build
a new generation, have nginx check it in a throwaway container, switch
`current` and reload nginx -- the container is not restarted, so requests
in flight finish. Compose recreates it only when its definition changes:
once, the first time a deployment moves to this layout, and after that for
a new proxy image or a change of `--proxy` mode or TLS.

A reload ends the TCP connections passed through to auth's MPORT once the
old nginx workers stop, 30 seconds later -- all of them, idle or not: a TCP
pass-through can't tell where a request ends. A connection idle for an hour
is closed too. edge opens new ones for its next requests; a request that is
in flight on such a connection at that moment fails (edge's HTTP client
does not retry it). A replica added later gets new connections only.

This needs a Versola release whose versola-tools ships the admin console;
`configure vps` says so clearly when pointed at an older one.

### `replica add|remove <auth|edge> [count]`

Runs more than one copy of auth or edge behind the reverse proxy, which
balances requests between them (`least_conn`). central is not replicated.
Works on `local` and `vps` (on `vps` it asks for confirmation first).

```
versola replica add auth 2
versola replica remove edge
```

- `add` starts `count` replicas (1 if omitted) one at a time, in the lowest
  free slots -- at most 9 per service. Each is waited for until it is
  ready, and only then does the proxy start sending it requests. A replica
  that doesn't come up is removed again; the ones started before it stay.
- `remove` takes the highest slots first: the replica leaves the proxy, the
  requests it is answering finish (up to 30 seconds), then it is stopped.
  The last replica of a service can't be removed.
- Slot 1 is the service as in the compose file; slot n uses the compose
  file's ports plus 100·(n−1) (auth 8080, 8180, 8280, ...; on `vps` auth's
  APORT/MPORT are fronted by the proxy, see above). The replicas are
  recorded in `~/.versola`: `up` brings the same ones back and `status`
  lists them.
- Each replica is a whole JVM (sized at about 850-880 MiB), with its own
  Postgres connection pool (10 connections; edge one more for LISTEN).
  `replica add` checks ports, that central runs, the Docker Compose
  version (2.24 or newer) and -- on `vps` -- that Postgres can take the new
  pools (the same check as `rollout`'s, see below), **not** memory: with
  Postgres' default `max_connections = 100`, a handful of extra replicas is
  the limit.
- Needs a running deployment (`versola up`; on `vps` one whose central is
  already behind the reverse proxy -- an older deployment gets there with one
  `versola up`) and is refused in the middle of a rolling upgrade. While a
  service has more than one replica, `configure` refuses (remove the extra
  ones first) -- to move such a deployment to another version, use `rollout`.

### `rollout <version>` / `rollout --abort` (vps)

`versola rollout <version>` moves a running vps
deployment to another Versola version without stopping it: the new
version is prepared next to the running one (versola-tools, the secrets
plan), the database is migrated, central is moved to the new version
behind a stand-in (central-2, on 28190/28191, next to the running central
behind the proxy's front of 8090; then central itself is recreated and the
stand-in goes -- the admin console and self-registration stay available),
then each auth and edge replica is replaced one at a time behind a
stand-in replica in a free slot, and the proxy moves to the new
configuration. Central keeps one replica; the stand-in exists only while
the rollout moves it. Without a stand-in (`--no-surge`), or when the
running proxy does not front central yet (a deployment from before that,
whose first rollout moves central to its new ports), central is stopped and
started in place instead: the admin console and self-registration are down
until it is ready, auth and edge keep serving from their caches. Cut off anywhere (an error, Ctrl-C,
a dropped SSH session), the same command continues (a Ctrl-C, the first one
included, exits 130); progress is read back
from `state.json` and the containers. `--dry-run` shows the plan, the
secrets plan and the pending migrations and changes nothing. `--no-surge`
replaces replicas without a stand-in (one fewer serves meanwhile; with one
replica it needs `--allow-downtime`; central is restarted in place, as
before stand-ins).

**Compatibility is declared by the release.** During a rollout the running
version works on the new version's schema, and an abort runs it there
alone. Every release's versola-tools carries a manifest, `release.json`,
with one entry per release: from which running versions that release can
be rolled to (`rollingFrom`) and from which no in-place upgrade exists
(`freshDatabaseFrom`). Before anything changes, `rollout` (and `--dry-run`)
runs the new release's versola-tools into a temporary directory and reads the entry
of the version it deploys (`~/.versola/rollout-compat-*`, removed at once;
it holds only unused secret candidates):

- the running version is in `rollingFrom`: the plan says *declared by the
  release* (with its notes link) and no flag is needed;
- it is in `freshDatabaseFrom`: the rollout is refused, **with or without
  `--assume-compatible`** -- no flag can make an in-place upgrade exist. The
  message gives the release notes and the way on (a backup, then
  `uninstall --purge` and `bootstrap`, with your own Postgres an empty
  database), before `migrate` has run;
- anything else -- a release from before the manifest, a version it does not
  mention, a downgrade (a manifest only speaks about upgrades: the running
  version has to be lower) -- keeps the old rule: `rollout` refuses without
  `--assume-compatible`, and says why (*the release does not state
  compatibility with 0.7.0*). Releases before 1.0 don't promise it ("compatibility
  between 0.x releases is not guaranteed"): read the release notes first
  (additive migrations; the running version works on the new schema). A release
  that is not compatible is deployed with `configure`, `migrate` and `up` (a
  short stop) after a database backup.

A glob in `rollingFrom` (`0.7.*`) does not match a pre-release such as
`0.7.9-rc1`; a glob in `freshDatabaseFrom` does. A manifest this versola
cannot read (a newer `schemaVersion`) stops the command with a message that
says so (`versola upgrade`): it may declare what this version cannot
understand. A rollout that is already under way is continued without asking
again. `rollout --abort` is not asked either; going back to an older version
by rolling it out again needs `--assume-compatible`, since an older release
cannot know a newer one.

`versola rollout --abort` goes back to the version the unfinished rollout
came from:

- nothing started from the new version yet (prepared, possibly migrated):
  the previous configuration becomes the active one again; no container
  is touched;
- otherwise the previous configuration becomes the active one again too, and
  what moved on is moved back the same way, stand-in first: central behind a
  stand-in (in place with `--no-surge`), and the auth and edge replicas that
  were already on the new version. Replicas that never left the old version
  are not touched: they already run from the previous configuration. A
  stand-in of the new version is retired before one of the old version
  starts. If the previous configuration is gone (more than one earlier one
  is recorded, or its files were removed), the previous version is
  configured anew instead -- versola-tools and the secrets plan run again,
  which needs `--allow-legacy-secrets` for a release without a secrets
  schema -- and every replica is recreated from it.

Migrations and secrets don't go back: the old version runs on the migrated
schema (not guaranteed before 1.0 -- a replica that doesn't become ready
stays out of the proxy while the others serve; restore the database from
the backup if the old version can't run at all), and secrets the new
version added stay in OpenBao. No compatibility confirmation is asked. If
the rollout used `--no-surge` because the server has no memory for one more
JVM, pass `--no-surge` to the abort too (it skips the stand-in memory and Postgres checks). Cut off, `--abort` again continues
it (`state.json` records that an abort is under way, so it does not turn
round). Once a rollout has finished there is nothing to abort: roll the
earlier version out instead.

While a rollout or an abort is under way, `configure`, `up` and `replica`
refuse, and so does `down` unless `--force` is given -- afterwards only
`rollout` (or `--abort`) starts the deployment again, not `up`.

**Before a stand-in starts** the rollout checks, and refuses with the
numbers if either is short (the way round is `--no-surge`, which needs
neither). `versola stats` makes both checks without starting anything, so
you can see the answer before you run the rollout (see `stats`):

- memory: free memory (`MemAvailable`) must hold one more replica --
  what a running replica of that service uses now (`docker stats`), never
  less than 900 MiB (a replica's JVM is sized at about
  850-880 MiB, and a freshly started one has not reached that yet) -- plus
  256 MiB: a rollout with a stand-in needs at least about 1.13 GiB (1156 MiB) free. Checked before anything changes and again
  right before each stand-in. It has to run on the vps itself: with Docker
  pointed at another host it refuses.
- Postgres connections: a stand-in opens its whole pool at once (10 for
  auth, 10 plus a LISTEN connection for edge and for central, read from the
  bundle's configs). `psql` in a throwaway `postgres:18-alpine` container (pulled
  the first time) asks the server how many more client connections it
  takes (counting other roles' sessions too); fewer than the pool plus 3 is
  a refusal. The connection uses the `sslmode` of the service's URL
  (versola-tools writes `disable` for a Postgres on loopback; without one,
  `verify-full`, as the services do), verified against the same CA as the
  services: the `--postgres-ca` file from the bundle (mounted read-only),
  else the system's CAs. libpq needs a chain up to a self-signed root in
  that file: with only an intermediate CA in it the check fails and warns.
  As in the services, the URL's `sslrootcert` wins over `ssl-root-cert`, and
  `disable`/`require` ignore the CA; a CA outside `/etc/versola/postgres-ca`
  or `verify-ca` without a CA file can't be checked and only warns. The
  check runs again before each stand-in, with the new version's pool size. The password goes from
  the service's `*.secrets.env` into that container's environment only --
  never onto a command line or the screen. If the check can't run (no
  image, a TLS error, a login failure) it only
  warns: a stand-in that can't connect never becomes ready and is removed
  again.
  `replica add` runs the same check before it starts anything, for the
  replicas it adds -- they all stay, so their pools together plus 3 must fit
  -- and refuses the same way (raise `max_connections`, or add fewer).

**Short outages a rollout does have:**

- only without a central stand-in (`--no-surge`, or the first rollout of a
  deployment whose proxy does not front central yet): central is stopped
  and started in place, so the admin console and self-registration are down
  until it is ready -- on an idle stand 0.7.1 was ready well within a
  minute; not measured on a loaded server. The rollout prints how long it
  was. Any wait for central (its
  stand-in too) is up to 5 minutes; a replica's, 2 minutes. If central is still not
  ready it stops, leaving auth and edge untouched: run the same command
  again once central is up (it only waits again). `--abort` also works and
  starts central once more, on the old version. auth and edge keep serving.
  If their five-minute cache refresh falls into that gap, seen on a stand:
  their outbound HTTP pool (`fixed`, 128 slots by default) keeps the refused
  connections and hands them out one per request, so the refreshes keep
  failing for about 128 / 13 refreshes (some 50 minutes) after central is
  back; they serve from their caches meanwhile. `HTTP_CLIENT_POOL_KIND=dynamic`
  in their environment avoids it (see Versola's `ReloadingCache`).
- auth's mutual-TLS port (8083) is passed through as TCP: every proxy
  reload (each replica taken out or put back) ends the connections the old
  nginx workers hold, 30 s later. Idle ones are simply reopened by edge; a
  request in flight at that moment fails. On vps the only caller is edge's
  native-app back channel (`/par`, `/token`, `/revoke`), so a native app
  would have to try again. Not measured with the real edge yet; in a local
  run of the e2e client (`e2e/rollout`, three reloads) it saw one per reload
  on a request kept always in flight and none on short ones. The listener
  is on by default; nothing on id.versola.io uses it today.
- every edge replica the rollout starts (the stand-in, each recreated
  slot, again on an abort) gets its own client certificate through
  central.

`rollout` never replaces a mutual-TLS certificate: a leaf is replaced by
`versola certs renew` (which publishes its successor as a pin before using it,
see there) or by a full `versola up`, which restarts everything anyway. A
rollout run while a renewal is staged but not committed (a `certs renew` that
was cut off) recreates the replicas as usual -- an edge it starts reads the
staged pin along with the current one -- and the next `certs renew` finishes
the renewal.

#### Runbook: a rolling upgrade on the vps

1. Read the release notes of the new version: additive migrations, the
   running version working on the new schema. If not, this is a
   `configure` / `migrate` / `up` deployment instead. A release that declares
   it in its manifest says so itself in step 3 (and a fresh-database release
   is refused there).
2. `versola backup create` -- migrations can't be rolled back -- and copy
   the archive off the server. Without the CLI: a `pg_dump -Fc` of the
   database and a copy of `~/.versola/active/state.json` (and OpenBao's data
   and `~/.versola/openbao/`, without which the dump can't be read).
3. `versola rollout <version> --dry-run`: the plan, the checks, the
   secrets plan, the migrations it would apply. Nothing changes.
4. `versola rollout <version>` (add `--assume-compatible` only when the plan
   says the release does not state compatibility). Cut off, run the same
   command again; `versola status` shows where it stands.
5. Check through the proxy (`curl -H 'Host: <your domain>'
   http://127.0.0.1:2821/.well-known/openid-configuration` and the admin
   console).

Going back: `versola rollout --abort` while the rollout is unfinished
(add `--no-surge` if the rollout needed it); once it finished, roll the
earlier version out the same way. If the old version can't run on the
migrated schema at all, put the database of step 2 back: `versola down`,
`versola configure vps <the old version>`, `versola backup restore <archive>
--replace-database`, `versola up` (see [`backup restore`](#backup-restore),
"Go back after a failed upgrade"). A Ctrl-C while a restore runs does not stop
it half way: the command says whether the database was replaced when it ends
(with a Postgres that was already running, it ends at once and the restore may
still finish inside the container: look at the tables before going on).

### `certs renew` (vps and local)

Auth's and central's mutual-TLS listeners and the certificate edge
presents to central are issued by the deployment's own CA (step-ca) into
the `versola-mtls-vps` volume (`versola-mtls-local` locally). The CA has
three intermediates under one root, and each listener trusts only the
ones it should: **server** signs the listeners' own certificates, **edge** signs the
identity an edge presents to central, **client** signs the certificates of
clients. The files `mtls-init` writes (the listeners' leaves and the identity of the
edge on this host) live 365 days by default. Central's listener trusts
the edge intermediate alone, so a client's certificate does not even
complete a handshake there; auth's trusts clients and edges, and accepts an
`urn:versola:edge:...` name only from a certificate the edge intermediate
signed.

`versola certs renew` keeps them valid, the way cert-manager does on
Kubernetes: it runs versola-tools' `mtls-init` service as a one-off, which
re-issues a certificate only when a third of its lifetime or less is left,
and restarts what reads it, because the services read their certificates at
startup. When nothing is due it prints nothing and exits 0; a failing step
exits non-zero with its output. Nothing needs registering in central: an
edge is recognised by the URI name in its certificate.

**On vps it is rolling, and a leaf is never replaced before the edges know
its successor.** The edge pins the listeners' own certificates (a leaf, not
a CA), as a set of two files, the current and the next. For a leaf that is
due:

1. `mtls-init` *stages* the replacement beside the current pair
   (`server.next.crt`, `central-server.next.crt`): the pin set now contains
   it, and nothing has been replaced.
2. The edges restart to learn it, one replica at a time -- out of the proxy,
   drained, restarted, ready, back in. (Edge identity renewals take this
   path too.)
3. With `--mtls-domain` or `--central-mtls-domain` published, edges in other
   networks learn the pin from central, which reads it when it starts: central
   restarts to publish it, and `--pin-wait` (default 15m) is left for them to
   fetch it before the leaf is replaced.
4. The leaf is *committed*, and central, then auth's replicas, restart onto
   it -- auth's one at a time, drained first, so its mutual-TLS connections are
   moved off a replica before it restarts.

A service with a single replica is restarted where it stands, and is
unreachable until it is ready (add a replica with `versola replica add` to
restart without a gap). Central has one replica
and always restarts in place. A renewal cut off anywhere is finished by running
it again: staging is repeatable, and nothing is replaced before step 4.

If a restart fails after the leaf was committed, the error names the services
still running on the old one (an edge that restarts later would pin the new
leaf while auth still presents the old); once the problem is fixed,
`versola certs renew --restart` restarts them -- central, auth, edge, in that
order -- and puts a replica that stayed out of the proxy back (a plain `renew`
finds nothing due by then).

Risk after a commit: if a restart failed and nobody runs `--restart`, the next
daily run finds nothing due and prints nothing, while a replica still presents
the old leaf; an edge restarted for another reason then pins only the new one
and cannot reach it. Treat the error as an open task, and `status` shows
`notAfter` of both. The lock the other commands take is held for the whole
`--pin-wait`, so `up`, `replica`, `rollout` and `configure` wait while a
renewal sleeps.

An edge on the same host reads the pin files from the volume with its restart.
An edge on another host gets the pins from central; to hand one a pin by hand
use the bundle `versola edge add` prints.

On local, central, auth and edge are restarted in that order, each waited for.

**How long they live: `--cert-lifetime-days`** (on `configure` and `bootstrap`,
vps only, remembered; also kept by `rollout`). The default is 365 days, renewed at a
third of the life left, so with the timer running months before they run out. Give a
number of days (1-36500) for another lifetime, or `unlimited` (or `0`): a notAfter
100 years away, so the files never need renewing. Unlimited gives up the expiry as a
limit on what a leaked key can do; a deployment that has it gets no timer, `--no-certs-timer`
is accepted there, and `status`/`doctor` do not warn about expiry or a missing timer. With
a finite lifetime the timer is required: `configure`, `up` and `bootstrap` install it, and
`--no-certs-timer` is refused with a message. A certificate whose lifetime differs from the
configured one is replaced at the next renewal run. The identities central issues to
remote edges renew in-process and are not affected. Releases before 0.8 deploy no
mutual-TLS certificates, so on them no timer is installed, nothing warns about one and `status`/`doctor`
read no certificates; `rollout` to 0.8 or later installs the timer, as `up` does, and, when it is the first release with certificates (0.7 to 0.8), first recreates the certificate authority and issues them (a rollout starts replicas with `--no-deps`, so compose's `depends_on` does not). A central stand-in serves no enrollment: only slot 1 has `APORT`. If a `certs renew` is
cut off after it replaced the leaves but before the services restarted onto them (SIGINT, a dropped ssh), it
leaves a marker, `~/.versola/certs-restart-pending`, and the next run (the timer's too) restarts everything and
clears it.

Run it on a schedule -- daily is plenty. `versola configure`, `versola up` and `versola bootstrap`
(on vps) install the timer, however the
deployment was made: the certificates are short-lived, and a deployment without a
renewal stops working when they run out. If lingering is off for the user, `up`
says so in a line of its own with `sudo loginctl enable-linger $USER`: a user
timer stops with the user's last session without it.

```
versola certs renew --show-timer      # prints what it would write; writes nothing
versola certs renew --install-timer   # writes + enables it (systemd, current user)
```

The timer is a systemd user timer (`~/.config/systemd/user/versola-certs-renew.{service,timer}`,
`OnCalendar=daily`, `Persistent=true`); on a host without systemd
`--show-timer` prints a crontab line instead, which is yours to add. A
user timer stops with the user's last session unless lingering is on
(`sudo loginctl enable-linger $USER`).

`versola status` and `versola doctor` show the `notAfter` of every
certificate, and warn in words when one has under 60 days left and is past the point
where it should have been renewed (or has expired), and when nothing renews them
at all -- no enabled systemd user timer and no crontab line -- or the timer will stop
at logout (lingering off). A missing timer is never silent.

### `edge add` / `edge bootstrap` (vps)

An edge in another network (or cloud) is registered with this deployment's
central, and set up on its own machine from a **bootstrap bundle**. It needs
the three names of "Public names": `--central-domain` (where it gets its first
certificate) and `--central-mtls-domain` (where it connects), and
`--mtls-domain` to reach auth's mutual-TLS listener.

On the central's machine:

```
versola edge add eu-central --group eu > eu-central.json
```

`edge add <id> [--group <g>]` registers the edge with central's admin API
(on this host; the credentials are the deployment's own), issues a one-time
enrollment token and prints the bundle: the edge's id and group, the token
(valid for an hour, spent by its first enrollment), `central.<domain>` to enroll
at, `central-mtls.<domain>` (and `id-mtls.<domain>`) to connect to, and the
certificates of those listeners to pin, with their fingerprints on stderr to
compare with what the edge machine shows. The bundle holds nothing that can be
used again. `--group` names a group of edges (central classifies groups:
first-party, or a partner's; an edge's group does not change); without it the
default group is used. The key central registers the edge with is shown once:
it is written to `./<id>.edge-key.json` (mode 0600, never overwritten;
`--key-out` moves it) and travels by another channel than the bundle.
`--existing` only issues a new token for an edge that is already registered (a
lost identity volume, an expired token). `-o` writes the bundle to a file
(0600) instead of stdout; with `> file`, run `umask 077` first, since it holds
a token (valid an hour) and the key file is created in the current directory:
keep that out of a git working tree.

On the edge's machine, with Docker and the Compose plugin:

```
versola edge bootstrap eu-central.json --key-file eu-central.edge-key.json
```

`edge bootstrap <file>` reads the bundle (an expired token, a non-https
address or a bundle without pins is refused before anything is written) and
writes `~/.versola/edge/<id>/` (`--dir` moves it): `edge.conf` (where to
enroll and connect, which certificates to pin -- generated, rewritten on every
run), the token (0600), the pins, a `compose.yml` with a volume for the edge's
identity and the edge's image at the bundle's Versola release (`--version`
overrides), then starts it. The identity volume holds the key and certificate
the edge enrolled for; keep it, or it enrolls again at every start and the
token is spent. `edge.local.conf` and `edge.secrets.env` in that directory are
yours: created empty, never overwritten (the second takes the key from
`--key-file`); whatever else the edge needs, such as its public URL and its
keys, goes into the first, which `edge.conf` includes last. `--no-start` writes
the files and stops. A partner's edge does not use any of this: it has no
connection to central. (Central's own `edge-identity` setting only matters for
partners' edges; an operator's own, first-party, edges are always extended by central.)

### `backup create` / `backup list` / `backup restore`

```bash
versola backup create [--out <dir>] [--skip-database]
versola backup list [--out <dir>]
versola backup restore <archive> [--replace-database] [--yes]
```

`backup create` writes one archive of the deployment this machine tracks
(vps or local), `versola-backup-<target>-<version>-<UTC time>.tar.gz`, mode
0600, into `--out` (default `~/versola-backups`, outside `~/.versola` so
`uninstall` never touches it). It holds what `configure` cannot make again:

| Member | What |
|---|---|
| `manifest.json` | CLI and Versola versions, target, Postgres mode (`bundled`: the deployment's own container, and local; `external`: yours), database and role, the vps proxy's configuration generation, whether the vps certificate comes from Let's Encrypt staging (`acmeStaging`, only when it does), and the size and SHA-256 of every other member; what was left out and why |
| `database.dump` | `pg_dump -Fc` of the database. The deployment's own Postgres is dumped inside its container (`docker exec`, the server's own `pg_dump`). After `versola down` that container is gone, and `backup create` starts that one service, only it, for the dump and stops it again afterwards (a Postgres that was already running is left running; a Ctrl-C or a dropped SSH session waits until it is stopped again; if it cannot be stopped the command warns and the archive is still written; if it cannot start, the error names `versola logs postgres`). Yours from a throw-away `postgres:18-alpine` container on the host network, connecting and verifying TLS as the services do, with the password read by Docker from `auth.secrets.env`, never put on a command line |
| `openbao-volume.tar` | OpenBao's data volume. OpenBao's file storage has no online snapshot, so OpenBao is stopped for the copy (seconds; the services keep running, they read their secrets from their own files), then started and unsealed with the saved key (also when the copy failed). A Ctrl-C, a dropped SSH session or a kill meanwhile waits until it runs again (it says so at once). The image the copy runs in (`postgres:18-alpine`) is pulled before the stop, never while OpenBao is down. It is stopped only when it is sure to come back, so no question is asked: its configuration file still exists, and this machine has an unseal key and a root token the running OpenBao accepts. Otherwise (OpenBao set up by hand, a stale key file) it is not stopped and the copy is left out, with the reason; the archive is still written, but the command fails (exit code 1), since a dump without OpenBao's data can't be read after a restore. A stopped OpenBao is copied as it is |
| `openbao/` | `<target>.json`, `<target>-admin.json` (the root token and unseal key), `<target>-postgres-role.sql`, the ones present |
| `state/` | `state.json`, and the copy of `--postgres-ca`'s file if there is one |

Not in it: the generated deployment files (`configure` writes them again),
the ACME certificate volume (a redeploy requests a new certificate) and the
mutual-TLS volume (`versola up` reissues the certificates). The command
prints what the archive holds, file names and sizes only, and what it does
not.

**Whoever has the archive has every secret of the deployment**: OpenBao's
data and the key that unseals it are in the same file, as they have to be
for the dump to be readable after a restore. Keep it like a password, and
copy it off the server: a backup that lives only on the server is lost with
it.

`--skip-database` leaves the dump out, for a managed database with
snapshots of its own (it warns when the database is the deployment's own
container: nothing else backs that one up). The database is dumped first, so
a database that refuses the login fails the command before OpenBao is
stopped. Any failure leaves no archive: it is packed under a temp name and
renamed at the end; the staging copies are removed, and those of a run that
was killed outright are removed by the next `backup create` into the same
directory. During an
unfinished rolling upgrade the archive is made, and the manifest
(`unfinishedRollout`) and the output say so: the dump may already have the
new version's schema. While it runs the
deployment is locked like for `configure`, and the dump is staged on disk
before packing, so `--out` needs about twice the dump's size free.

`backup list` shows the archives in `--out`, newest first, with the summary
from each manifest.

#### `backup restore`

```bash
versola backup restore <archive> [--replace-database] [--yes]
```

`backup restore` puts an archive of `backup create` back. `<archive>` is a file
path, or the name of a file in `~/versola-backups` (what `backup list` shows).
Only an archive of a **vps** deployment on **its own Postgres** is restored (the
manifest's `postgresMode` is `bundled`); with a Postgres of your own the command
says so and how to restore `database.dump` with `pg_restore` yourself, and
`local` is refused. The deployment is locked like for `configure`.

Before anything on the machine changes, the archive is read from end to end
against the sizes and SHA-256 of its manifest. A member that does not match, a
file that is cut short, a member the manifest does not list or this CLI does not
write, anything that is not a plain file, a newer archive format: refused. What
the manifest says (a version, a database name) is shown only when it is a plain
word, so an archive cannot put control characters on the terminal or arguments
into a command it prints.

It is one command that does what the machine needs, because the database cannot
be put back before the deployment's own Postgres container exists, and that
container comes from the bundle `configure` writes. So there are two steps, and
the output of the first names the next:

1. **A machine with nothing deployed** (a new server, or after `uninstall
   --purge`): OpenBao's data volume, its credentials in `~/.versola/openbao`
   (mode 0600) and an empty Postgres data volume (so that `configure` does not
   take the machine for one that lost its database). It replaces nothing: if
   the machine already has any of these (a credentials file, a volume, a
   container, an unfinished first install) it lists them and stops, naming
   `versola uninstall --purge` (which asks first). A failure, or a Ctrl-C,
   removes what the run had put back. It prints the `versola configure vps
   <version> ...` to run next, with the flags the archive's record holds
   (`--auth-url`, `--proxy`, `--central-url`, the public names,
   `--cert-lifetime-days`, `--est`, `--registry`, `--postgres-db`, and
   `--setup-openbao` when OpenBao was set up by hand, and `--acme-staging` when
   the deployment got its certificate from Let's Encrypt staging: the record
   does not keep it, so `backup create` writes it in the manifest; an archive made
before that has no such note, so add the flag yourself if the old server used it). The record itself is not
   put back: `configure` writes a new one.
2. **After that `configure`**, on the same Versola version as the archive, with
   the services stopped (`versola down`): the same command puts the database
   back. Another version is refused (a newer one would need migrations, an
   older one cannot read the schema), and so is a deployment whose OpenBao
   keys are not the archive's (the dump holds values only those keys open;
   compared by the unseal key, never shown), a rolling upgrade that is not
   finished, and running services. The deployment's own Postgres is started for
   it alone and stopped again after, as `backup create` does. The dump goes in
   through `docker exec` into the container, the password read from the
   container's own environment, never from a command line. The database is
   emptied first (every schema but the system ones is dropped with what is in
   it: a table a newer version made would otherwise stay, and the old
   version's migrations would fail on it) and the dump applied by
   `pg_restore --no-owner`, all between one `BEGIN` and one `COMMIT`, which is
   sent only when `pg_restore` has finished. When anything fails, a broken
   dump included, nothing is committed and the database is as it was. An empty
   database asks a plain
   `Continue? [y/N]` (`--yes` answers it). A database with data in it is
   refused unless `--replace-database` is given, and then asks you to type
   `vps`, which `--yes` does not answer (exit code 4); say what it would
   replace first and take a `backup create` if you want a copy.

Then `versola up`. `migrate` is not needed: the schema is the archive's, which
is the deployment's version (`up` prints its note that no migration is recorded,
because `configure` wrote a new record; the services check their schema at
start). For an archive taken during an unfinished rolling upgrade (the output
says so) the schema may be the older or the newer version's: `versola migrate`,
or `versola rollout --abort`, is then the next step.

**Restore only archives you made or trust.** The checksums are in the archive,
so they show damage, not who made it. The dump is run as SQL by the
deployment's own database role, and the `--registry` and `--auth-url` in the
printed `configure` come from the archive's record (the output says the command
is built from it), and so does `--acme-staging`: if browsers reject the
certificate after the restore, run `configure` again with `--acme-staging=false`.

**Move to another server.** On the old one `versola backup create`, and copy the
archive off the server. On the new one (the CLI installed, `versola doctor
--target vps ...` happy, the domain pointing at it): `versola backup restore
<archive>`, the `versola configure vps ...` it prints, `versola backup restore
<archive>` again, `versola up`.

**Go back after a failed upgrade.** With the archive taken before it:
`versola down`, `versola configure vps <the old version>`, `versola backup
restore <archive> --replace-database` (type `vps`), `versola up`.

Not restored: the generated bundle (`configure` writes it again), the ACME
certificate and mutual-TLS volumes (reissued), a Postgres of your own.

### `status`

Shows the deployed version and target (on `vps` also the recorded auth URL,
the proxy mode, marked when the certificate comes from Let's Encrypt staging,
and OpenBao when it was set up by hand -- what a `configure vps` without flags
keeps; and the registry, when it is not
Versola's own), when migrations last ran, the replicas of each service, the
progress of an unfinished rolling upgrade, and the containers with their
health (`docker compose ps` under the hood); on `vps` and `local` also the
`notAfter` of every mutual-TLS certificate and of each public name's own,
soonest first, marking those due for renewal (see `certs renew`).

Stopped containers are included. Both formats warn for each configured
central/auth/edge replica, proxy, OpenBao and Postgres container that is
missing, not running, restarting or unhealthy, with a command such as
`versola logs auth-2 --tail 100`. Optional services outside this Compose
deployment are not checked. Successfully completed init jobs are not service
outages; a running container without a healthcheck is not unhealthy.
`health: starting` means its healthcheck has not finished yet.
OpenBao retained from an earlier Compose project is checked by its
target-specific container name; `versola logs openbao` selects that container's
project. `containers[]` remains the current Compose project's snapshot.

```
versola status
versola status --json | jq .version
```

`--json` prints the same facts as one JSON object on stdout, for scripts and
health checks; nothing else goes to stdout. Fields are stable and additive:
new ones may appear, none is renamed or changes type; `schema` (now `1`) is
raised if that ever has to change, so a script can check it. It never contains a
secret, a password, a URL with credentials or a container's environment
(containers are read with a fixed `docker inspect` template).

- `schema`; `cliVersion`; `deployed` -- `false` when nothing is deployed (the
  object then has only `schema`, `cliVersion`, `deployed`, `containers: []` and
  `warnings`,
  and the exit code is 0, as for the text form).
- `target`, `version` (during a rolling upgrade: where it goes), `authUrl`,
  `centralUrl` (when recorded), `proxyMode`, `imageRegistry` (only when not
  Versola's own), `migratedAt` (RFC 3339, UTC; absent: never migrated).
- `rollout`: `{underway, from?, rollingBack}` -- `from` is the version an
  unfinished rollout comes from.
- `replicas`: per service (`central`, `auth`, `edge`) the recorded slots,
  `[{slot, version, standIn?}]`; the version is per slot because a rolling
  upgrade has two side by side, `standIn: true` marks its extra replica.
- `database`: `{mode, host}` -- `mode` is `bundled` (the deployment's own
  Postgres container; always on `local`) or `external`; `host` is the
  recorded `host:port`, or `null` (local).
- `containers`: every container of the deployment (services, replicas,
  reverse proxy, OpenBao, a bundled Postgres), stopped ones included:
  `[{name, service, slot?, state, health?, image, startedAt?}]`. `service` is
  `auth` for `auth-2` with `slot: 2`; services that are not replicated have
  no `slot`. `health` is there only while `state` is `running`: a stopped or
  restarting container, such as the one-shot `mtls-init` after its run, has
  none, and `state` tells what became of it. `containers` is `null` when
  Docker could not be asked: the object is still printed, with a warning, and
  the exit code is non-zero.
- `certificates` (vps and local): `[{name, notAfter?, renewDue, expired,
  error?}]`, soonest to expire first; `error` instead of `notAfter` for one
  that could not be read.
- `warnings`: the `WARNING:` lines of the text form (service problems above, certificates that should
  have been renewed, no renewal timer) and what could not be read, as
  sentences.

### `stats`

One snapshot of what every container of the deployment uses now, and on
`vps` whether a rolling upgrade's stand-in replica would fit -- in the
rollout's own numbers, so `stats` and the rollout's pre-flight never
disagree. Read-only. For a live view use `docker stats` with the names it
prints.

```
versola stats
versola stats --json
```

- One row per container (services and their replicas, reverse proxy,
  OpenBao, a bundled Postgres): CPU % (100% is one core, so a busy JVM can
  show more), memory used / limit and PIDs as `docker stats --no-stream`
  reports them; `--` for a container that is not running.
- `vps` only: the free memory (`MemAvailable`, which already counts every
  running container -- Postgres included -- as used) against what a stand-in
  needs (see "Before a stand-in starts" under `rollout`: the largest of what
  a running replica uses now and 900 MiB, plus 256 MiB), for the services a
  rollout would start a stand-in for, and a verdict:
  `free 2.34 GiB -- enough for one stand-in (needs 1.13 GiB: ...)`, or
  `not enough for a stand-in ...: a rollout would refuse; free memory first,
  or run it with --no-surge (...)`. Like the rollout it has to run on the vps
  itself; elsewhere (another OS, Docker pointed at a remote host) it prints
  why it can't tell instead of a verdict.
- `vps` only: the Postgres connections a stand-in would open against what
  the server takes (the rollout's `psql` check). It runs only when the
  `postgres:18-alpine` image is already on the machine -- `stats` never
  pulls it, and prints `not checked (runs during rollout)` instead. Progress
  goes to stderr with `--json`.
- `local` shows the container rows only: Docker Desktop's memory is not the
  machine's, and no rollout runs there.
- `--json` prints one object on stdout, stable and additive like `status
  --json`: `{schema, target, containers, host?, postgres?, warnings}`. `containers`
  is `[{name, service, slot?, state, cpuPercent, memUsedBytes, memLimitBytes,
  pids}]` (figures `null` when not measured; the list `null` when Docker
  could not be asked, with a non-zero exit). `host` is `{readable, reason?,
  memAvailableBytes?, standIn?}` with `standIn` `{services, service?,
  needBytes, thresholdBytes, measured, fits}` (`service` is the one that needs
  the most; `measured` is false when the 900 MiB floor applies). `postgres`
  is `{checked, reason?, freeConnections?, needConnections?,
  marginConnections?, service?, fits}`; the three counters are there, 0
  included, exactly when `checked` is `true`.

### `logs`

```bash
versola logs                         # the last 200 lines of every service
versola logs central --tail 100      # one service
versola logs auth -f                 # every replica of auth, until Ctrl-C
versola logs proxy --since 10m       # what the reverse proxy said in the last 10 minutes
```

Shows what the deployment's services print, on `local` and `vps`, without a
docker command, a container name or a compose file path: it is `docker compose
logs` for the compose project this machine tracks, with the same files `status`
uses, so the replicas, the reverse proxy and OpenBao are part of it.

- **`[service...]`**: the deployment's compose services, as Compose lists them
  (`central`, `auth`, `edge`, `proxy`, `openbao`, `step-ca`, `postgres` where the
  deployment has one, ...), or a replica as `status` shows it. `auth` is every
  replica of auth (`auth` and each `auth-N`, also a stand-in of a rolling
  upgrade), `auth-3` only slot 3, `auth-1` slot 1. A name the deployment does
  not have is refused with the list of those it has. `postgres` on a vps that
  uses your own Postgres (`--postgres-host`) says so: its logs are on the
  server that runs it. No service: all of them, each line prefixed with its
  service; with exactly one service the prefix is left out, so
  `versola logs auth-3 | jq` reads the service's own lines.
- **`--tail <n|all>`**: lines from the end of each service's log, 200 by
  default, also with `--follow`; `all` for the whole history.
- **`-f`, `--follow`**: keep printing new lines; Ctrl-C ends it, with exit
  status 0.
- **`--since <duration|time>`**: only lines since then (`10m`, `2h`,
  `2026-11-02T10:00:00`), as `docker compose logs` takes it.
- **`-t`, `--timestamps`**: when each line was written.

Nothing deployed on this machine: it exits 1 and says to run `versola
bootstrap` first. It takes no lock, like `status`, so a `logs -f` left open
never holds up a `configure`, `up` or `rollout`.

**Mind where you paste it.** The output is the services' own, unchanged: the
CLI neither adds to it nor filters it, and a service can print what it should
not. step-ca prints its administrative password on its first start, so
`versola logs step-ca --tail all` shows it: do not paste that into a ticket or
a chat. When `up` or `rollout` reports that a service never became ready,
the error ends with the `versola logs` command to run.

### `admin reset-password`

```bash
versola admin reset-password        # a new first-login link for the bootstrap admin
versola admin reset-password --yes
```

For a first login link that was lost, opened by something else (a link preview
counts) or has expired. It prints a new one-time link; open it in a browser and
set the admin's permanent password. The link is shown once and stored nowhere:
not in `state.json`, not in a file.

It recreates auth's first replica twice -- first with `BOOTSTRAP_RESET=1`, which
makes auth drop an unused temporary password and link and issue new ones, then
without it, so that later starts do not reset again -- and reads auth's log for
the link. A plain restart would change nothing: auth keeps a still-valid
temporary password, and reads the variable from its container's environment,
which `docker restart` does not renew. Sign-in is interrupted for a moment when
auth has one replica; other replicas keep serving. On vps it asks for
confirmation (`--yes` answers it).

- **The admin already has a permanent password:** nothing is reset and the
  command fails saying so; a forgotten permanent password is recovered through
  the product's own account recovery.
- **No link in auth's log** (a release without first-login links, a
  deployment configured without one, or auth slower than 20 seconds): it says so,
  names where `ADMIN_BOOTSTRAP_PASSWORD` is kept (the path, never the value) and
  exits 0. Run it again if auth was only slow: the link of the run that missed
  it is gone with the container.
- **The second recreate fails** (or auth does not become ready after it): the
  error says to run `versola up`, which also repairs an auth left with
  `BOOTSTRAP_RESET` set (until then it resets again on every start); no link is
  printed.
- **Ctrl-C or a dropped SSH session** does not interrupt the sequence: the
  command finishes restarting auth without the reset first.
- **local:** the admin login is fixed (`admin / Admin1234!`); it is printed and
  nothing is restarted.

Not covered by tests: a run against real containers and a real auth release (the
unit tests use a fake Docker); that needs a versola-tools release with versolauth/versola#904.

### `down`

Stops the deployment this machine tracks, local or vps. Nothing is deleted
by default, so a later `bootstrap` or `up` picks up the same data.

On `vps` it asks first, naming the version, the auth URL and the services it
stops (the container's Postgres among them, when there is one), and that the
domain stops answering until `versola up`; `-y`/`--yes` prints the same line
and skips the question. `local` never asks.

`--volumes` also deletes the stack's own volumes. On `local` that is the
Postgres data volume. On `vps` it is the mutual-TLS CA and certificate
volumes and the diagnostics volumes (heap dumps, GC logs); the next `up` creates
a new CA and issues every certificate again. The Postgres,
OpenBao and TLS certificate volumes are `external` and are never removed by
`down`, and the question says so.

While a rolling upgrade is under way it refuses (see `rollout`); `--force`
stops the stack anyway.

### `uninstall`

Removes everything versola deployed locally: stops the stack and deletes
its Postgres volume and OpenBao's data volume, removes the `versola-*`
images that were pulled, and clears `~/.versola`. Prompts for confirmation first — pass `-y`/`--yes`
to skip that. Any answer but yes prints `Aborted.`, removes nothing and
exits 4 (versions before this one exited 0 there).

OpenBao's volume is removed only if it exists; one that is already gone is
not mentioned. The volume an older versola-cli shared between `local` and `vps`
(`versola-openbao-file`) is never removed, `--yes` or not: it may hold the
secrets of an old vps install on the same machine. If it exists, `uninstall`
names it at the end with the `docker volume rm` command for it.

On `vps`, OpenBao's data volume (`versola-openbao-file-vps`: every secret
and signing key), the Postgres data volume (`versola-postgres-data-vps`, when
Postgres runs in a container) and the TLS certificate volume
(`versola-acme-vps`) are kept: they are a live server's data, and so is
`~/.versola/openbao/vps-admin.json` (the root token and unseal key of that
OpenBao: it opens every secret in it, so keep it private and in your backup).
`uninstall` names the ones it kept (the file only if it exists, never its
content) and ends with `To start over from scratch: versola uninstall
--purge`, also when there is no deployment record any more.

On `vps` it also removes the daily `certs renew` timer (`versola-certs-renew`
in `~/.config/systemd/user`: `systemctl --user disable --now`, the two unit
files deleted, `daemon-reload`), plain or with `--purge`, and lists it among
what it will remove. A later `configure`/`bootstrap` installs it again. If
systemd cannot be asked to stop it (no user session bus over plain ssh), the
files are removed anyway and the output says so. A crontab line you added
yourself is not touched. Not removed while Docker is unreachable: the stack
may still be running, and the certificates it uses need the timer. When
the deployment used a Postgres of your own, it also prints that Postgres's
address and the flag that gives it back (`--postgres-host <host:port>`): the
record that remembered it is removed.

#### `uninstall --purge`

Starting over from scratch, typically after a failed first attempt on a VPS:

```
versola uninstall --purge
versola bootstrap vps <version> --auth-url ...
```

On top of the above it destroys, after listing them by name:

- OpenBao's vps volume: every secret and signing key, so whatever was
  encrypted with them can never be read again;
- the bundled Postgres volume: the database (users, clients, sessions);
- this machine's OpenBao files for vps in `~/.versola/openbao`: `vps.json`
  (AppRole), `vps-admin.json` (root token and unseal key),
  `vps-postgres-role.sql`, `vps-postgres-init.sql`. Other targets' files stay;
- any `.tmp-*` file in that directory: what a write cut off before its rename
  leaves, possibly a partial copy of a credentials file (named, never read).

The list also names Versola's vps containers, which are removed first,
running or stopped (a stopped one still holds its volume): OpenBao's and
Postgres', and every container of the vps compose project (`versola-vps`:
auth, central, the proxy). The latter matter when a plain `uninstall` already
removed the deployment record: with no compose file to `down` them, the
services of the earlier run would stay up and hold the ports the next `up`
needs. If one is running and this machine has no deployment record, it warns
before the question: that is also what a live server whose `~/.versola` was
lost looks like. If Docker cannot say whether a volume or a container exists,
nothing is removed.

Only what exists is listed and removed. It asks you to **type `vps`**; any other
answer aborts with an error (exit code 4) and removes nothing, and `--yes` does not answer
it (a script feeds `vps` on stdin). It works without a deployment record (after
a plain `uninstall`), needs Docker running, and is refused when the record on
this machine is `local` (a plain `uninstall` already removes local's data).

Kept either way: the TLS certificate volume (reusing it avoids Let's
Encrypt's rate limits; `docker volume rm versola-acme-vps` if you really
want it gone) and a Postgres of your own (`--postgres-host`), which is never
touched: drop and recreate its schemas yourself, since what Versola stored in
it was encrypted with the destroyed keys.

The order never leaves OpenBao's secrets without the files that open them:
stack, then Versola's vps OpenBao and Postgres containers (running or
stopped, a stopped one still holds its volume), then OpenBao's volume, then the
files, then the Postgres volume. If a step fails it stops there and says so;
running `versola uninstall --purge` again continues with what is left.

Never run it on a server whose `~/.versola` was lost while Versola ran there:
the configure error for that case (`unknown-state`) says so too.

If a deployment was recorded but Docker isn't reachable to confirm it's
actually stopped, `~/.versola` is deliberately left in place rather than
cleared — deleting it there would remove the only way to properly stop
that deployment once Docker is reachable again.

After a failed first install (`configure` began writing secrets and did not
finish: no deployment record, only the note `secrets-pending.json`) a plain
`uninstall` keeps that note, so `versola configure vps ...` afterwards resumes the
install instead of refusing with `unknown-state`; `uninstall --purge` is for a
real fresh start and removes the note with the rest. The note is kept only when
this machine has no record at all: beside a record it is a leftover of a
finished install and is removed, so a live server whose record is lost still
reads as `unknown`.

Removing `~/.versola/active` is the last step. If it fails on a permission
error (Docker made a file there as root: a bundle written by an older version
mounted a file that did not exist yet, and Docker made a directory of it), the
error says the stack, volumes and images are already removed and ends with
`sudo rm -rf '<path>'`, the one command that finishes it.

Does **not** remove the `versola` binary itself or its PATH entry;
safely deleting a program's own running executable isn't portable across
platforms (Windows won't allow it at all). It prints the binary's path
so you can remove it yourself.

### `upgrade`

```
versola upgrade                  # update to the latest release
versola upgrade --check          # only say whether a newer release exists
versola upgrade --check --json   # the same for scripts
versola upgrade --version 0.3.3  # install exactly this release
```

Checks the latest versola-cli release on GitHub and, if it's newer than
this binary's version, downloads it — verified against that release's
published `checksums.txt`, the same way install.sh/install.ps1 verify
their own downloads — and replaces the running executable in place.
Prompts for confirmation first — pass `-y`/`--yes` to skip that. A binary
that is already ahead of the latest release (a pre-release, say) is not
offered a "downgrade": it says so and stops.

`--check` changes nothing. It prints `versola-cli 0.3.3 is available (you
have 0.3.2): run versola upgrade`, or `versola-cli 0.3.2 is current`, and
exits 0 in both cases (a script reads the answer, not the code); it exits 1
only when GitHub can't be reached. With `--json` it prints one object on
stdout, `{"current": "0.3.2", "latest": "0.3.3", "upToDate": false}`;
`--json` goes with `--check` only (exit code 2 otherwise). On a `dev`
build `upToDate` is always `false`.

`--version <release>` installs exactly that release instead of the latest
(`0.3.3`; a leading `v` is accepted and dropped). It takes the same
download, checksum check and confirmation as a plain `upgrade`, without
asking GitHub which release is the latest. A pre-release (`0.4.0-rc1`) is
installed too, with a warning. A release older than the running binary is
installed after a warning that a newer versola-cli may have written
deployment state (`~/.versola/active`) an older one can't read (it stops
with a "newer schema" error, see "Upgrading the CLI" above). When the state
on this machine is in layout 5 (see above) that is no longer a "may": the
warning, read-only (`upgrade` looks at the record and prints nothing from it),
says that the older CLI will stop on every command that reads the state, that
nothing is lost, and gives the way back (the install script with this release's version, for macOS/Linux and
Windows; `versola upgrade --version` only if the older release has that flag), and
the question becomes
"Install it anyway?" (`--yes` answers it). In layout 4 the warning is the
generic one. What isn't a
version (`latest`, a path, anything but `X.Y.Z` or `X.Y.Z-pre`) is refused
with exit code 2 before anything is downloaded; a release that doesn't exist says so and points at the
releases page. `--check` and `--version` don't go together (exit code 2).
There is no channel flag: builds of the team's staging are installed with
`--version`.

#### The daily notice

Once a day, any command run in a terminal asks GitHub (one request, 2
seconds at most) whether a newer versola-cli exists, and if one does, prints
one line to **stderr**, after the command's own output and only if the
command succeeded:

```
A newer versola-cli is available: 0.3.3 (you have 0.3.2). Run versola upgrade.
```

It never replaces the binary: only `versola upgrade` does. It is never
printed, and GitHub is never asked, for `version`, `upgrade`, `help`,
`completion`, `uninstall` (`--purge` removes `~/.versola`, which the cache would recreate), `--json` output, a `dev` build, when stdout or stderr is not a
terminal, or when `CI` is set. `VERSOLA_NO_UPDATE_NOTIFIER` set to any value
(even empty) turns it off. The lookup starts before the command runs, so a
long command such as `bootstrap` has the answer by its end and a short one
waits for it at most to the 2-second budget; a lookup that fails (no
internet, GitHub down) is silent, doesn't fail the command, and counts as
done for the day, so a server without outbound access loses the 2 seconds
once a day, not on every command. The line is shown at most once every 24
hours, not on every command until you upgrade. The cache is
`~/.versola/update-check.json` (`checkedAt`, `latest`, `notifiedAt`; no
secrets, mode 0600); deleting it only means one more lookup.

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
deploys — that one is whatever you pass to `bootstrap local [<version>]`
(see `versions` for those).
Binaries built from source without release flags report `dev`.

### `versions`

```
versola versions                    # the 10 newest releases
versola versions --all --limit 30   # pre-releases too, 30 of them
versola versions | cut -f1          # just the version numbers
```

Lists Versola's releases (Versola itself, not versola-cli) on stdout,
newest first, one per line, as three tab-separated columns: the version,
its release date (UTC), and markers -- `latest` (the release `bootstrap
local` deploys without a version), `deployed` (the version this machine's
record names, local or vps) and `pre-release`. The markers column may be
empty. Pre-releases are left out unless `--all` is given. `--limit N`
shows N releases (default 10, at most 100: one page of GitHub's API).

It asks GitHub's public API without a token (60 requests an hour per
address is plenty). If GitHub can't be reached it fails with the address
of the releases page instead; a failed `latest` lookup or an unreadable
deployment record only leaves that marker out.

## Exit codes

Every command prints its error once, on stderr, and exits with a code a
script can act on:

| Code | Meaning | Examples |
|---|---|---|
| 0 | Success | |
| 1 | Failure not listed below | a docker command failed, a migration failed, a service never got ready |
| 2 | Wrong arguments or flags | unknown command or flag, wrong number of arguments, an unknown target for `configure`/`bootstrap` or `doctor --target`, a flag that only applies to vps given on `local`, a missing `--auth-url` on vps, a bad value (`--service`, `--cert-lifetime-days`, a replica count) |
| 3 | The machine isn't ready | `doctor` found problems; `configure`'s/`bootstrap`'s prerequisite checks failed (Docker, memory, disk, a port already in use) |
| 4 | Not confirmed | anything but yes at a `Continue? [y/N]` (or no terminal and no `--yes`), at `uninstall`'s or `upgrade`'s question, at `postgres init`'s, or a purge or a `backup restore --replace-database` without `vps` typed |
| 5 | The secrets plan has problems | `secrets plan`; `configure`, `bootstrap` and `rollout` when their secrets plan has problems (nothing is written to OpenBao) |
| 130 | Interrupted | Ctrl-C during `rollout` (the first one stops between two steps, later ones leave sooner), during `backup create`'s OpenBao copy, or between the steps of `backup restore` (what it had put back is removed) |

A problem found inside a command that has no code of its own is 1.

`status` and `status --json`, 0 means the status was read successfully,
even when a service is down or unhealthy; monitoring scripts must inspect
`containers` and `warnings`. Failure to read the containers or the configured
service inventory exits 1. Nothing deployed still exits 0.

`migrate --dry-run` exits 0 whenever it could report, with or
without pending migrations, and 1 when it couldn't. Validation errors raised deeper in deployment or secrets handling stay 1,
such as an invalid `--auth-url` or an unsupported `secrets plan` target;
`secrets test` errors without a class also stay 1. A Ctrl-C while `secrets plan` or `rollout --dry-run` runs
versola-tools exits 130 (SIGTERM 143, SIGHUP 129), as the shell does.

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
internal/cmd/           cobra commands (doctor, bootstrap, configure, migrate, up, status, stats, logs, down, replica, rollout, secrets, postgres, backup, certs, edge, uninstall, upgrade, version, versions)
internal/checks/        the actual check logic doctor (and configure) run
internal/deploy/        the deployment steps themselves — configure, migrate, up, replicas, rollout — which bootstrap chains together
internal/secrets/       the secret schema reader and the plan (secrets plan / configure / rollout)
internal/openbao/       OpenBao client, provisioning, stored credentials
internal/proxy/         the reverse proxy's generated nginx config (local, vps nginx/external)
internal/topology/      ports and slots read from the compose file (docker compose config)
internal/state/         locates ~/.versola/active, the on-disk state configure writes (state.json, layout 4 or 5)
internal/docker/        thin wrappers around the docker CLI
internal/fsutil/        atomic file writes
internal/release/       GitHub releases API: the latest release and a list (upgrade, versions, bootstrap local without a version)
internal/wait/          polls a readiness endpoint until it answers 200 or times out
internal/browser/       opens a URL in the default browser — used by bootstrap (admin console) and doctor (install pages)
e2e/rollout/            manual end-to-end run of rollout with stand-in images (not in CI)
install.sh              one-line installer for macOS/Linux
install.ps1             one-line installer for Windows
LICENSE                 Apache License 2.0 (also published as a release asset)
.github/workflows/      ci.yml (vet/build/test on every push+PR), release.yml (builds binaries + checksums on tag push)
.claude/                Claude Code setup for contributors: hooks (hooks/claudehook), /check and /release-prep skills, the versola-cli-reviewer agent
```

## License

`versola-cli` is licensed under the [Apache License 2.0](LICENSE).
Copyright 2026 Georgii Kovalev.

This covers the CLI only. The Versola server images it deploys
(`ghcr.io/versolauth/versola-*`) are distributed under the
[Versola Community License v1.0](https://github.com/versolauth/versola/blob/main/LICENCE.md);
installing the CLI does not grant any rights to them.

The Versola name and logo (including the files in `docs/brand/`) are not
licensed under Apache 2.0.
