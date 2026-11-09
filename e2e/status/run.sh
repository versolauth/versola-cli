#!/usr/bin/env bash
# Real Docker/Compose and the built CLI, with an isolated temporary home.
set -euo pipefail
cd "$(dirname "$0")/../.."
command -v jq >/dev/null
docker info >/dev/null
temp=$(mktemp -d "${TMPDIR:-/tmp}/versola-status-e2e.XXXXXX")
cli="$temp/versola"
if [[ $(go env GOOS) == windows ]]; then cli+=.exe; fi
go build -o "$cli" ./cmd/versola
# Keep Docker's selected context and user-installed Compose plugin when the
# CLI's state moves to a disposable HOME, as the rollout harness does.
export DOCKER_CONFIG="${DOCKER_CONFIG:-$HOME/.docker}"
if command -v cygpath >/dev/null; then DOCKER_CONFIG=$(cygpath -w "$DOCKER_CONFIG"); fi
export HOME="$temp" CI=true
if command -v cygpath >/dev/null; then
  export USERPROFILE="$(cygpath -w "$temp")"
else
  export USERPROFILE="$temp"
fi
bundle="$temp/.versola/active/bundle-e2e"
mkdir -p "$bundle"
project="versola-status-e2e-$(date +%s)-$$"
# An inherited COMPOSE_PROJECT_NAME overrides name: in the fixture. Pin it
# for the CLI too, so neither the test nor its cleanup can reach that project.
export COMPOSE_PROJECT_NAME="$project"
compose="$bundle/compose.yml"
cat > "$temp/.versola/active/state.json" <<'JSON'
{"schemaVersion":4,"target":"local","version":"0.7.5","bundleDir":"bundle-e2e","proxyMode":"local","certLifetimeDays":0,"slots":{"auth":[{"n":1,"version":"0.7.5"},{"n":2,"version":"0.7.5"}]}}
JSON
cat > "$compose" <<YAML
name: $project
services:
  auth:
    image: busybox:1.37.0
    command: [sh, -c, 'touch /tmp/ready; exec sleep 86400']
    healthcheck:
      test: [CMD, test, -f, /tmp/ready]
      interval: 1s
      timeout: 1s
      retries: 1
  auth-2:
    image: busybox:1.37.0
    command: [sleep, '86400']
  central:
    image: busybox:1.37.0
    command: [sh, -c, 'if test -f /tmp/restart; then exit 1; fi; exec sleep 86400']
    restart: always
  mtls-init:
    image: busybox:1.37.0
    command: [sh, -c, 'exit 0']
YAML
compose_arg="$compose"
if command -v cygpath >/dev/null; then compose_arg=$(cygpath -w "$compose"); fi
dc() { MSYS_NO_PATHCONV=1 docker compose -p "$project" -f "$compose_arg" "$@"; }
cleanup() {
  dc down --remove-orphans >/dev/null 2>&1 || true
  echo "Status e2e artifacts: $temp"
}
trap cleanup EXIT
json="$temp/status.json"
text="$temp/status.txt"
snapshot() {
  # Under set -e either CLI exiting nonzero fails this test. The output must
  # remain one JSON object, including when services are down.
  "$cli" status --json > "$json"
  jq -e -s 'length == 1 and .[0].schema == 1' "$json" >/dev/null
  jq -e --arg project "$project" '.containers | all(.name | startswith($project + "-"))' "$json" >/dev/null
  "$cli" status > "$text"
  jq -r '.warnings[] | select(startswith("service "))' "$json" | tr -d '\r' | while IFS= read -r warning; do
    grep -F -- "WARNING: $warning" "$text" >/dev/null
  done
  if jq -r '.warnings[]' "$json" | grep -q mtls-init; then
    echo 'Successful init reported as an outage' >&2; return 1
  fi
}
wait_health() {
  local id health
  id=$(dc ps -a -q auth | tr -d '\r')
  for ((i=0; i<60; i++)); do
    health=$(docker inspect --format '{{.State.Health.Status}}' "$id" | tr -d '\r')
    [[ "$health" == "$1" ]] && return 0
    sleep 1
  done
  echo "auth did not become $1" >&2; return 1
}
dc up -d
wait_health healthy
snapshot
jq -e '.warnings | length == 0' "$json" >/dev/null
# The next process exits immediately on every restart. Wait for Docker's
# restart backoff, then assert both formats during that stable window.
dc exec -T central touch /tmp/restart
dc restart central
restarting=false
for ((i=0; i<60; i++)); do
  id=$(dc ps -a -q central | tr -d '\r')
  count=$(docker inspect --format '{{.RestartCount}}' "$id" | tr -d '\r')
  if ((count >= 7)); then restarting=true; break; fi
  sleep 1
done
[[ "$restarting" == true ]] || { echo 'central did not enter restart backoff' >&2; exit 1; }
snapshot
jq -e '.warnings | any(contains("service central (") and contains("is restarting"))' "$json" >/dev/null
dc exec -T auth test -f /tmp/ready
dc stop central
snapshot
jq -e '.warnings | any(contains("service central") and contains("state: exited") and endswith("versola logs central --tail 100`"))' "$json" >/dev/null
dc rm -f central
snapshot
jq -e '.warnings | any(contains("service central has no container"))' "$json" >/dev/null
dc exec -T auth rm /tmp/ready
wait_health unhealthy
snapshot
jq -e '.warnings | any(contains("service auth (") and contains("is unhealthy"))' "$json" >/dev/null
dc stop auth-2
snapshot
jq -e '.warnings | any(contains("service auth-2 (") and contains("versola logs auth-2 --tail 100"))' "$json" >/dev/null
dc down
snapshot
jq -e '[.warnings[] | select(contains("has no container"))] | length == 3' "$json" >/dev/null
dc up -d
wait_health healthy
snapshot
jq -e '.warnings | length == 0' "$json" >/dev/null
echo 'PASS status text/JSON: healthy, restarting, exited, missing, unhealthy, replica, down, recovery, successful init; exit 0 throughout'
