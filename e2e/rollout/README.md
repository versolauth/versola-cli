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
          8082 and 8083 (new and long-lived connections); counts every
          transport error and unexpected status
run.sh    builds the images, deploys version A, rolls out version B under
          traffic, prints the report
```

**Pass** = `versola rollout` exits 0, the traffic client reports zero
failures, and every target was served by both versions (`-expect-versions`).

Connections to 8083 are the known risk (nginx cuts stream connections of a
retired worker after `worker_shutdown_timeout`, 30 s). The client therefore
keeps pooled connections there and counts every re-dial after the first as a
failure (net/http would otherwise reconnect quietly), and keeps one request
(`/slow?ms=1500`) always in flight so a cut is an error. Plain POSTs send
`Connection: close`: a keep-alive connection that a reload closes at the very
moment a POST is written fails whatever the CLI did.

## Run (only on a throwaway machine)

```
./run.sh run /path/to/versola [--replicas 3] [--no-surge] [--base-tools 0.6.2]
E2E_CONFIRM_CLEAN=yes ./run.sh clean
```

Linux only, needs more than 3.25 GiB of available memory and 4 GiB of disk (the
CLI's own prerequisite checks) and free ports 2821, 8082, 8083 and 8200. It
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
