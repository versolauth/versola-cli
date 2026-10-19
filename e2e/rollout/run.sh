#!/usr/bin/env bash
# Rollout e2e: deploys a stub version A of Versola on this machine with the
# real versola-cli, rolls it out to a stub version B while GET and POST traffic
# runs against the proxy (public port, 8082, 8083), and checks nothing failed.
#
#   ./run.sh run   /path/to/versola [--replicas 3] [--no-surge] [--base-tools 0.6.2]
#                  [--abort-at prepared|central|standin|midway] [--strict]
#                  [--slow-central SECONDS]
#   ./run.sh clean
#
# --abort-at stops the rollout (SIGTERM: "stop after the step under way") once
# it reaches that phase, then runs `versola rollout --abort` under the same
# traffic, and checks every container is version A again:
#   prepared  after the new bundle is prepared, before migrate: the abort only
#             makes the previous configuration active again (nothing restarts)
#   central   after central restarted on B, before any stand-in
#   standin   after the auth stand-in (B) takes traffic
#   midway    after the first replica was recreated on B
#
# --slow-central makes central of version B answer /readiness with 503 for that
# many seconds after it starts (default 2), to see the rollout wait for it and
# report how long central was unavailable; auth and edge must keep serving.
#
# Cuts of the long-lived 8083 connections (nginx closes a retired worker's
# connections 30 s after each reload) are the known limitation: reported, not
# a failure. --strict counts them as failures (see README.md).
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
# Central sits behind the proxy's front: 8090 is nginx's and balances over the
# central replicas, which listen on 28090/28091. Answers on both mean the front
# is up with central behind it; the front must answer with central's version.
check_central() {
  local want="$1" out
  out="$(curl -fsSi --max-time 5 http://127.0.0.1:8090/ 2>&1)" || { echo "e2e: central's front (127.0.0.1:8090) does not answer: $out" >&2; return 1; }
  grep -q "central" <<<"$out" || { echo "e2e: 127.0.0.1:8090 is not answered by central: $out" >&2; return 1; }
  [ -z "$want" ] || grep -qF "$want" <<<"$out" || { echo "e2e: central behind 8090 is not $want: $out" >&2; return 1; }
  curl -fsS --max-time 5 http://127.0.0.1:28090/ >/dev/null 2>&1 || { echo "e2e: central itself does not answer on its own port 28090" >&2; return 1; }
}
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
  local base="$1" slow_central="${2:-}"
  say "building stub images ($A, $B) and the wrapped versola-tools (base $base)"
  for v in "$A" "$B"; do
    for s in central auth edge; do
      local delay=()
      [ "$s" = central ] && [ "$v" = "$B" ] && [ -n "$slow_central" ] && delay=(--build-arg READY_DELAY="$slow_central")
      docker build -q --platform linux/amd64 --build-arg ROLE="$s" --build-arg VERSION="$v" \
        ${delay[@]+"${delay[@]}"} -t "versola-e2e/$s:$v" "$HERE/stub" >/dev/null
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
  local replicas=3 surge="" base="0.6.2" abort_at="" strict="" slow_central=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --abort-at) abort_at="$2"; shift 2 ;;
      --replicas) replicas="$2"; shift 2 ;;
      --no-surge) surge="--no-surge"; shift ;;
      --base-tools) base="$2"; shift 2 ;;
      --strict) strict=yes; shift ;;
      --slow-central) slow_central="$2"; shift 2 ;;
      *) die "unknown argument $1" ;;
    esac
  done
  [ -x "$cli" ] || die "$cli is not an executable versola binary"
  local pattern="" versions="$A,$B"
  case "$abort_at" in
    "") ;;
    prepared) pattern="Pulling the images"; versions="$A" ;;
    central) pattern="Waiting for central to be ready" ;;
    standin) pattern="takes traffic" ;;
    midway) pattern="runs $B and takes traffic" ;;
    *) die "--abort-at: prepared, central, standin or midway" ;;
  esac
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
  for p in 2821 8082 8083 8090 8200 28090 28091; do
    if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then die "port $p is already in use"; fi
  done

  [ -z "$slow_central" ] || [[ "$slow_central" =~ ^[0-9]+$ ]] || die "--slow-central: a number of seconds"
  build "$base" "$slow_central"

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
  say "central behind the proxy's front"
  check_central "$A" || die "central is not behind the front after up"

  say "building the traffic client"
  (cd "$HERE/traffic" && docker run --rm -v "$HERE/traffic:/src" -v "$work:/out" -w /src \
      -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 golang:1.25-alpine go build -o /out/traffic .)

  stop="$work/stop"; rm -f "$stop"
  "$work/traffic" -h >/dev/null 2>&1 || die "cannot execute $work/traffic (is $E2E_HOME on a noexec mount?)"
  "$work/traffic" -host e2e.test -stop-file "$stop" -expect-versions "$versions" ${strict:+-strict} >"$work/traffic.txt" 2>&1 &
  tpid=$!
  rpid=""
  # A killed or failed script must not leave the client hammering the proxy,
  # nor a rollout running in the background.
  trap 'touch "$stop"; kill "$tpid" ${rpid:-} 2>/dev/null || true' EXIT
  sleep 8
  local rrc=0 arc=0
  if [ -z "$abort_at" ]; then
    say "rolling out $B ${surge}"
    "$cli" rollout "$B" --yes --assume-compatible $surge || rrc=$?
  else
    say "rolling out $B ${surge}, stopping it at '$abort_at'"
    local log="$work/rollout.txt"
    "$cli" rollout "$B" --yes --assume-compatible $surge >"$log" 2>&1 &
    rpid=$!
    local sent=""
    while kill -0 "$rpid" 2>/dev/null; do
      if [ -z "$sent" ] && grep -qF "$pattern" "$log"; then
        kill -TERM "$rpid" 2>/dev/null || true; sent=yes
      fi
      sleep 0.5
    done
    wait "$rpid" || rrc=$?
    rpid=""
    cat "$log"
    [ -n "$sent" ] || die "the rollout never reached '$abort_at' (no \"$pattern\" in its output)"
    [ "$rrc" -ne 0 ] || die "the rollout finished before it could be stopped at '$abort_at'"
    rrc=0 # stopped on purpose
    "$cli" status || true
    sleep 5
    say "aborting ${surge}"
    "$cli" rollout --abort --yes $surge | tee "$work/abort.txt" || arc=$?
    if [ "$abort_at" = prepared ] && ! grep -q "nothing running was touched" "$work/abort.txt"; then
      echo "e2e: an abort before migrate must only restore the previous configuration" >&2; arc=1
    fi
    local stray
    stray="$(docker ps --format '{{.Names}} {{.Image}}' | grep -E '^versola-(central|auth|edge)' | grep -v ":$A\$" || true)"
    if [ -n "$stray" ]; then echo "e2e: still not on $A after the abort:" >&2; echo "$stray" >&2; arc=1; fi
    local st_out; st_out="$("$cli" status 2>&1 || true)"
    if grep -q "under way" <<<"$st_out"; then echo "e2e: status still reports a rollout under way" >&2; arc=1; fi
  fi
  sleep 10
  touch "$stop"
  local trc=0
  wait "$tpid" || trc=$?

  local crc=0
  check_central "$A" || crc=1
  [ "$abort_at" != "" ] || check_central "$B" || crc=1

  say "traffic report"
  cat "$work/traffic.txt"

  say "what runs now"
  docker ps --format '{{.Names}}\t{{.Image}}' | grep -E '^versola' | sort || true
  "$cli" status || true

  echo
  echo "rollout exit: $rrc   abort exit: $arc   traffic exit: $trc   central front: $crc   (all 0 is a pass)"
  [ "$rrc" -eq 0 ] && [ "$arc" -eq 0 ] && [ "$trc" -eq 0 ] && [ "$crc" -eq 0 ]
}

case "${1:-}" in
  run) shift; [ $# -ge 1 ] || die "usage: run.sh run /path/to/versola [--replicas N] [--no-surge] [--base-tools V] [--abort-at PHASE] [--strict] [--slow-central SECONDS]"; run "$@" ;;
  clean) clean ;;
  *) die "usage: run.sh run /path/to/versola [...] | run.sh clean" ;;
esac
