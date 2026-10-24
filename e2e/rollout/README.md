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

## Run (only on a throwaway machine)

```
./run.sh run /path/to/versola [--replicas 3] [--no-surge] [--base-tools 0.6.2]
./run.sh run /path/to/versola --abort-at prepared|central|standin|midway
./run.sh run /path/to/versola --strict      # cuts on 8083 fail the run
./run.sh run /path/to/versola --slow-central 40   # central B needs 40 s to become ready
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
