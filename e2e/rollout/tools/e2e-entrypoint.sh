#!/bin/sh
# Wraps the real versola-tools entrypoint for the rollout e2e.
#
#   - "migrate" (compose's `migrate` service runs this same image) succeeds
#     without a database: the e2e is about moving traffic, and the stand's
#     Postgres is not what is under test.
#   - anything else runs the real entrypoint, so the configs, secret files and
#     the secrets schema are the real ones, and then the services' images in
#     the generated compose file are replaced with the stubs of this version.
#
# E2E_VERSION is the version this image stands for (set at build time).
set -eu

for a in "$@"; do
  if [ "$a" = "migrate" ]; then
    echo "e2e stub: migrate ($E2E_VERSION) -- nothing to apply"
    exit 0
  fi
done

./entrypoint.sh "$@"

# E2E_ROLLING_FROM: this version declares itself rollable from that one, in
# the compatibility manifest (release.json) the real tools image writes to /out.
# The base image's own release.json, if it has one, is replaced: the stub
# declares exactly what the test says, so a rollout needs no --assume-compatible.
if [ -n "${E2E_ROLLING_FROM:-}" ] && [ -d /out ]; then
  printf '{
  "schemaVersion": 1,
  "version": "%s",
  "rollingFrom": ["%s"],
  "freshDatabaseFrom": [],
  "notes": "https://example.invalid/e2e"
}
'     "$E2E_VERSION" "$E2E_ROLLING_FROM" >/out/release.json
fi

f=/out/compose.fragment.yml
[ -f "$f" ] || exit 0 # e.g. `secrets plan`: nothing to rewrite

# `pull_policy: never`: the stubs exist only on this machine, and the
# rollout's `compose pull` must not go looking for them in a registry.
for svc in central auth edge; do
  sed -i "s#image: ghcr.io/versolauth/versola-$svc:[^ ]*#image: versola-e2e/$svc:$E2E_VERSION\n    pull_policy: never#" "$f"
done
sed -i "s#image: ghcr.io/versolauth/versola-tools:[^ ]*#image: ghcr.io/versolauth/versola-tools:$E2E_VERSION\n    pull_policy: never#" "$f"

for svc in central auth edge; do
  grep -q "image: versola-e2e/$svc:$E2E_VERSION" "$f" || { echo "e2e stub: no image of $svc in $f to replace" >&2; exit 1; }
done
grep -q "image: ghcr.io/versolauth/versola-tools:$E2E_VERSION" "$f" || { echo "e2e stub: no migrate image in $f to replace" >&2; exit 1; }
echo "e2e stub: services of $f are now versola-e2e/*:$E2E_VERSION"
