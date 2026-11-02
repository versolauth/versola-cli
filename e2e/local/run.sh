#!/usr/bin/env bash
# Local e2e: what a new customer does first, with real Versola images.
# Builds the CLI from this checkout, then on this machine's Docker:
#
#   versola doctor
#   versola bootstrap local <version> --no-browser
#   three probes through the proxy on http://127.0.0.1:2821
#   versola status
#   versola down
#   versola up --no-browser          (a restart must work too)
#   the same probes again
#   versola uninstall -y
#   nothing named versola* is left: no container, no volume, no
#   ghcr.io/versolauth/versola-* image, no ~/.versola/active
#
#   ./run.sh <version>                  or   E2E_VERSION=<version> ./run.sh
#
# Run it ONLY on a throwaway Linux machine with Docker (a CI runner, a test
# VM): it starts containers named versola-*, OpenBao and the proxy on
# 127.0.0.1:2821, and `versola uninstall -y` removes every
# ghcr.io/versolauth/versola-* image on the host, not only this run's. It
# refuses to start if any container or volume named versola* already
# exists. The CLI's state lives in an isolated HOME (E2E_HOME), never in the
# real ~/.versola.
#
# On a failure it writes, to E2E_ARTIFACTS (default ./e2e-local-artifacts),
# `docker ps -a` of the versola containers, the last 500 log lines of each
# except step-ca's, and `versola status` -- never a file from ~/.versola or
# the bundle (they hold the secrets), and never step-ca's log: on its first
# start step-ca prints its CA administrative password there -- then removes
# the deployment again with `versola
# uninstall -y` (E2E_KEEP=1 leaves it running to look at by hand).
#
# Not checked: logging in (see README.md), the vps target.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
E2E_HOME="${E2E_HOME:-/var/tmp/versola-e2e-local-home}"
E2E_ARTIFACTS="${E2E_ARTIFACTS:-$PWD/e2e-local-artifacts}"
MARKER=".versola-e2e-local"
BASE="http://127.0.0.1:2821"
V="${1:-${E2E_VERSION:-}}"

die() { echo "e2e: $*" >&2; exit 1; }
say() { printf '\n=== %s\n' "$*"; }

[ -n "$V" ] || die "usage: $0 <versola version>   (or E2E_VERSION=<version>)"
# The version goes into image tags and paths: no leading v, nothing odd.
[[ "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$ ]] || die "version $V does not look like a Versola release (e.g. 0.7.5, no leading v)"
command -v docker >/dev/null || die "no docker"
command -v curl >/dev/null || die "no curl"
command -v go >/dev/null || die "no go (the CLI is built from this checkout)"

# A docker that cannot be asked is not "nothing there".
containers() {
  local all
  all="$(docker ps -a --format '{{.Names}}')" || die "cannot ask docker for its containers (is the daemon up, and are you allowed to use it?)"
  printf '%s\n' "$all" | grep -E '^versola' || true
}
volumes() {
  local all
  all="$(docker volume ls --format '{{.Name}}')" || die "cannot ask docker for its volumes"
  printf '%s\n' "$all" | grep -E '^versola' || true
}
images() {
  docker images --format '{{.Repository}}:{{.Tag}}' --filter 'reference=ghcr.io/versolauth/versola-*' || die "cannot ask docker for its images"
}

found="$(containers)"
[ -z "$found" ] || die "containers named versola* already exist here, refusing to touch them:
$found"
found="$(volumes)"
[ -z "$found" ] || die "volumes named versola* already exist here, refusing to touch them:
$found"
if [ -e "$E2E_HOME" ]; then
  [ -f "$E2E_HOME/$MARKER" ] || die "$E2E_HOME exists and was not made by this script (no $MARKER): choose another E2E_HOME"
  [ ! -e "$E2E_HOME/.versola/active" ] || die "$E2E_HOME/.versola/active is left from an earlier run: run \`HOME=$E2E_HOME versola uninstall -y\`, or remove $E2E_HOME"
fi

say "building the CLI from $REPO"
mkdir -p "$E2E_HOME/bin"
touch "$E2E_HOME/$MARKER"
VERSOLA="$E2E_HOME/bin/versola"
# Built before HOME moves: the go build cache and module cache stay where
# they are.
(cd "$REPO" && CGO_ENABLED=0 go build -o "$VERSOLA" ./cmd/versola)

# The CLI keeps its state under $HOME; the docker CLI's own config (and any
# registry login) stays where it is.
export DOCKER_CONFIG="${DOCKER_CONFIG:-$HOME/.docker}"
export HOME="$E2E_HOME"

collect() {
  mkdir -p "$E2E_ARTIFACTS"
  echo "e2e: collecting diagnostics into $E2E_ARTIFACTS (container list, container logs except step-ca's, versola status; no state or bundle files)" >&2
  docker ps -a --filter name=versola >"$E2E_ARTIFACTS/docker-ps.txt" 2>&1 || true
  local c
  for c in $(docker ps -a --filter name=versola --format '{{.Names}}' 2>/dev/null); do
    # step-ca's first start prints "Your CA administrative password is: ..."
    # to its log (versola-tools' compose initialises it from
    # DOCKER_STEPCA_INIT_* without a password file). Not filtered, left out:
    # a filter that misses one line uploads a secret.
    case "$c" in
      *step-ca*)
        echo "logs of $c are not collected: step-ca prints its CA administrative password to its log on the first start" >"$E2E_ARTIFACTS/logs-$c.NOT-COLLECTED.txt"
        continue ;;
    esac
    docker logs --tail 500 "$c" >"$E2E_ARTIFACTS/logs-$c.txt" 2>&1 || true
  done
  "$VERSOLA" status >"$E2E_ARTIFACTS/versola-status.txt" 2>&1 || true
}

on_exit() {
  local rc=$?
  [ "$rc" -eq 0 ] && return
  collect
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "e2e: FAILED (exit $rc); E2E_KEEP=1, the deployment is left as it is (HOME=$E2E_HOME)" >&2
  else
    echo "e2e: FAILED (exit $rc); removing the deployment with versola uninstall -y" >&2
    "$VERSOLA" uninstall -y >/dev/null 2>&1 || echo "e2e: versola uninstall -y failed too: clean up by hand (HOME=$E2E_HOME)" >&2
  fi
  exit "$rc"
}
trap on_exit EXIT

# expect PATH CODE: the proxy answers PATH with CODE. Retried for a minute:
# `up` waits for the same three answers before it says "running", so a
# retry only covers a slow restart, not a broken one.
expect() {
  local path="$1" want="$2" got="" i
  for i in $(seq 1 30); do
    got="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$BASE$path" || true)"
    [ "$got" = "$want" ] && { echo "ok: $path -> $got"; return 0; }
    sleep 2
  done
  die "$BASE$path answered $got, expected $want (after 60 s)"
}
probes() {
  # The same three the CLI's proxy waits for (internal/proxy/wait.go): auth
  # (discovery), edge (401 without a token), central's admin console.
  expect /.well-known/openid-configuration 200
  local doc
  doc="$(curl -fsS --max-time 5 "$BASE/.well-known/openid-configuration")" || die "the discovery document could not be read"
  grep -q '"issuer"' <<<"$doc" || die "the discovery document has no issuer"
  expect /permissions/me 401
  expect /central/admin/ 200
}

say "versola doctor"
"$VERSOLA" doctor

say "versola bootstrap local $V"
"$VERSOLA" bootstrap local "$V" --no-browser </dev/null
probes

say "versola status"
"$VERSOLA" status

say "versola down"
"$VERSOLA" down </dev/null

say "versola up"
"$VERSOLA" up --no-browser </dev/null
probes

say "versola uninstall -y"
"$VERSOLA" uninstall -y </dev/null

say "checking nothing is left"
left="$(containers)"
[ -z "$left" ] || die "containers left after uninstall:
$left"
left="$(volumes)"
[ -z "$left" ] || die "volumes left after uninstall:
$left"
left="$(images)"
[ -z "$left" ] || die "versola images left after uninstall:
$left"
[ ! -e "$HOME/.versola/active" ] || die "$HOME/.versola/active is left after uninstall"
# Expected to stay, by design: the third-party images (openbao, postgres,
# nginx, step-ca) and ~/.versola/openbao/local* (local OpenBao's
# credentials; their volume is gone, so they are dead).

rm -rf "$E2E_HOME"
say "PASS: bootstrap, restart and uninstall of Versola $V"
