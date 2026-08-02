#!/bin/sh
# Installs versola-cli for macOS/Linux, no admin/sudo required.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/versolauth/versola-cli/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/versolauth/versola-cli/main/install.sh | sh -s v0.1.0   # pin a version
set -e

REPO="versolauth/versola-cli"
INSTALL_DIR="$HOME/.local/bin"
BIN_NAME="versola"

# 1. Detect OS.
OS="$(uname -s)"
case "$OS" in
  Darwin) OS="darwin" ;;
  Linux) OS="linux" ;;
  *)
    echo "Error: unsupported OS: $OS (this script only supports macOS/Linux -- see install.ps1 for Windows)" >&2
    exit 1
    ;;
esac

# 2. Detect architecture.
ARCH="$(uname -m)"
case "$ARCH" in
  arm64|aarch64) ARCH="arm64" ;;
  x86_64|amd64) ARCH="amd64" ;;
  *)
    echo "Error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

ASSET="versola-${OS}-${ARCH}"

# 3. Resolve version: use $1 if given, otherwise ask GitHub for the latest release.
# Note: GitHub's "latest release" API excludes pre-releases, so this fails to
# find anything until a non-pre-release build is published -- pass a version
# explicitly (e.g. "sh install.sh v0.1.0-beta") until then.
VERSION="$1"
if [ -z "$VERSION" ]; then
  API_RESPONSE="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null || true)"
  VERSION="$(printf '%s' "$API_RESPONSE" | grep '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  if [ -z "$VERSION" ]; then
    echo "Error: could not determine the latest versola-cli version" >&2
    echo "  (no non-pre-release exists yet -- pass a version explicitly, e.g. 'sh install.sh v0.1.0-beta')" >&2
    exit 1
  fi
fi

echo "Installing versola-cli ${VERSION} for ${OS}/${ARCH}..."

BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
# Portable mktemp: GNU mktemp (Linux) accepts "-d" with no template. BSD/macOS
# mktemp requires one and errors ("too few X's in template") without it --
# the "-t" form is its portable way to supply a prefix instead.
TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'versola-install')"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading ${ASSET}..."
if ! curl -fsSL -o "${TMP_DIR}/${ASSET}" "${BASE_URL}/${ASSET}"; then
  echo "Error: could not download ${ASSET} for release ${VERSION}" >&2
  echo "  (check that ${VERSION} exists and publishes a ${ASSET} asset: ${BASE_URL})" >&2
  exit 1
fi

# 4. Verify checksum if this release published one. Older releases (e.g. v0.1.0-beta)
# didn't, so a missing checksums.txt (HTTP 404) is a soft skip, not an error.
# A non-404 failure (network blip, proxy, DNS, etc.) is NOT the same thing --
# it means verification didn't happen, not that it wasn't needed, so that
# case gets a distinct warning instead of silently looking like the former.
HTTP_CODE="$(curl -fsSL -w '%{http_code}' -o "${TMP_DIR}/checksums.txt" "${BASE_URL}/checksums.txt" 2>/dev/null || true)"
if [ "$HTTP_CODE" = "200" ]; then
  echo "Verifying checksum..."
  # Exact filename match via awk, not a grep regex on $ASSET -- $ASSET can
  # contain regex-special characters (the "." in *.exe, though that's a
  # Windows-only asset today), which grep would treat as "match any
  # character" instead of a literal dot. Also strips a leading "*" (sha256sum
  # binary-mode marker) or "./" (relative-path prefix) from the filename
  # column before comparing, so verification isn't silently skipped just
  # because whatever generated checksums.txt used one of those conventions.
  EXPECTED="$(awk -v want="$ASSET" '{ name=$2; sub(/^\*/, "", name); sub(/^\.\//, "", name); if (name == want) { print $1; exit } }' "${TMP_DIR}/checksums.txt")"
  ACTUAL=""
  if [ -z "$EXPECTED" ]; then
    echo "Warning: checksums.txt has no entry for ${ASSET}, skipping verification" >&2
  elif command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
  else
    echo "Warning: neither sha256sum nor shasum is available, skipping checksum verification" >&2
  fi

  if [ -n "$EXPECTED" ] && [ -n "$ACTUAL" ]; then
    if [ "$EXPECTED" != "$ACTUAL" ]; then
      echo "Error: checksum mismatch for ${ASSET}" >&2
      echo "  expected: $EXPECTED" >&2
      echo "  actual:   $ACTUAL" >&2
      exit 1
    fi
    echo "Checksum OK."
  fi
elif [ "$HTTP_CODE" = "404" ]; then
  echo "Note: ${VERSION} did not publish checksums.txt, skipping verification."
elif [ -z "$HTTP_CODE" ] || [ "$HTTP_CODE" = "000" ]; then
  echo "Warning: could not fetch checksums.txt (network error), skipping verification" >&2
else
  echo "Warning: could not fetch checksums.txt (HTTP ${HTTP_CODE}), skipping verification" >&2
fi

# 5. Install.
mkdir -p "$INSTALL_DIR"
mv "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/${BIN_NAME}"
chmod +x "${INSTALL_DIR}/${BIN_NAME}"

echo "Installed to ${INSTALL_DIR}/${BIN_NAME}"

# 6. Make sure it's actually runnable as a bare command. Strip any trailing
# slash before each ":" separator so an existing PATH entry written with a
# trailing slash (e.g. "~/.local/bin/") still matches $INSTALL_DIR (no
# trailing slash) -- otherwise this would false-negative and print
# "not on your PATH" guidance the user doesn't need.
NORMALIZED_PATH="$(printf '%s' ":$PATH:" | sed 's#/*:#:#g')"
case "$NORMALIZED_PATH" in
  *":$INSTALL_DIR:"*)
    echo ""
    echo "Done. Run 'versola doctor' to get started."
    ;;
  *)
    echo ""
    echo "${INSTALL_DIR} is not on your PATH yet."
    echo "Add this line to your shell profile (~/.bashrc, ~/.zshrc, or similar):"
    echo ""
    echo "  export PATH=\"\$PATH:${INSTALL_DIR}\""
    echo ""
    echo "Then restart your terminal and run 'versola doctor'."
    ;;
esac
