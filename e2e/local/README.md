# Local e2e

Checks the first thing a new customer does, `versola bootstrap local
<version>`, end to end with the real CLI built from this checkout and the
real Versola images (`ghcr.io/versolauth/*`, plus OpenBao, Postgres,
step-ca and nginx from Docker Hub). `run.sh` runs, in order:

```
versola doctor
versola bootstrap local <version> --no-browser
probes        http://127.0.0.1:2821/.well-known/openid-configuration -> 200 (with an issuer)
              http://127.0.0.1:2821/permissions/me                   -> 401
              http://127.0.0.1:2821/central/admin/                   -> 200
versola status
versola down
versola up --no-browser
probes        the same three again
versola uninstall -y
nothing left  no container and no volume named versola*, no
              ghcr.io/versolauth/versola-* image, no ~/.versola/active
```

The probes are the three answers the CLI's proxy itself waits for before
it says "running" (`internal/proxy/wait.go`): auth, edge and central's admin
console, each through the proxy. After `uninstall` the third-party images
(openbao, postgres, nginx, step-ca) and `~/.versola/openbao/local*` stay by
design; the latter is in the isolated `HOME`, which `run.sh` removes after a
passing run.

**Pass** = every command exits 0, every probe gets its status, and nothing
named versola* is left.

## In CI

`.github/workflows/e2e-local.yml` runs it on `ubuntu-latest`:

- nightly, with the latest Versola release (`releases/latest` of
  `versolauth/versola`);
- by hand (Actions -> e2e-local -> Run workflow), with an optional
  `version`;
- on a pull request labelled `e2e` (when the label is added, and on every
  push while it is there).

Scheduled and manual runs only exist on the default branch: GitHub reads
the `schedule` and `workflow_dispatch` triggers from `main`, so a change to
this workflow on a branch is tried with the `e2e` label instead. It is not a
required check and not part of `ci.yml`.

On a failure the run uploads the artifact `e2e-local-diagnostics` (kept 7
days): `docker ps -a` of the versola containers, the last 500 log lines of
each except step-ca's, and `versola status`. Nothing from `~/.versola` or
the bundle directory is ever collected: those files hold the secrets.
step-ca's log is left out too (a note file says so): on its first start
step-ca prints its CA administrative password there, and a filter that
misses one line would upload it.

## Run by hand (only on a throwaway Linux machine)

```
e2e/local/run.sh 0.7.5
E2E_VERSION=0.7.5 e2e/local/run.sh
E2E_KEEP=1 e2e/local/run.sh 0.7.5     # leave a failed deployment running to look at
```

Needs Docker with the Compose plugin, at least 4 GiB of memory for Docker
and free disk (the CLI's own `doctor` checks), port 2821 free, Go to build
the CLI, curl, and network for `ghcr.io` and Docker Hub.

It refuses to start if a container or a volume named `versola*` exists. The
CLI's state goes to an isolated `HOME` (`E2E_HOME`, default
`/var/tmp/versola-e2e-local-home`; a directory this script did not make is
refused). `versola uninstall -y` removes every `ghcr.io/versolauth/versola-*`
image on the host, not only this run's.

Diagnostics go to `E2E_ARTIFACTS` (default `./e2e-local-artifacts`). After
a failure the script removes the deployment with `versola uninstall -y`
unless `E2E_KEEP=1`; if it stops before that, `HOME=$E2E_HOME versola
uninstall -y` with the binary in `$E2E_HOME/bin` cleans up.

## Not checked

- **Logging in.** The admin login is a multi-step flow (PKCE authorization
  request, session cookie, CSRF token, password, OTP); a curl version would
  break on every change of that flow's shape. A login smoke test is a
  follow-up.
- **The vps target**, and anything on a remote host: never run from here.
- `rollout`, `replica`, `backup`, `certs`: see `e2e/rollout/` for rollout
  with stub images.

Not yet seen on a real run: this was written without a Docker daemon; the
first CI run (scheduled, manual or labelled) must confirm it.
