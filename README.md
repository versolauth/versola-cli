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

`doctor`, `bootstrap`, `configure`, `migrate`, `up`, `status`, `down`,
`secrets`, `uninstall`, and `version` are all implemented and have been
tested end-to-end against a real local deployment (Postgres + auth +
central + edge + the reverse proxy, real browser login through to the admin
console). `replica`, `rollout` and `certs` are implemented too (see their
sections below); that local test did not cover them. Releases are
automated (see Releasing below) and installed via the one-line scripts
below.

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
- With `--target vps` and `--auth-url` and/or `--central-url`: central's
  second public name (see "Central's second name" below) resolves, to
  this server, and answers a TLS handshake on its port 8093 and, if the
  URL has no port, on 443. Meant for a server where central already runs
  (after `versola up`): before that there is nothing to answer
- With `--target vps`, on a host that already has a deployment: the
  mutual-TLS certificates in the `versola-mtls-vps` volume are not
  expired and not within the last third of their lifetime (a warning
  then; see `certs renew`)
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
  it also needs `--auth-url` and `--postgres-host` (see `bootstrap --help`),
  and accepts `--postgres-db` and `--postgres-ca`.
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

The answer is read from stdin one line at a time, never further than the
newline, so a script can feed several answers to consecutive commands
(`printf 'y\ny\n' | { versola configure vps ...; versola migrate; }`).
Pasting several commands into an interactive terminal is different: the shell
has already taken the next line as typed-ahead input, and the prompt reads it
as the answer. To avoid prompts in scripts, pass `-y`/`--yes` to `configure`,
`migrate`, `up`, `bootstrap` and `replica add|remove`; `rollout`, `upgrade` and
`uninstall` accept it too. With `--yes`, `configure`, `migrate`, `up`,
`bootstrap`, `replica` and `rollout` still print the "This will …" line (so a
script's log shows what it agreed to) and then go on without asking.
`local` never asks about its own deployment, so `--yes` changes nothing there.
The one exception it does not cover: `configure local` / `bootstrap local` on a
machine whose record still points at a running VPS deployment ask before
replacing that record, even with `--yes`, because replacing it leaves `status`,
`down` and `uninstall` unable to find the VPS containers. In a script that
question is answered by stdin, or the command aborts.

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

Secret **values are never printed** by `configure`, `bootstrap`, `rollout`, or
`secrets plan` — only names, decisions and where a value is kept:

- OpenBao's own AppRole `role-id`/`secret-id`: `configure` prints the path,
  `~/.versola/openbao/<target>.json` (readable only by you).
- The Postgres password generated on the first `configure` against a new
  OpenBao: `configure` writes the SQL that sets it on Versola's role to
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

This CLI writes its state file in `~/.versola` in **layout 4** (layout 2
added the schema revision; layout 3 the Postgres host and the records a
rolling upgrade needs; layout 4 the Postgres database name and CA, from
`--postgres-db` and `--postgres-ca`). It reads the layouts older CLIs wrote,
but an older CLI cannot read layout 4 and stops with a "newer schema" error: after the
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

  Postgres can also be a remote, TLS-only server: `--postgres-db <name>`
  picks the database (default `auth`) and `--postgres-ca <file>` is a PEM
  file with the CA that signed its certificate. Both are vps-only and are remembered
  (the CA file is copied next to the deployment's state): `rollout` reuses
  them, and a later `configure` without the flag keeps the stored value. They need a
  `versola-tools` release that understands `POSTGRES_DB` /
  `POSTGRES_CA_FILE`.

  For a test deployment, add `--acme-staging`: the certificate then comes
  from Let's Encrypt's staging environment — untrusted by browsers, but
  without production's rate limits (a handful of certificates per domain
  per week).

  The name central is reached at (see "Central's second name" below) also
  needs a DNS record pointing at this server: `central.<domain>`, next to
  `--auth-url`'s name.

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

  Versola's proxy doesn't hold 443 here, so it can't route central's
  second name; your web server has to (see "Central's second name").

### Central's second name: `--central-url`

A deployment is addressed by two host names: `id.<domain>` (auth,
`--auth-url`) and `central.<domain>` (central's edge surface, which edges
reach over mutual TLS). `--central-url` (on `configure` and `bootstrap`,
vps only) is that second one, as `https://<name>[:<port>]`. Left out, it is
`https://central.<domain>`: `--auth-url`'s host with its first label
replaced (`id.example.com` and `example.com` both give
`central.example.com`); `configure` passes it to versola-tools as
gen-env's `--central-mutual-tls-url`, so it is what an edge's
`central.url` is generated with, and records it. Nothing is derived from
an `http://` or IP `--auth-url`: versola-tools keeps its own default
(`https://127.0.0.1:8093`, the host itself). Pass the flag to override
the name or to point at central's own port instead
(`https://203.0.113.7:8093`: nothing routes it then).

The name's DNS record points at this server. No public certificate is
needed for it: central's server certificate comes from the deployment's
own PKI and the edge pins it.

With `--proxy nginx` the proxy routes the name itself. Port 443 is then
shared by TLS it terminates and TLS it must not touch, so it is a stream
(TCP) server that reads the server name from the ClientHello
(`ssl_preread`):

- `central.<domain>` goes, as bytes, to central's mutual-TLS listener on
  `127.0.0.1:8093` -- the handshake and the edge's client certificate
  reach central untouched;
- every other name -- `--auth-url`'s -- goes to the http server it was
  always served by, which moves to `127.0.0.1:8444` and keeps the ACME
  certificate. Port 80 is unchanged, so Let's Encrypt's HTTP-01 challenge
  works as before. The stream server tells it who the client is in a
  PROXY protocol header (the http server trusts only that loopback peer),
  so auth's per-IP throttling still sees real addresses.

Central's connection takes a hop through a loopback relay
(`127.0.0.1:8445`) that drops that header, which its TLS listener would
take for a broken handshake. All three loopback ports must be free;
`configure` checks. Port 8093 stays open on all interfaces, as before, for
edges that use the host address: central itself listens there on a
deployment without the proxy's front of it, and the proxy does (balancing
over central's replicas, see below) on one with it.

The generated file is `proxy/stream.d/central-mtls.conf` in the proxy's
config (see `~/.versola/active/proxy-vps/current/`).

With `--proxy external`, Versola's proxy only listens on
`127.0.0.1:2821` and doesn't see 443, so the web server in front has to do
the same, without terminating TLS for this name -- for nginx, a `stream`
block in the main context sharing 443 with the `http` server that handles
`--auth-url`'s name:

```nginx
stream {
    map $ssl_preread_server_name $versola_tls {
        hostnames;
        central.example.com 127.0.0.1:8093;   # central's mutual-TLS listener
        default             127.0.0.1:8443;   # your existing https server, moved off 443
    }
    server {
        listen 443;
        ssl_preread on;
        proxy_pass $versola_tls;
    }
}
```

(Your `http` server for the auth name then listens on 8443 and, to keep
the client address, uses `proxy_protocol` on both ends; or leave the
address out of it.) Until that is in place, edges outside this host can use
the host's address and port 8093 directly: `--central-url
https://<host>:8093`.

`versola doctor --target vps --auth-url ...` checks the name resolves to
this server and answers a TLS handshake on 8093 (and 443).

This needs versola-tools to read `--central-mutual-tls-url` from the
arguments it is run with; an image that does not keeps its default
(`https://127.0.0.1:8093`) and the proxy routes the name regardless.

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
compose file sets `MPORT: 8093` for central, with `MBIND_HOST: "0.0.0.0"`;
versola#607) gets that listener moved the same way: the proxy listens on
8093 on every interface (the address edges and the `central.<domain>` relay
below use, unchanged) and balances over the central replicas, which listen
on the loopback 20000 higher (28093; 28193 for a second replica) --
`overrides.yml` and `replicas.yml` set `MPORT` and `MBIND_HOST: "127.0.0.1"`
for them. Port 8093 is then the proxy's: `configure` and `up` refuse another
service on it, and a rollout's central stand-in takes its own pair of ports
(28190/28191, 28193) instead of fighting the running central for 8093. A
release whose central has no `MPORT` in its compose file (0.7.x) is
untouched: the proxy listens on nothing there. Moving the listener's port
between two releases is refused by `rollout` like any other moved port;
gaining or losing it is not.
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

### `rollout <version>` / `rollout --abort` (vps)

`versola rollout <version> --assume-compatible` moves a running vps
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
a dropped SSH session), the same command continues; progress is read back
from `state.json` and the containers. `--dry-run` shows the plan, the
secrets plan and the pending migrations and changes nothing. `--no-surge`
replaces replicas without a stand-in (one fewer serves meanwhile; with one
replica it needs `--allow-downtime`; central is restarted in place, as
before stand-ins).

**Compatibility is yours to confirm.** During a rollout the running
version works on the new version's schema, and an abort runs it there
alone. Versola releases before 1.0 don't promise that ("compatibility
between 0.x releases is not guaranteed"), so `rollout` refuses to start
without `--assume-compatible`: read the release notes first (additive
migrations; the running version works on the new schema). A release that
is not compatible is deployed with `configure`, `migrate` and `up` (a
short stop) after a database backup. **0.7.0 needs a fresh database**
(its migrations were squashed): a deployment on 0.6.x can't be rolled out
to it, and moving to 0.7.0 means installing it on an empty schema.

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
neither):

- memory: free memory (`MemAvailable`) must hold one more replica --
  what a running replica of that service uses now (`docker stats`), never
  less than 900 MiB (since 0.7.0 a replica's JVM is sized at about
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
  is on by default; nothing on id.versola.kz uses it today.
- every edge replica the rollout starts (the stand-in, each recreated
  slot, again on an abort) gets its own client certificate through
  central.

#### Runbook: a rolling upgrade on the vps

1. Read the release notes of the new version: additive migrations, the
   running version working on the new schema. If not, this is a
   `configure` / `migrate` / `up` deployment instead (and 0.6.x -> 0.7.0
   needs a fresh database).
2. Back the database up (`pg_dump` of each schema) -- migrations can't be
   rolled back -- and copy `~/.versola/active/state.json`.
3. `versola rollout <version> --dry-run`: the plan, the checks, the
   secrets plan, the migrations it would apply. Nothing changes.
4. `versola rollout <version> --assume-compatible`. Cut off, run the same
   command again; `versola status` shows where it stands.
5. Check through the proxy (`curl -H 'Host: <your domain>'
   http://127.0.0.1:2821/.well-known/openid-configuration` and the admin
   console).

Going back: `versola rollout --abort` while the rollout is unfinished
(add `--no-surge` if the rollout needed it); once it finished, roll the
earlier version out the same way. If the old version can't run on the
migrated schema at all, restore the database from step 2 and deploy the
old version with `configure` / `migrate` / `up`.

### `certs renew` (vps and local)

Auth's and central's mutual-TLS listeners and the certificate edge
presents to central are issued by the deployment's own CA (step-ca) into
the `versola-mtls-vps` volume (`versola-mtls-local` locally), valid for two
years. `versola certs renew` keeps them valid, the way cert-manager does
on Kubernetes: it runs versola-tools' `mtls-init` service as a one-off,
which re-issues a certificate only when a third of its lifetime or less is
left, and -- if it issued anything -- restarts `central`, `auth` and
`edge` (every replica, in that order, waiting for each to be ready before
the next), because they read their certificates at startup. When nothing
is due it prints nothing and exits 0; a failing step exits non-zero with
its output. Nothing needs registering in central: an edge is recognised by
the URI name in its certificate.

If a restart fails after the certificates were renewed, the error names
the services still running on the old ones; once the problem is fixed,
`versola certs renew --restart` restarts them (a plain `renew` finds
nothing due by then).

The edge pins central's server certificate. An edge on the same host
mounts the volume and picks the new one up with its restart; an edge on
another host has to be given the renewed `central-server.crt` (and, when
`ca.crt` changed, the CA bundle) before its next reconnect.

Run it on a schedule -- daily is plenty:

```
versola certs renew --show-timer      # prints what it would write; writes nothing
versola certs renew --install-timer   # writes + enables it (systemd, current user)
```

The timer is a systemd user timer (`~/.config/systemd/user/versola-certs-renew.{service,timer}`,
`OnCalendar=daily`, `Persistent=true`); on a host without systemd
`--show-timer` prints a crontab line instead, which is yours to add. A
user timer stops with the user's last session unless lingering is on
(`sudo loginctl enable-linger $USER`).

### `status`

Shows the containers from the last `bootstrap` run and their health
(`docker compose ps` under the hood).

### `down`

Stops the locally deployed stack. Pass `--volumes` to also delete the
Postgres data volume (kept by default, so a later `bootstrap` picks up
the same data). While a rolling upgrade is under way it refuses (see
`rollout`); `--force` stops the stack anyway.

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
internal/cmd/           cobra commands (doctor, bootstrap, configure, migrate, up, status, down, secrets, certs, uninstall, upgrade, version)
internal/checks/        the actual check logic doctor (and configure) run
internal/deploy/        the deployment steps themselves — configure, migrate, up — which bootstrap chains together
internal/state/         locates ~/.versola/active, the on-disk state configure writes
internal/wait/          polls a readiness endpoint until it answers 200 or times out
internal/browser/       opens a URL in the default browser — used by bootstrap (admin console) and doctor (install pages)
install.sh              one-line installer for macOS/Linux
install.ps1             one-line installer for Windows
LICENSE                 Apache License 2.0 (also published as a release asset)
.github/workflows/      ci.yml (vet/build/test on every push+PR), release.yml (builds binaries + checksums on tag push)
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
