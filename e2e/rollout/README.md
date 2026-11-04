# Rollout e2e

Checks `versola rollout` end to end with the real CLI, the real proxy (nginx),
the real OpenBao and the real compose files from `versola-tools`, and stub
services in place of Versola's own:

```
stub/     auth/edge/central stand-ins: same ports and probes, every reply
          carries X-Stub-Version; TLS on MPORT; slow readiness/shutdown knobs
tools/    wraps the real versola-tools image: runs its real entrypoint, then
          points the generated compose file at the stub images; `migrate`
          is a no-op (no Postgres needed)
traffic/  GET+POST client for the proxy's public port (127.0.0.1:2821),
          8082, 8083 (new and long-lived connections) and central's front
          8090 (left out with --no-surge: central restarts in place); counts every
          transport error and unexpected status, and on the long-lived 8083
          connections tells cuts and reconnects apart (below)
mtls-client.sh  --real only: a client and its EST certificate for 8083 (below)
run.sh    builds the images, deploys version A, rolls out version B under
          traffic, prints the report
```

**Pass** = `versola rollout` exits 0, the traffic client reports zero
failures, and every target was served by both versions (`-expect-versions`).
Cuts (below) are reported but do not fail the run; `--strict` makes them
failures.

Connections to 8083 are the known limitation. 8083 is a TCP pass-through
(nginx `stream`): after every reload the retired workers keep the connections
they had, and nginx closes all of them at `worker_shutdown_timeout` (30 s),
idle or not -- a stream proxy can't tell where a request ends. edge keeps
pooled connections there (zio-http; Versola's default pool is `Fixed(128)`
per target, with no maximum age), so each reload ends edge's pooled
connections to 8083 30 s later. The client keeps three pooled connections
there, one per pooled target -- one of them (`/slow?ms=1500`) always has a
request in flight -- and counts two things apart:

- **reconnect**: the pooled connection was closed while idle and the next
  request opened a new one. zio-http does the same (it drops a closed channel
  it takes from its pool and opens another), so no request of edge's fails.
  Counted, harmless.
- **cut**: the connection died under a request -- in flight, or between
  taking it from the pool and writing. net/http retries a GET on a dead reused
  connection by itself, which would hide it, so a request answered only after
  a second connection was taken counts too. A timeout, a retry that finds no
  new connection, or a failure on the retry's new connection is a failure,
  not a cut. zio-http does not retry: edge's request fails (on vps, 8083
  carries the native-app back channel: `/par`, `/token`, `/revoke`).
  `/token` with an authorization code is single-use, so a blind retry would
  not be safe either.

A local run of this client through nginx 1.24 `stream` with two stub
upstreams and three reloads (`worker_shutdown_timeout` 5 s) gave 3 cuts, all
on `/slow`, 0 on the short requests and 6 reconnects -- the old counting
reported the same run as 9 failures. Not yet re-run on the stand.

Because cuts are expected, a default run no longer fails on them -- so it
does not catch a regression that only cuts established 8083 connections
(say, the proxy being restarted instead of reloaded). Run with `--strict`
to look for exactly that; the plain targets still catch any outage of new
connections.

Plain POSTs send `Connection: close`: a keep-alive connection that a reload
closes at the very moment a POST is written fails whatever the CLI did.

The traffic client is a Go module of its own (`traffic/go.mod`), so CI's
`go test ./...` at the repository root does not run its tests: run
`go test -race ./...` in `traffic/` by hand.

## Real images (`--real FROM TO`)

The first step of versola-cli#84, "rollout by default only after a run on real
images under load". `./run.sh run /path/to/versola --real 0.7.5 0.7.6` deploys
the released images FROM with the bundled Postgres (no `--postgres-host`, as a
new customer does; FROM and TO are releases or test tags the `versola` CI
publishes: its CI/CD workflow, run by hand with a `test_tag`, builds the images
from a branch and pushes them under that tag, and two tags of one commit do for
a run of the mechanics), rolls out to TO and runs the traffic client with `-real`
meanwhile. The stubs are not built, `migrate` is the real one, and the rollout
runs without `--allow-legacy-secrets` and without `--assume-compatible`: TO's
own `release.json` decides (pass `--assume-compatible` to override it, the
way an operator would). `--replicas`, `--no-surge` and `--abort-at` work as
with stubs; `--manifest` and `--slow-central` are for stubs only.

What differs from the stub run:

- The client's requests are the ones a real service answers without
  credentials: `/.well-known/openid-configuration` and `/.well-known/jwks.json`
  (200), edge's `/permissions/me` (401: what the CLI itself expects of edge
  behind the proxy), `POST /token` with no credentials, 8082 and central's
  front 8090. For the last three any status below 500 is an answer; 5xx and
  transport errors are failures everywhere. Their exact status was not seen on
  a real run, so it is not asserted. 8083 is added below.
- Real answers carry no version, so the version is read from the containers:
  after `up` every central/auth/edge container must run FROM, at the end TO
  (FROM again after an abort). The client does not prove that traffic ran on
  both sides of the rollout as it does with stubs (`-expect-versions`); only
  that it ran, without a failure, for as long as the rollout did.
- Needs 5432 free and the volume `versola-postgres-data-vps` absent; `clean`
  removes that volume, and only with `E2E_CONFIRM_CLEAN=yes` (its data is lost).
  `clean` does not remove the pulled `ghcr.io/versolauth/versola-*` images of
  FROM and TO (it knows only the stub tags): `docker rmi` them by hand if the
  stand runs short of disk.

### Load and numbers (`--rps N`)

`--rps 30` sends the plain targets at 30 requests per second in all, at that
pace whatever the server does: a server that stalls does not slow the client
down, requests pile up (at most 256 under way per target) and past that each
unsent request is a failure (with the plain targets at 30 requests per second
the cap is not reached: it matters above about 25 requests per second per
target). Without `--rps` every target has one request at
a time, 50 ms apart, as before. The pooled 8083 targets (stubs only) stay one
request at a time on their one connection. After the run the report holds:

- failures and cuts per 5 s window (counted when the request ended: a stalled
  request lands up to 10 s after it was sent), with the clock the `===` step lines of the
  script carry, so a burst can be laid against "rolling out", "aborting" or
  "rollout phase over";
- how long the rollout phase took;
- the lowest `MemAvailable` seen meanwhile against the value before it (the
  host's, so the stand's other load is in it: run on an idle stand). On a
  12 GB stand it says nothing about a 4 GB VPS beyond the sum it shows.

### The 8083 client (`mtls-client.sh`)

A real 8083 needs a client certificate, which the stubs did not. Before the
traffic starts `run.sh --real` runs `mtls-client.sh`, written from versola's
`scripts/est-smoke.sh` (steps 1-4; read its header): it registers a service
client (`tls_client_auth`, EST enrolment) in central through the admin API,
creates an enrolment token, enrols with a key and a CSR made here
(`/.well-known/est/simpleenroll` through the proxy), and asks `/token` once
over 8083 with the certificate. auth learns of a new client by a refresh of its
cache, every five minutes by default: until then `/token` answers 401
`invalid_client`, so it is polled every 5 s for up to `E2E_SYNC_WAIT_SECONDS`
(420; 275 s on the stand run below). The admin credential is the `CENTRAL_RESOURCE_SECRET` of the run's
own deployment, read from its `central.secrets.env` in the isolated `E2E_HOME`
the way `versola edge add` does; it goes to curl on stdin and is never printed.
The client is left in the throwaway deployment (`clean` removes its database).
If this step fails the run stops before any traffic; `--no-mtls` runs without
8083 and is then not a pass. Needs `curl`, `openssl` and `python3` on the
stand.

The client then sends, to 8083 with the certificate, `POST /token` with the
`client_credentials` grant: on a new connection each time and on 3 long-lived
ones (edge keeps a pool, and a request has to be in flight when the proxy
closes the connection for a cut to show). 200 is expected; cuts are reported
and reconnects counted as for the stubs (the known limitation above).

EST is off in auth unless the deployment was configured with `--est`
(versola-tools then writes auth's `est` block; without it `simpleenroll` and
`cacerts` answer an empty 404), and only a versola-tools of 0.8.0 or later knows it
(an older one ignores the flag, with the same 404): FROM of a `--real` run with
the 8083 client has to be that new, else use `--no-mtls`. `run.sh --real` passes
`--est` to `configure` unless `--no-mtls` is given, which is also what an
operator who wants EST on a vps does.

Seen on a stand (Ubuntu 24.04, 4 vCPU, 8 GB, two test tags of one commit of
`versola` main, `--replicas 1 --assume-compatible --rps 30`, 2026-11-04): the
`default` tenant accepts the client, enrolment answers 200, auth knows the
client after about 275 s and answers `/token` 200 on 8083; the rollout ran 398 s
with 35 226 requests, 0 failures, 0 cuts and 18 reconnects (what the known
8083 limitation predicts: a proxy reload ends edge-style pooled connections;
how many reloads there were, and how the reconnects split, was not counted),
and the memory available fell by up to 755 MiB. Not seen: 3 replicas, `--no-surge`, `--abort-at`, a real migration
between two commits.

Not covered yet, so a clean `--real` run is **not** a pass of #84: the
authorization code flow (it needs a user, and a stock vps install runs with
`ENV_NAME=prod`: no fixed OTP, no password-set endpoint; see #84), central's
admin console, the refusal of a release that needs a fresh database.

## Run (only on a throwaway machine)

```
./run.sh run /path/to/versola [--replicas 3] [--no-surge] [--base-tools 0.6.2]
./run.sh run /path/to/versola --abort-at prepared|central|standin|midway
./run.sh run /path/to/versola --strict      # cuts on 8083 fail the run
./run.sh run /path/to/versola --slow-central 40   # central B needs 40 s to become ready
./run.sh run /path/to/versola --manifest declared  # B's release.json lists A in rollingFrom: no --assume-compatible
./run.sh run /path/to/versola --manifest fresh     # B says it needs a fresh database: refused, A keeps serving
./run.sh run /path/to/versola --real 0.7.5 0.7.6 --rps 30   # with 30 requests per second; see "Load and numbers"
./run.sh run /path/to/versola --real 0.7.5 0.7.6 --no-mtls   # without the 8083 client (not a pass)
./run.sh run /path/to/versola --real 0.7.5 0.7.6   # real images and the bundled Postgres, see "Real images" (the versions are examples)
E2E_CONFIRM_CLEAN=yes ./run.sh clean
```

The rollout's preflight runs here too: the memory check for real (the stubs
use a few MiB, so it passes on any host that runs this), the Postgres
connection check only as a warning -- there is no Postgres.

`--abort-at` stops the rollout with SIGTERM ("stop after the step under
way") once its output shows that phase, runs `versola rollout --abort` under
the same traffic, and passes when the abort exits 0, the traffic client
reports no failures, and every central/auth/edge container is version A
again. `prepared` (stopped while pulling, before migrate) must restore the
previous configuration without restarting anything; the client then only
expects version A. `central` stops once central's stand-in (central-2) is
starting (with `--no-surge`, once central is restarted in place); `standin`
and `midway` stop at auth's and edge's steps, after central is done. With
`--no-surge` there is no stand-in, so `standin` stops after the first
replaced replica, like `midway`. After a `central` abort the auth and edge
containers must be the very ones that ran before the rollout (same IDs): an
abort recreates only what moved on. What stubs can't
show: the old version on a really migrated schema, edge registering its
client certificate with central, real start times -- those need the real
images (the `0.7.0` -> `0.7.0-rt1` run on the stand).

`--slow-central N` makes the stub central of version B answer `/readiness` with
503 for N seconds after it starts. The rollout must wait for its stand-in
(central-2) without routing to it, keep the old central serving 8090 meanwhile and
finish with no failures; with `--no-surge` central restarts in place and the
rollout prints how long it was unavailable (about N s plus the 1 s poll). The real central's start time is not measured here.

Linux only, needs more than 3.25 GiB of available memory and 4 GiB of disk (the
CLI's own prerequisite checks) and free ports 2821, 8082, 8083, 8090, 8200, 28090, 28091, 28190 and 28191. It
refuses to start if a container named `versola*` or the volume
`versola-openbao-file-vps` exists, or ports 2821/8082/8083 are taken. The
CLI's state goes to an isolated `HOME` (`E2E_HOME`, default
`/var/tmp/versola-e2e-home`, removed by `clean` only if `run` made it). Needs network for `ghcr.io` (the base tools image),
Docker Hub (`openbao`, `nginx`, `golang`) and an amd64 Docker.

The stub images and the wrapped tools exist only in the local image store, so
the rewritten compose file carries `pull_policy: never`.

Assumed, not yet seen on a real run: the generated compose file sets no
`command:`/`entrypoint:` on central/auth/edge (else a stub would start with a
java command line), and `docker compose pull` skips `pull_policy: never`
services (run.sh checks the latter before deploying).

Central is behind the proxy's front like auth's APORT/MPORT: nginx owns
127.0.0.1:8090 and the stub central listens on 28090/28091. run.sh checks
after `up` and after the run that 8090 answers with central of the expected
version and that 28090 answers too. Not run here (there is no Docker daemon
in the sandbox this was written in): the first run on a throwaway machine
must confirm it.
