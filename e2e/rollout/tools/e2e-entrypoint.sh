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
