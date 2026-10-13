#!/usr/bin/env bash
# Rollout e2e: deploys a stub version A of Versola on this machine with the
# real versola-cli, rolls it out to a stub version B while GET and POST traffic
# runs against the proxy (public port, 8082, 8083), and checks nothing failed.
#
#   ./run.sh run   /path/to/versola [--replicas 3] [--no-surge] [--base-tools 0.6.2]
#   ./run.sh clean
#
# Run it ONLY on a throwaway machine (the test stand): it starts containers
# named versola-*, a real OpenBao and the proxy on 127.0.0.1:2821/8082/8083,
# and refuses to start if anything named versola-* is already there. The CLI's
# own state lives in an isolated HOME (E2E_HOME), not in the real ~/.versola.
# Postgres is not needed: the stub tools make `migrate` a no-op and the stub
# services do not connect to anything. Nothing here prints a secret.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
E2E_HOME="${E2E_HOME:-/var/tmp/versola-e2e-home}"
MARKER=".versola-e2e"
A="${E2E_VERSION_A:-1.0.0-e2e}"
B="${E2E_VERSION_B:-1.0.1-e2e}"
AUTH_URL="https://e2e.test"
PG_HOST="127.0.0.1:5432"

die() { echo "e2e: $*" >&2; exit 1; }
say() { printf '\n=== %s\n' "$*"; }

# A docker that cannot be asked is not "no containers".
existing() {
  local all
  all="$(docker ps -a --format '{{.Names}}')" || die "cannot ask docker (is the daemon up, and are you allowed to use it?)"
  printf '%s\n' "$all" | grep -E '^versola' || true
}

clean() {
  say "cleaning up the e2e deployment"
  docker info >/dev/null 2>&1 || die "cannot reach the docker daemon"
  local names; names="$(existing)"
  if [ -n "$names" ]; then
    # Any container named versola* goes, a real deployment's included: say
    # which, and make the operator say yes in the environment.
    echo "this removes the containers:"; echo "$names"
    [ "${E2E_CONFIRM_CLEAN:-}" = "yes" ] || die "set E2E_CONFIRM_CLEAN=yes to remove them (only on the throwaway stand)"
    # shellcheck disable=SC2086
    docker rm -f $names >/dev/null
  fi
  docker volume rm versola-openbao-file-vps >/dev/null 2>&1 || true
  for v in "$A" "$B"; do
    for s in central auth edge; do docker rmi "versola-e2e/$s:$v" >/dev/null 2>&1 || true; done
    docker rmi "ghcr.io/versolauth/versola-tools:$v" >/dev/null 2>&1 || true
  done
  # Only a directory this script made: E2E_HOME comes from the environment.
  if [ -e "$E2E_HOME" ]; then
    [ -f "$E2E_HOME/$MARKER" ] || die "$E2E_HOME was not made by this script (no $MARKER): not removing it"
    rm -rf "$E2E_HOME"
  fi
  echo "done"
}

build() {
  local base="$1"
  say "building stub images ($A, $B) and the wrapped versola-tools (base $base)"
  for v in "$A" "$B"; do
    for s in central auth edge; do
      docker build -q --platform linux/amd64 --build-arg ROLE="$s" --build-arg VERSION="$v" \
        -t "versola-e2e/$s:$v" "$HERE/stub" >/dev/null
    done
    docker build -q --platform linux/amd64 --build-arg BASE="ghcr.io/versolauth/versola-tools:$base" \
      --build-arg VERSION="$v" -t "ghcr.io/versolauth/versola-tools:$v" "$HERE/tools" >/dev/null
  done
  # `versola rollout` pulls the new version's images first; the stubs are only
  # in the local store, so compose must skip them (pull_policy: never). Check
  # that this docker compose does, before anything is deployed.
  local t; t="$(mktemp -d)"
  printf 'services:\n  s:\n    image: versola-e2e/auth:%s\n    pull_policy: never\n' "$A" >"$t/c.yml"
  docker compose -f "$t/c.yml" pull >/dev/null 2>&1 \
    || { rm -rf "$t"; die "this docker compose ($(docker compose version --short 2>/dev/null)) does not skip pull_policy: never on 'compose pull': the rollout's pull step would fail. Use a newer Compose, or push the images to a local registry"; }
  rm -rf "$t"
}

run() {
  local cli="$1"; shift
  local replicas=3 surge="" base="0.6.2"
  while [ $# -gt 0 ]; do
    case "$1" in
      --replicas) replicas="$2"; shift 2 ;;
      --no-surge) surge="--no-surge"; shift ;;
      --base-tools) base="$2"; shift 2 ;;
      *) die "unknown argument $1" ;;
    esac
  done
  [ -x "$cli" ] || die "$cli is not an executable versola binary"
  command -v docker >/dev/null || die "no docker"

  local found; found="$(existing)"
  [ -z "$found" ] || die "containers named versola* already exist here, refusing to touch them:
$found"
  docker volume inspect versola-openbao-file-vps >/dev/null 2>&1 && die "volume versola-openbao-file-vps exists, refusing to use it"
  [ ! -e "$E2E_HOME/.versola" ] || die "$E2E_HOME/.versola is left from an earlier run: run.sh clean first"
  [ "$(uname -s)" = "Linux" ] || die "Linux only (network_mode: host, /proc/meminfo)"
  # The CLI's own prerequisite checks (internal/checks): at least 3.25 GiB of
  # available memory for a first configure, 4 GiB of free disk.
  local kb; kb="$(awk '/MemAvailable/ {print $2}' /proc/meminfo)"
  [ "${kb:-0}" -ge $((3584 * 1024)) ] || die "only $((${kb:-0} / 1024)) MiB of memory available; the CLI wants more than 3.25 GiB free for a first configure"
  [ "$(df -Pk "$PWD" | awk 'NR==2 {print $4}')" -ge $((4 * 1024 * 1024)) ] || die "less than 4 GiB of free disk in $PWD"
  for p in 2821 8082 8083 8200; do
    if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then die "port $p is already in use"; fi
  done

  build "$base"

  # The CLI keeps its state under $HOME; the docker CLI's own config stays where it is.
  export DOCKER_CONFIG="${DOCKER_CONFIG:-$HOME/.docker}"
  mkdir -p "$E2E_HOME"; touch "$E2E_HOME/$MARKER"
  export HOME="$E2E_HOME"
  local work="$E2E_HOME/run"; mkdir -p "$work"

  say "deploying stub $A with $replicas auth and $replicas edge replicas"
  printf 'y\n' | "$cli" configure vps "$A" --auth-url "$AUTH_URL" --postgres-host "$PG_HOST" --proxy external
  printf 'y\n' | "$cli" migrate
  printf 'y\n' | "$cli" up --no-browser
  if [ "$replicas" -gt 1 ]; then
    printf 'y\n' | "$cli" replica add auth $((replicas - 1))
    printf 'y\n' | "$cli" replica add edge $((replicas - 1))
  fi
  "$cli" status

  say "building the traffic client"
  (cd "$HERE/traffic" && docker run --rm -v "$HERE/traffic:/src" -v "$work:/out" -w /src \
      -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 golang:1.25-alpine go build -o /out/traffic .)

  stop="$work/stop"; rm -f "$stop"
  "$work/traffic" -h >/dev/null 2>&1 || die "cannot execute $work/traffic (is $E2E_HOME on a noexec mount?)"
  "$work/traffic" -host e2e.test -stop-file "$stop" -expect-versions "$A,$B" >"$work/traffic.txt" 2>&1 &
  tpid=$!
  # A killed or failed script must not leave the client hammering the proxy.
  trap 'touch "$stop"; kill "$tpid" 2>/dev/null || true' EXIT
  sleep 8
  say "rolling out $B ${surge}"
  local rrc=0
  "$cli" rollout "$B" --yes $surge || rrc=$?
  sleep 10
  touch "$stop"
  local trc=0
  wait "$tpid" || trc=$?

  say "traffic report"
  cat "$work/traffic.txt"

  say "what runs now"
  docker ps --format '{{.Names}}\t{{.Image}}' | grep -E '^versola' | sort || true
  "$cli" status || true

  echo
  echo "rollout exit: $rrc   traffic exit: $trc   (0 and 0 is a pass)"
  [ "$rrc" -eq 0 ] && [ "$trc" -eq 0 ]
}

case "${1:-}" in
  run) shift; [ $# -ge 1 ] || die "usage: run.sh run /path/to/versola [--replicas N] [--no-surge] [--base-tools V]"; run "$@" ;;
  clean) clean ;;
  *) die "usage: run.sh run /path/to/versola [...] | run.sh clean" ;;
esac
